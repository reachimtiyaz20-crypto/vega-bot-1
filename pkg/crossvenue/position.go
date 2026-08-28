// Package crossvenue models perp-perp funding positions across two venues.
//
// HOW THIS DIFFERS FROM pkg/funding
//
// pkg/funding models cash-and-carry: long SPOT, short PERP, ONE venue. Two
// legs, one funding stream, one settlement clock.
//
// Here there is no spot leg. We are LONG the perp on the venue whose funding is
// lower (often negative, where longs are paid) and SHORT the perp on the venue
// whose funding is higher (where shorts are paid). Two legs, TWO funding
// streams, and -- the part that breaks naive implementations -- TWO DIFFERENT
// SETTLEMENT CLOCKS.
//
// # THE TWO-CLOCK PROBLEM
//
// Hyperliquid settles every hour. Binance and Bybit settle every eight. A
// position long Hyperliquid and short Binance receives 24 settlements a day on
// one leg and 3 on the other. Accruing both at the same cadence is an 8x error
// on whichever leg you got wrong, and it is an error that flatters the result
// roughly half the time, which is what makes it dangerous.
//
// So each leg carries its own interval and accrues independently. Every rate
// entering this package is in BPS PER HOUR, converted at the boundary, because
// the one thing worse than two clocks is two units.
//
// SIGN CONVENTION, STATED ONCE
//
//	A funding rate is paid BY LONGS TO SHORTS when positive.
//
//	Long leg  (we are long):   contribution = -rate
//	Short leg (we are short):  contribution = +rate
//
// Because the long venue is by construction the lower-paying one, the two
// contributions sum to the SPREAD, and the spread is positive by construction.
// A negative net therefore means the spread has collapsed or inverted since
// entry -- which is the single thing this position can be wrong about, and the
// thing the exit rule watches for.
//
// WHAT THIS PACKAGE DOES NOT MODEL
//
//   - Price divergence between the two venues. Both legs are the same size in
//     base units so the delta nets out, but the MARGIN does not: a rally that
//     profits the long leg on venue A does not automatically post to venue B,
//     where the short leg is being liquidated. That is a real, unpriced risk.
//   - The DEX custody risk on Hyperliquid. There is no support desk.
//   - Capital that cannot be rebalanced between venues instantly.
package crossvenue

import (
	"fmt"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/funding"
)

// PlanCheck is one comparison of the forecast against what actually happened.
//
// # WHY THIS EXISTS
//
// The entry gate refuses trades whose simulated path breaches the stop loss.
// That gate is only worth having if the simulation is right, and nothing so far
// tests that against reality -- the tests replay two losses the model was built
// from, which is not the same as predicting one it has not seen.
//
// So the forecast is frozen at entry and checked at every settlement. If
// predicted and actual diverge, the model is wrong and one position says so.
type PlanCheck struct {
	AtHours float64 `json:"at_hours"`
	Legs    string  `json:"legs"`

	PredictedFundingBps float64 `json:"predicted_funding_bps"`
	ActualFundingBps    float64 `json:"actual_funding_bps"`
	FundingErrorBps     float64 `json:"funding_error_bps"`

	PredictedNetBps float64 `json:"predicted_net_bps"`
	ActualNetBps    float64 `json:"actual_net_bps"`

	// CostDriftBps is how much the round trip moved since entry. It is the
	// OTHER reason net can miss the forecast, and separating it stops a
	// worsening exit from being read as funding that failed to arrive.
	CostDriftBps float64 `json:"cost_drift_bps"`
}

