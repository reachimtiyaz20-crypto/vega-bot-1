// Package economics is the cost model for VEGA.
//
// This package exists because the previous funding-arb bot did not have one.
// A search of that codebase for "fee", "commission", "taker" and "maker"
// returned a single match: the API parameter string "incomeType=FUNDING_FEE".
// With no cost side, the bot ran a deterministic -25.5 bps per cycle while its
// dashboard displayed a monotonically rising "earned" figure.
//
// Two structural rules are enforced here rather than left to callers:
//
//  1. A zero-value FeeSchedule is INVALID. It does not silently mean "free".
//     Assess refuses to return Viable for it. This makes "forgot the fees"
//     a loud failure instead of a quiet profit.
//
//  2. Nothing in this package clamps at zero. Net figures are signed and are
//     expected to be negative most of the time. A number that cannot go
//     negative is not a measurement.
package economics

import (
	"errors"
	"fmt"
	"math"
)

// IntervalsPerDay is the number of funding settlements per day on Binance
// USDT-M perpetuals: 00:00, 08:00, 16:00 UTC.
//
// Note: this is the standard schedule. Some symbols settle every 4h (6/day)
// during elevated-funding regimes. The monitor must read the actual interval
// from the exchange per symbol rather than trusting this constant; it is the
// default only.
const IntervalsPerDay = 3.0

// DaysPerYear is used for annualising. 365, not 252 -- funding accrues on
// calendar days, not trading days.
const DaysPerYear = 365.0

var (
	// ErrNoFeeModel is returned when a FeeSchedule implies zero cost. This is
	// the exact defect that killed the previous bot, so it is an error rather
	// than a permitted configuration.
	ErrNoFeeModel = errors.New("economics: fee schedule implies zero round-trip cost; refusing to assess")

	// ErrBadHold is returned for a non-positive planned hold.
	ErrBadHold = errors.New("economics: planned hold days must be > 0")
)

// FeeSchedule is the taker fee schedule in basis points.
//
// Defaults below are Binance spot and USDT-M futures at VIP 0 with no BNB
// discount. They are deliberately pessimistic: paper trading that assumes a
// discount you do not have produces a track record you cannot reproduce with
// real money.
//
// Maker fees are not modelled because this strategy is not maker-capable at
// entry. Funding capture requires both legs on at a known delta; waiting for
// a limit fill on one leg leaves the position directional in the gap. Assume
// taker on all four legs and be pleasantly surprised later.
type FeeSchedule struct {
	SpotTakerBps    float64 // Binance spot taker, VIP 0, no BNB discount: 10
	FuturesTakerBps float64 // Binance USDT-M futures taker, VIP 0: 5

	// SlippageBpsPerLeg is an allowance for the gap between the quoted price
	// and the settled price. Project GAMA measured this at -3.95 bps per leg
	// on Solana DEX routes across 30 samples. Centralised venue books on
	// majors are far tighter, so the default here is deliberately small -- but
	// it is NOT zero, because every one of the four failed bots assumed the
	// quote was the fill.
	//
	// This is an estimate, and it is the only estimate in this struct. It must
	// be replaced with a measured value before any capital is committed.
	SlippageBpsPerLeg float64
}

// DefaultFees returns the Binance VIP 0 schedule.
//
//	buy spot      10 bps
//	short futures  5 bps
//	close futures  5 bps
//	sell spot     10 bps
//	--------------------
//	round trip    30 bps
//
// Thirty basis points exceeds three days of funding at typical rates. That
// single fact invalidated the previous bot's entire configuration.
func DefaultFees() FeeSchedule {
	return FeeSchedule{
		SpotTakerBps:      10.0,
		FuturesTakerBps:   5.0,
		SlippageBpsPerLeg: 1.0,
	}
}

// Validate reports whether the schedule can be used for a decision.
func (f FeeSchedule) Validate() error {
	if f.SpotTakerBps < 0 || f.FuturesTakerBps < 0 || f.SlippageBpsPerLeg < 0 {
		return fmt.Errorf("economics: negative fee component in %+v", f)
	}
	if f.RoundTripBps() <= 0 {
		return ErrNoFeeModel
	}
	return nil
}

// RoundTripBps is the full cost of opening AND closing a hedged position:
// buy spot + short futures on entry, sell spot + close futures on exit.
//
// Four legs, all taker, plus a slippage allowance on each. At Binance
// defaults this is 30 bps of fees plus 4 bps of slippage allowance = 34 bps.
// The previous bot computed this number nowhere.
func (f FeeSchedule) RoundTripBps() float64 {
	fees := 2 * (f.SpotTakerBps + f.FuturesTakerBps)
	slip := 4 * f.SlippageBpsPerLeg
	return fees + slip
}