// Position is one open or closed cross-venue funding position.
//
// It embeds funding.ExitWatch so that the exit cost is MEASURED over the life
// of the position rather than assumed symmetric with entry. In pkg/funding that
// watch was built and left unwired; here it is wired from the start, because a
// cross-venue exit crosses four legs on two venues and is far more likely to
// deteriorate than a single-venue one.
type Position struct {
	Coin       string `json:"coin"`
	LongVenue  string `json:"long_venue"`  // lower funding: we are LONG here
	ShortVenue string `json:"short_venue"` // higher funding: we are SHORT here

	OpenedAt    time.Time `json:"opened_at"`
	ClosedAt    time.Time `json:"closed_at,omitempty"`
	CloseReason string    `json:"close_reason,omitempty"`

	// NotionalUSD is PER LEG. CapitalUSD is what must actually sit on the two
	// venues combined -- roughly twice the notional, and it cannot be moved
	// between them quickly. Reporting return on notional instead of capital
	// doubles the headline figure, so both are carried explicitly.
	NotionalUSD float64 `json:"notional_usd"`
	CapitalUSD  float64 `json:"capital_usd"`

	// Settlement intervals, per leg, in hours. Recorded at entry so a venue
	// changing its schedule later cannot silently rewrite history.
	LongIntervalHours  float64 `json:"long_interval_hours"`
	ShortIntervalHours float64 `json:"short_interval_hours"`

	// Rates at entry, bps per hour.
	EntryLongBpsHr   float64 `json:"entry_long_bps_hr"`
	EntryShortBpsHr  float64 `json:"entry_short_bps_hr"`
	EntrySpreadBpsHr float64 `json:"entry_spread_bps_hr"`

	// Most recent observed rates, bps per hour.
	LastLongBpsHr  float64   `json:"last_long_bps_hr"`
	LastShortBpsHr float64   `json:"last_short_bps_hr"`
	LastObservedAt time.Time `json:"last_observed_at,omitempty"`

	// EntryCostBps is fees plus MEASURED slippage across both opening legs.
	// EntryCostMeasured is false if either book could not be read -- in which
	// case no position should have been opened at all, and this flag exists so
	// that a book written by an older or buggier version cannot pass as
	// measured.
	EntryCostBps      float64 `json:"entry_cost_bps"`
	EntryCostMeasured bool    `json:"entry_cost_measured"`

	// ExitCostBps is only real once closed. While open, the honest figure comes
	// from ExitWatch, not from this field.
	ExitCostBps      float64 `json:"exit_cost_bps,omitempty"`
	ExitCostMeasured bool    `json:"exit_cost_measured,omitempty"`

	// --- accrual, per leg, kept separate so a wrong clock is visible ---
	LongLegBps  float64 `json:"long_leg_bps"`  // may be negative
	ShortLegBps float64 `json:"short_leg_bps"` // may be negative

	LongSettlements  int `json:"long_settlements"`
	ShortSettlements int `json:"short_settlements"`

	LastLongSettleAt  time.Time `json:"last_long_settle_at,omitempty"`
	LastShortSettleAt time.Time `json:"last_short_settle_at,omitempty"`

	// Next settlement timestamps as each VENUE reports them.
	//
	// Settlement is detected by watching these ADVANCE, not by reimplementing
	// each venue's schedule. Hyperliquid settles hourly on the hour, Binance
	// and Bybit at 00/08/16 UTC -- but schedules change, and a hardcoded one
	// that drifts would silently stop accruing while the position looked fine.
	// The venue is the authority on its own clock.
	LongNextFundingMs  int64 `json:"long_next_funding_ms,omitempty"`
	ShortNextFundingMs int64 `json:"short_next_funding_ms,omitempty"`

	// NegativeSettlements counts CONSECUTIVE settlements taken while the live
	// spread was zero or inverted. It resets the moment the spread recovers,
	// so it measures a regime, not a bad afternoon.
	NegativeSettlements int `json:"negative_settlements"`

	// MissedSettlements counts intervals that elapsed between two polls and
	// had to be booked together.
	//
	// Measured 2026-08-13: a position held 23.03 hours had booked 19 hourly
	// Hyperliquid settlements instead of 23. The detector fired once per poll
	// however many intervals had passed, and the four it dropped were all on
	// the leg that PAYS -- so the position read about 38 bps richer than it
	// was. A gap that silently improves the result is the worst kind.
	MissedSettlements int `json:"missed_settlements,omitempty"`

	// EntryBasisBps and LastBasisBps track the PRICE gap between the two
	// venues. Their difference is the position's entire price P&L:
	//
	//	price result = LastBasisBps - EntryBasisBps
	//
	// Long the cheaper leg and short the dearer one, convergence pays. But
	// nothing guarantees convergence, and a 136 bps gap is hours of funding.
	// This is measured and reported, NOT yet gated -- see Config.MaxEntryBasisBps.
	EntryBasisBps float64 `json:"entry_basis_bps,omitempty"`
	LastBasisBps  float64 `json:"last_basis_bps,omitempty"`
	BasisMeasured bool    `json:"basis_measured,omitempty"`

	// PeakNetBps is the best this position ever showed. Kept because a
	// position that earned 40 bps and gave it all back is a different failure
	// from one that never earned anything, and the two are indistinguishable
	// from the final number alone.
	PeakNetBps float64 `json:"peak_net_bps"`

	// PlanPath is the forecast made at entry. PlanCostBps is the round trip
	// that forecast assumed, kept so cost drift can be separated from funding
	// error.
	PlanPath    []PlanPoint `json:"plan_path,omitempty"`
	PlanCostBps float64     `json:"plan_cost_bps,omitempty"`
	PlanChecks  []PlanCheck `json:"plan_checks,omitempty"`

	funding.ExitWatch
}

// Key identifies a position. Direction is part of it: a pair that inverts is a
// DIFFERENT trade, not the same one continuing.
// pairKeyOf identifies a position by coin and UNORDERED venue pair.
//
// IDENTITY MUST NOT DEPEND ON DIRECTION.
//
// Keys were coin|long|short until 2026-08-19. Candidates are emitted with the
// profitable leg long, so when a spread crossed zero the feed republished the
// same pair under reversed labels and the position's key stopped matching. It
// then froze -- no settlements, no exit evaluation, for up to max-hold of
// thirty days. An archived book from 16 August held a KAITO position opened on
// the 12th and last observed on the 13th.
//
// Direction is a property of the position, not of what it IS. Two independent
// code reviews reached the same conclusion.
func pairKeyOf(coin, a, b string) string {
	if a > b {
		a, b = b, a
	}
	return coin + "|" + a + "|" + b
}

func (p Position) Key() string {
	return pairKeyOf(p.Coin, p.LongVenue, p.ShortVenue)
}

// Pair is the human-readable leg description.
func (p Position) Pair() string {
	return fmt.Sprintf("%s  long %s / short %s", p.Coin, p.LongVenue, p.ShortVenue)
}

// Closed reports whether this position has been closed.
func (p Position) Closed() bool { return !p.ClosedAt.IsZero() }

// HeldHours is wall clock, not settlement count. Break-even is a DURATION, and
// judging it against a count of samples is the bug that turned KAITO green
// after 76 minutes against a 2.1 hour break-even.
func (p Position) HeldHours(now time.Time) float64 {
	end := now
	if p.Closed() {
		end = p.ClosedAt
	}
	d := end.Sub(p.OpenedAt).Hours()
	if d < 0 {
		return 0
	}
	return d
}

// HeldDays is HeldHours in days.
func (p Position) HeldDays(now time.Time) float64 { return p.HeldHours(now) / 24 }

// FundingCollectedBps is the net of both legs.
func (p Position) FundingCollectedBps() float64 { return p.LongLegBps + p.ShortLegBps }

// FundingCollectedUSD values the accrual at the position's notional.
func (p Position) FundingCollectedUSD() float64 {
	return p.FundingCollectedBps() / 10000 * p.NotionalUSD
}

// EffectiveExitBps is what closing would cost RIGHT NOW.
//
// The measured watch wins whenever it exists. The fallback is the entry cost --
// the same symmetry assumption pkg/funding makes -- and it is only ever used
// before the first exit measurement lands.
//
// This is the fix for the blind stop-loss: a rule reading a stale entry-cost
// estimate cannot see an exit that has tripled.
func (p Position) EffectiveExitBps() float64 {
	if p.Closed() && p.ExitCostMeasured {
		return p.ExitCostBps
	}
	return p.ExitWatch.EffectiveExitBps(p.EntryCostBps)
}

// RoundTripBps is the full cost of having been in this position.
func (p Position) RoundTripBps() float64 { return p.EntryCostBps + p.EffectiveExitBps() }

// NetBps is the honest result: what was collected, less what it cost to get in
// and what it currently costs to get out.
func (p Position) NetBps() float64 { return p.FundingCollectedBps() - p.RoundTripBps() }

// NetUSD is NetBps on the notional.
func (p Position) NetUSD() float64 { return p.NetBps() / 10000 * p.NotionalUSD }

// AllInNetBps is NetBps plus price P&L.
//
// While a position is OPEN these must stay apart, for the reason given on
// BasisDriftBps: price P&L is unrealised and reverses, and a paper gain must
// never mask a funding loss. Once a position CLOSES both are banked, and
// reporting only the funding half omits a realised term.
//
// ONG on 2026-08-21 collected +191.5 bps of funding against -392.8 bps of
// basis convergence. It was reported as a $+7.02 profit. It lost $8.05.
func (p Position) AllInNetBps() float64 {
	d, ok := p.BasisDriftBps()
	if !ok {
		return p.NetBps()
	}
	return p.NetBps() + d
}

// AllInNetUSD is AllInNetBps in dollars on the notional.
func (p Position) AllInNetUSD() float64 { return p.AllInNetBps() / 10000 * p.NotionalUSD }

// ReturnPct is on DEPLOYED CAPITAL, which is roughly twice the notional.
//
// This is deliberately the only percentage this type exposes. Return on
// notional is the same trade reported at double, and every previous attempt in
// this project quoted the flattering one.
func (p Position) ReturnPct() float64 {
	if p.CapitalUSD <= 0 {
		return 0
	}
	return p.NetUSD() / p.CapitalUSD * 100
}