// EntryBps is the cost of getting in only. Useful for reporting the sunk cost
// of a position that is already open, where the exit cost is still ahead.
func (f FeeSchedule) EntryBps() float64 {
	return f.SpotTakerBps + f.FuturesTakerBps + 2*f.SlippageBpsPerLeg
}

// Opportunity is a candidate observed on the exchange.
type Opportunity struct {
	Symbol string

	// FundingRatePct is the rate for ONE settlement interval, expressed as a
	// percentage. Binance returns this as a decimal string: "0.0001" means
	// 0.01% per interval, so the caller must multiply the API value by 100
	// before it lands here. Getting this conversion backwards is a factor-of-
	// 100 error that would make everything look wildly viable.
	FundingRatePct float64

	// NotionalUSD is the size of ONE leg. Capital deployed is roughly twice
	// this, because both legs need funding. See Assessment.CapitalUSD.
	NotionalUSD float64

	// IntervalsPerDayOverride, when non-zero, replaces the default of 3 for
	// symbols on a 4-hour funding schedule.
	IntervalsPerDayOverride float64
}

func (o Opportunity) intervalsPerDay() float64 {
	if o.IntervalsPerDayOverride > 0 {
		return o.IntervalsPerDayOverride
	}
	return IntervalsPerDay
}

// Assessment is the answer. Every field is signed; nothing is clamped.
type Assessment struct {
	Symbol string

	// Inputs echoed back, so a journal line is self-contained and a future
	// reader does not have to guess what the fee model was at the time.
	FundingRatePct float64
	HoldDays       float64
	NotionalUSD    float64

	// AnnualizedPct is the funding rate annualised on NOTIONAL, before costs.
	// At 0.01% per 8h this is 10.95%. This is the headline number retail
	// sources quote. It is not the return on capital -- see NetAnnualOnCapital.
	AnnualizedPct float64

	// BreakEvenDays is how long the position must be held for funding to pay
	// for the round trip. +Inf when the rate is zero or negative.
	BreakEvenDays float64

	IntervalsHeld      float64
	ExpectedFundingBps float64 // revenue over the hold; negative if funding is negative
	CostBps            float64 // full round trip
	NetBps             float64 // ExpectedFundingBps - CostBps; frequently negative

	NetUSD float64 // NetBps applied to NotionalUSD

	// CapitalUSD is what must actually be deployed: roughly 2x notional,
	// because the spot leg is bought outright and the futures leg needs
	// margin. Retail sources omit this, which is how a 20% notional figure
	// gets quoted as a 20% return.
	CapitalUSD float64

	// NetAnnualOnCapital extrapolates the hold-period net to a year and
	// expresses it against CapitalUSD, not notional. This is the only return
	// figure in this package that means anything, and it is roughly HALF the
	// annualised notional figure above.
	//
	// It assumes the current rate persists for a year, which it will not.
	// It is a scaling of one observation, not a forecast.
	NetAnnualOnCapital float64

	Viable bool
	Reason string

	// Gate is a stable machine-readable name for WHY this was refused,
	// empty when viable. Reason is for a human reading one line; Gate is
	// for counting across thousands, which is the only way to see which
	// threshold is actually binding.
	Gate string
}

// Assess answers the only question that matters: over the hold period we are
// actually willing to commit to, does funding exceed the cost of getting in
// and out?
//
// It returns an Assessment with Viable=false and a populated Reason rather
// than an error for ordinary rejections -- a rejected opportunity is still
// data worth journaling. It returns an error only when the inputs are
// structurally unusable, which is a programming fault, not a market condition.
func Assess(o Opportunity, fees FeeSchedule, plannedHoldDays float64) (Assessment, error) {
	if err := fees.Validate(); err != nil {
		return Assessment{Symbol: o.Symbol, Reason: err.Error()}, err
	}
	if plannedHoldDays <= 0 || math.IsNaN(plannedHoldDays) {
		return Assessment{Symbol: o.Symbol, Reason: ErrBadHold.Error()}, ErrBadHold
	}
	if math.IsNaN(o.FundingRatePct) || math.IsInf(o.FundingRatePct, 0) {
		err := fmt.Errorf("economics: %s funding rate is not a finite number", o.Symbol)
		return Assessment{Symbol: o.Symbol, Reason: err.Error()}, err
	}

	ipd := o.intervalsPerDay()
	intervals := ipd * plannedHoldDays
	cost := fees.RoundTripBps()

	// Percent to basis points: 0.01% -> 1 bp.
	revenue := o.FundingRatePct * 100 * intervals
	net := revenue - cost

	a := Assessment{
		Symbol:             o.Symbol,
		FundingRatePct:     o.FundingRatePct,
		HoldDays:           plannedHoldDays,
		NotionalUSD:        o.NotionalUSD,
		AnnualizedPct:      o.FundingRatePct * ipd * DaysPerYear,
		BreakEvenDays:      BreakEvenDays(o.FundingRatePct, fees, ipd),
		IntervalsHeld:      intervals,
		ExpectedFundingBps: revenue,
		CostBps:            cost,
		NetBps:             net,
		NetUSD:             net / 10000 * o.NotionalUSD,
		CapitalUSD:         o.NotionalUSD * 2,
	}

	if a.CapitalUSD > 0 {
		yearsHeld := plannedHoldDays / DaysPerYear
		if yearsHeld > 0 {
			a.NetAnnualOnCapital = (a.NetUSD / a.CapitalUSD) / yearsHeld * 100
		}
	}

	switch {
	case o.FundingRatePct < 0:
		a.Viable = false
		a.Reason = fmt.Sprintf("funding is negative (%.4f%% per interval): the short leg PAYS. Position would bleed %.1f bps over %.0fd on top of %.1f bps costs",
			o.FundingRatePct, -revenue, plannedHoldDays, cost)
	case o.FundingRatePct == 0:
		a.Viable = false
		a.Reason = "funding is zero: no revenue to offset the round trip"
	case net <= 0:
		a.Viable = false
		a.Reason = fmt.Sprintf("net %.1f bps over %.0fd hold: %.1f bps funding does not cover %.1f bps round trip; needs %.1f days to break even",
			net, plannedHoldDays, revenue, cost, a.BreakEvenDays)
	default:
		a.Viable = true
		a.Reason = fmt.Sprintf("net +%.1f bps over %.0fd hold: %.1f bps funding vs %.1f bps round trip; breaks even at day %.1f",
			net, plannedHoldDays, revenue, cost, a.BreakEvenDays)
	}

	return a, nil
}

// BreakEvenDays is how many days a position must be held before accumulated
// funding covers the round trip.
//
//	days = round_trip_bps / (intervals_per_day * rate_pct * 100)
//
// At the default 34 bps round trip and 3 intervals/day:
//
//	0.005% per 8h -> 22.7 days   (the previous bot's threshold; it closed at 3)
//	0.010% per 8h -> 11.3 days
//	0.030% per 8h ->  3.8 days
//	0.100% per 8h ->  1.1 days
//
// Returns +Inf for a non-positive rate: no amount of holding rescues a
// position that is paying rather than receiving.
func BreakEvenDays(ratePct float64, fees FeeSchedule, intervalsPerDay float64) float64 {
	if ratePct <= 0 || intervalsPerDay <= 0 {
		return math.Inf(1)
	}
	return fees.RoundTripBps() / (intervalsPerDay * ratePct * 100)
}

// MinViableRatePct is the rate below which a position cannot pay for itself
// over the given hold. Use this as the entry threshold instead of an arbitrary
// constant.
//
// The previous bot hardcoded MIN_FUNDING_RATE=0.005 alongside
// MAX_POSITION_HOURS=72. Those two constants were never checked against each
// other. This function is the check: at a 3-day hold the minimum viable rate
// is 0.0378%, which is 7.6x what the config permitted.
//
//	min_rate_pct = round_trip_bps / (intervals_per_day * 100 * hold_days)
func MinViableRatePct(fees FeeSchedule, plannedHoldDays float64) float64 {
	if plannedHoldDays <= 0 {
		return math.Inf(1)
	}
	return fees.RoundTripBps() / (IntervalsPerDay * 100 * plannedHoldDays)
}

// MaxHoldForRate is the inverse framing: given a rate, the shortest hold that
// still clears costs. A monitor that wants to answer "can I trade this at all"
// compares this against the longest hold it is willing to commit to.
func MaxHoldForRate(ratePct float64, fees FeeSchedule) float64 {
	return BreakEvenDays(ratePct, fees, IntervalsPerDay)
}

// AnnualizedPct converts a per-interval funding rate to an annualised
// percentage ON NOTIONAL, before any costs.
//
// This number is roughly double the return on deployed capital, because both
// legs must be funded. Anywhere it is shown to a human it must be labelled as
// notional, or it will be read as a return.
func AnnualizedPct(ratePct float64, intervalsPerDay float64) float64 {
	if intervalsPerDay <= 0 {
		intervalsPerDay = IntervalsPerDay
	}
	return ratePct * intervalsPerDay * DaysPerYear
}