// BasisDriftBps is the price P&L: how far the venues' disagreement has moved
// since entry. Positive is in our favour -- we are long the leg that has
// gained relative to the other.
//
// Deliberately NOT folded into NetBps. Funding P&L is settled and banked;
// price P&L is unrealised and reverses. Adding them would produce a single
// number in which a real funding loss could be hidden by a paper price gain,
// which is precisely the failure mode of the four bots before this one.
func (p Position) BasisDriftBps() (float64, bool) {
	if !p.BasisMeasured {
		return 0, false
	}
	return p.LastBasisBps - p.EntryBasisBps, true
}

// CurrentSpreadBpsHr is the live spread. It may be negative or zero, which is
// the whole point of tracking it.
func (p Position) CurrentSpreadBpsHr() float64 { return p.LastShortBpsHr - p.LastLongBpsHr }

// FundingPerDayBps is what the position earns per day at the CURRENT spread.
//
// The spread is already per-hour on both legs, so a day is simply 24 of them.
// No interval appears here: intervals govern WHEN money moves, not how much
// accrues per unit time.
func (p Position) FundingPerDayBps() float64 { return p.CurrentSpreadBpsHr() * 24 }

// BreakEvenHours is how long the CURRENT spread needs to run to repay the full
// round trip. Returns ok=false when the spread cannot repay it at all.
func (p Position) BreakEvenHours() (hours float64, ok bool) {
	s := p.CurrentSpreadBpsHr()
	if s <= 0 {
		return 0, false
	}
	return p.RoundTripBps() / s, true
}

// PastBreakEven reports whether the position has been held long enough, in wall
// clock, to have covered its round trip at the spread actually observed.
func (p Position) PastBreakEven(now time.Time) bool {
	return p.FundingCollectedBps() >= p.RoundTripBps()
}

// ObserveRates records the latest funding on both venues.
//
// Both arguments are BPS PER HOUR. If a caller ever passes a raw per-interval
// rate here, an 8-hourly venue reads 8x too high and the position looks
// profitable while it bleeds.
func (p *Position) ObserveRates(longBpsHr, shortBpsHr float64, at time.Time) {
	p.LastLongBpsHr = longBpsHr
	p.LastShortBpsHr = shortBpsHr
	p.LastObservedAt = at

	if n := p.NetBps(); n > p.PeakNetBps {
		p.PeakNetBps = n
	}
}

// ObserveExitCost feeds a freshly measured cost of closing into the watch.
//
// The value is the FULL round trip out: both legs, fees plus measured slippage,
// on both venues.
func (p *Position) ObserveExitCost(bps float64, at time.Time) {
	p.ExitWatch.Observe(bps, at)
}

// SettleLong records a funding settlement on the long leg.
//
// rateBpsPerHour is the venue's rate normalised to an hour. It is multiplied by
// THIS LEG's interval to get the amount actually moved, so an hourly venue
// moves one hour's worth and an 8-hourly venue moves eight.
//
// We are LONG, so a positive rate is money OUT.
func (p *Position) SettleLong(rateBpsPerHour float64, at time.Time) {
	bps := -rateBpsPerHour * p.LongIntervalHours
	p.LongLegBps += bps
	p.LongSettlements++
	p.LastLongSettleAt = at
	p.recordSpreadRegime()
}

// SettleShort records a funding settlement on the short leg.
//
// We are SHORT, so a positive rate is money IN.
func (p *Position) SettleShort(rateBpsPerHour float64, at time.Time) {
	bps := rateBpsPerHour * p.ShortIntervalHours
	p.ShortLegBps += bps
	p.ShortSettlements++
	p.LastShortSettleAt = at
	p.recordSpreadRegime()
}

// recordSpreadRegime updates the consecutive-negative counter at settlement
// time.
//
// Counted at SETTLEMENT rather than at every poll on purpose. Polls are five
// minutes apart, so a poll-based counter would reach any threshold within the
// hour on ordinary noise and turn into the rotation-churn bug that cost the
// cash-and-carry book 186 bps.
func (p *Position) recordSpreadRegime() {
	if p.CurrentSpreadBpsHr() <= 0 {
		p.NegativeSettlements++
		return
	}
	p.NegativeSettlements = 0
}

// CheckAgainstPlan compares the position with the forecast made at entry.
//
// Records at most ONE check per call, against the most recent settlement the
// position has actually passed. Recording several at once would stamp the same
// instantaneous net onto multiple forecast points and manufacture agreement.
func (p *Position) CheckAgainstPlan(now time.Time) {
	if len(p.PlanPath) == 0 {
		return
	}
	held := p.HeldHours(now)

	last := -1.0
	if n := len(p.PlanChecks); n > 0 {
		last = p.PlanChecks[n-1].AtHours
	}

	idx := -1
	for i := range p.PlanPath {
		at := p.PlanPath[i].AtHours
		if at <= last {
			continue
		}
		// Small tolerance: a settlement detected on a 5-minute poll lands
		// slightly after the instant it was forecast for.
		if at > held+0.1 {
			break
		}
		idx = i
	}
	if idx < 0 {
		return
	}

	pt := p.PlanPath[idx]
	actualFunding := p.FundingCollectedBps()

	p.PlanChecks = append(p.PlanChecks, PlanCheck{
		AtHours:             pt.AtHours,
		Legs:                pt.Legs,
		PredictedFundingBps: pt.CollectedBps,
		ActualFundingBps:    actualFunding,
		FundingErrorBps:     actualFunding - pt.CollectedBps,
		PredictedNetBps:     pt.NetBps,
		ActualNetBps:        p.NetBps(),
		CostDriftBps:        p.RoundTripBps() - p.PlanCostBps,
	})
}

// PlanAccuracy summarises how well the forecast has held.
//
// worstFundingErr is the largest absolute miss on FUNDING alone -- the part the
// model is actually forecasting. Cost drift is reported separately because it
// is a market change, not a modelling error.
func (p Position) PlanAccuracy() (worstFundingErr, meanFundingErr, costDrift float64, n int, ok bool) {
	if len(p.PlanChecks) == 0 {
		return 0, 0, 0, 0, false
	}
	var sum float64
	for _, c := range p.PlanChecks {
		e := c.FundingErrorBps
		if e < 0 {
			e = -e
		}
		if e > worstFundingErr {
			worstFundingErr = e
		}
		sum += c.FundingErrorBps
	}
	n = len(p.PlanChecks)
	return worstFundingErr, sum / float64(n), p.PlanChecks[n-1].CostDriftBps, n, true
}

// DescribePlan renders the forecast-vs-actual state for a log line.
func (p Position) DescribePlan() string {
	worst, mean, drift, n, ok := p.PlanAccuracy()
	if !ok {
		if len(p.PlanPath) == 0 {
			return "no forecast recorded"
		}
		return fmt.Sprintf("forecast recorded (%d points), no settlement reached yet", len(p.PlanPath))
	}
	return fmt.Sprintf(
		"forecast vs actual over %d settlements: funding error worst %+.2f bps, mean %+.2f bps; "+
			"round trip has moved %+.2f bps since entry",
		n, worst, mean, drift)
}

// Close finalises the position with a MEASURED exit cost.
//
// exitCostBps must be the real measured cost of the closing legs. Passing an
// estimate here would make the closed record indistinguishable from a measured
// one, so measured is recorded explicitly alongside it.
func (p *Position) Close(reason string, exitCostBps float64, measured bool, at time.Time) {
	p.ClosedAt = at
	p.CloseReason = reason
	p.ExitCostBps = exitCostBps
	p.ExitCostMeasured = measured
}

// Describe renders the position for a log line.
func (p Position) Describe(now time.Time) string {
	// A position whose collected funding already covers the round trip HAS
	// broken even. Showing roundTrip/currentSpread there answers "how long if
	// I entered now", in a field that reads like "how long until this is
	// whole" -- the same continuous-model confusion that cost $7.49 today.
	be := "unreachable"
	switch {
	case p.PastBreakEven(time.Now().UTC()):
		be = "REPAID"
	default:
		if h, ok := p.BreakEvenHours(); ok {
			be = fmt.Sprintf("%.1fh at the current spread", h)
		}
	}
	return fmt.Sprintf(
		"%s  $%.0f/leg  held %.2fh  spread %+.3f bps/hr (entry %+.3f)  "+
			"funding %+.2f bps (L %+.2f in %d, S %+.2f in %d)  cost %.2f  net %+.2f bps ($%+.2f)  be %s",
		p.Pair(), p.NotionalUSD, p.HeldHours(now),
		p.CurrentSpreadBpsHr(), p.EntrySpreadBpsHr,
		p.FundingCollectedBps(), p.LongLegBps, p.LongSettlements,
		p.ShortLegBps, p.ShortSettlements,
		p.RoundTripBps(), p.NetBps(), p.NetUSD(), be)
}
