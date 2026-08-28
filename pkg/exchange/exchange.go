// Package exchange defines what a trading venue must provide before VEGA will
// assess anything on it.
//
// Three things must be true before an opportunity can be called viable, and
// each of them is enforced here rather than left to a caller's discipline:
//
//  1. The venue's fee schedule has been VERIFIED against its own published
//     page, with a URL and a date. Six venues means six chances to repeat the
//     defect that killed four previous bots.
//
//  2. The symbol has a SPOT MARKET. Cash-and-carry buys spot and shorts the
//     perp. On 2026-08-05, 435 of Binance's 806 USDT perpetuals had no spot
//     pair -- they are perp-only listings, and the trade is not expensive
//     there, it is impossible. The first scan built against this package
//     ranked six untradeable fictions in its top eight.
//
//  3. Execution cost is MEASURED, not assumed. A single assumed slippage
//     constant was wrong by 125x in one direction on BTCUSDT (real: 0.008 bps)
//     and 6x in the other on BNCUSDT (real: 5.93 bps). Half-spreads come off
//     the live book, per symbol, per leg.
//
// The pattern throughout: a missing input produces a refusal with a stated
// reason, never a silently optimistic default.
package exchange

import (
	"context"
	"fmt"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/economics"
)

// FeeSource records where a fee schedule came from and when it was checked.
// "I remember Bybit is 5.5 bps" is not a source. A URL and a date is.
type FeeSource struct {
	URL        string // the venue's own published fee page
	VerifiedOn string // YYYY-MM-DD, the date a human read that page
	Note       string // VIP tier, discounts assumed, anything that moves the number
}

// Venue is a trading venue and its cost structure.
type Venue struct {
	Name string

	// Fees is the round-trip cost model for a hedged position on this venue.
	// SlippageBpsPerLeg here is a FALLBACK only, used when the live book
	// could not be read. Measured spreads override it per symbol.
	Fees economics.FeeSchedule

	// FeesVerified must only be set true by a human who has read the venue's
	// published fee schedule and recorded it in Source. Defaulting this to
	// false is the entire safety property of this package.
	FeesVerified bool
	Source       FeeSource

	// SpotAvailable reports whether the venue offers spot markets at all.
	// A perp-only venue cannot run cash-and-carry; it can only participate in
	// cross-venue funding spreads, which is a different strategy with
	// different risks.
	SpotAvailable bool

	DefaultFundingIntervalHours float64
	Notes                       string
}

// IntervalsPerDay for this venue's default schedule.
func (v Venue) IntervalsPerDay() float64 {
	if v.DefaultFundingIntervalHours <= 0 {
		return 3
	}
	return 24 / v.DefaultFundingIntervalHours
}

// Validate reports whether this venue may be used for a viability decision.
func (v Venue) Validate() error {
	if v.Name == "" {
		return fmt.Errorf("exchange: venue has no name")
	}
	if !v.FeesVerified {
		return fmt.Errorf("exchange: %s fee schedule is UNVERIFIED; scanning is allowed, entering is not", v.Name)
	}
	if v.Source.URL == "" || v.Source.VerifiedOn == "" {
		return fmt.Errorf("exchange: %s claims verified fees but records no source URL and date", v.Name)
	}
	if err := v.Fees.Validate(); err != nil {
		return fmt.Errorf("exchange: %s: %w", v.Name, err)
	}
	if !v.SpotAvailable {
		return fmt.Errorf("exchange: %s has no spot markets; cash-and-carry is not possible here", v.Name)
	}
	return nil
}

// Observation is one symbol's funding AND liquidity state on one venue at one
// moment. This is the unit that gets journaled.
//
// The liquidity fields are not decoration. They are the difference between a
// screen that ranks opportunity and one that ranks illiquidity.
type Observation struct {
	Venue  string
	Symbol string

	// FundingRatePct is per settlement interval, as a percentage. Every venue
	// client normalises into this unit, so nothing downstream has to ask what
	// unit it is holding.
	FundingRatePct float64

	IntervalHours   float64
	MarkPrice       float64
	IndexPrice      float64
	NextFundingTime time.Time
	ObservedAt      time.Time

	// --- liquidity, measured from the live book ---

	// SpotSymbolAvailable reports whether a spot pair exists and is trading.
	// False makes the position unconstructable, not merely costly.
	SpotSymbolAvailable bool

	// Half-spreads in bps for each leg, from best bid/ask. These are FLOORS
	// on execution cost: they assume the whole order fills at the touch.
	SpotHalfSpreadBps float64
	PerpHalfSpreadBps float64

	// Quote-currency size resting at the touch on each leg. If the intended
	// notional exceeds this, the half-spread understates the real cost.
	SpotTopOfBookUSD float64
	PerpTopOfBookUSD float64

	// Volume on each side. Futures volume alone is not enough: HFTUSDT showed
	// $96m of perp volume against a spot book holding $5 at the touch. The
	// long leg is bought on spot, so spot depth is the binding constraint.
	PerpQuoteVolume24hUSD float64
	SpotQuoteVolume24hUSD float64

	// LiquidityMeasured is false when the book could not be read for this
	// symbol -- futures bookTicker returns 728 entries against 855 symbols,
	// so this genuinely happens. False means "unknown", never "free".
	LiquidityMeasured bool
}

// IntervalsPerDay derived from this observation's own interval.
func (o Observation) IntervalsPerDay() float64 {
	if o.IntervalHours <= 0 {
		return 3
	}
	return 24 / o.IntervalHours
}

// BasisBps is the perp-vs-index spread. Not edge -- a sanity signal. A large
// basis means funding is actively pulling the perp back toward index, which
// is the same force that will collapse the rate you are trying to collect.
func (o Observation) BasisBps() float64 {
	if o.IndexPrice == 0 {
		return 0
	}
	return (o.MarkPrice - o.IndexPrice) / o.IndexPrice * 10000
}

// RoundTripSlippageBps is the measured execution cost across all four legs:
// buy spot, short perp, close perp, sell spot.
func (o Observation) RoundTripSlippageBps() float64 {
	return 2*o.SpotHalfSpreadBps + 2*o.PerpHalfSpreadBps
}

// EffectiveFees returns the venue's fee schedule with assumed slippage
// replaced by measured slippage for this specific symbol.
//
// economics.FeeSchedule applies SlippageBpsPerLeg to four legs, so the
// measured total is divided by four to arrive at the equivalent per-leg
// figure. The two legs are not equally expensive -- spot and perp books
// differ -- but the total, which is what the entry gate consumes, is exact.
func (o Observation) EffectiveFees(base economics.FeeSchedule) economics.FeeSchedule {
	if !o.LiquidityMeasured {
		return base
	}
	base.SlippageBpsPerLeg = o.RoundTripSlippageBps() / 4
	return base
}

// Constraints are the caller's requirements for what counts as enterable.
type Constraints struct {
	NotionalUSD float64
	HoldDays    float64

	// MinQuoteVolume24hUSD is applied to BOTH legs independently. BNCUSDT
	// traded $451k in 24h; a $10k position is 2.2% of that, twice, in and out.
	MinQuoteVolume24hUSD float64

	// MinTopOfBookUSD refuses symbols where resting size at the touch is a
	// fraction of the intended notional. Expressed as a multiple of notional:
	// 0.25 means the touch must hold at least a quarter of the order. Below
	// that, the measured half-spread is not a floor, it is fiction.
	MinTopOfBookFraction float64

	// RequireMeasuredLiquidity refuses symbols whose book could not be read.
	// Leave this true for anything that informs a decision. Set it false only
	// to survey what is out there.
	RequireMeasuredLiquidity bool

	// MaxRoundTripSlippageBps refuses symbols whose MEASURED execution cost
	// across all four legs exceeds this ceiling.
	//
	// This is a better instrument than MinQuoteVolume24hUSD and should
	// eventually replace it. 24h volume can be wash-traded -- it is a number an
	// exchange reports about itself. Top-of-book depth and the half-spread are
	// resting orders you would actually have to cross, and they are much harder
	// to fake.
	//
	// Measured 2026-08-11: BTCUSDT 0.02 bps, BNBUSDT 0.33, ZECUSDT 0.43,
	// LINKUSDT 2.31, SUIUSDT 2.90 -- against MOVEUSDT at 30.96 on an $85k spot
	// book. A ceiling of 8 separates those cleanly.
	//
	// Zero disables the check.
	MaxRoundTripSlippageBps float64

	// Sizing, when set, lets a caller resize these constraints to what a
	// symbol's book can actually hold. See Constraints.ForBook.
	//
	// Zero value is inert: ForBook falls back to NotionalUSD, so constraints
	// built before this existed behave exactly as they did.
	Sizing SizingPolicy
}

// DefaultConstraints is a starting point, not a recommendation. The volume
// floor in particular is a judgement call that should be revisited against
// the paper ledger rather than trusted because it appeared in code.
func DefaultConstraints() Constraints {
	return Constraints{
		NotionalUSD:              10000,
		HoldDays:                 7,
		MinQuoteVolume24hUSD:     50_000_000,
		MinTopOfBookFraction:     0.25,
		RequireMeasuredLiquidity: true,
		MaxRoundTripSlippageBps:  8,
	}
}

// Assess applies the venue's fee schedule and this symbol's measured
// liquidity to produce a viability decision.
//
// Refusals are returned as Assessment{Viable: false} with a stated Reason
// AND a non-nil error. The arithmetic is still filled in so the observation
// can be journaled -- a rejected candidate is data worth keeping, and three
// months of rejections is exactly the evidence that decides whether this
// strategy is worth capital.
func Assess(v Venue, o Observation, c Constraints) (economics.Assessment, error) {
	opp := economics.Opportunity{
		Symbol:                  o.Symbol,
		FundingRatePct:          o.FundingRatePct,
		NotionalUSD:             c.NotionalUSD,
		IntervalsPerDayOverride: o.IntervalsPerDay(),
	}
	fees := o.EffectiveFees(v.Fees)

	refuse := func(gate, format string, args ...any) (economics.Assessment, error) {
		a, _ := economics.Assess(opp, fees, c.HoldDays)
		a.Viable = false
		a.Gate = gate
		err := fmt.Errorf(format, args...)
		a.Reason = "REFUSED: " + err.Error()
		return a, err
	}

	if err := v.Validate(); err != nil {
		return refuse(GateVenueInvalid, "%s", err)
	}
	if !o.SpotSymbolAvailable {
		return refuse(GateNoSpotPair, "%s has no spot pair; the long leg cannot be constructed at any price", o.Symbol)
	}
	if c.MinQuoteVolume24hUSD > 0 {
		if o.PerpQuoteVolume24hUSD < c.MinQuoteVolume24hUSD {
			return refuse(GatePerpVolumeTooLow, "%s perp 24h volume $%.0f is below the $%.0f floor; $%.0f is %.2f%% of the daily tape",
				o.Symbol, o.PerpQuoteVolume24hUSD, c.MinQuoteVolume24hUSD, c.NotionalUSD,
				pctOf(c.NotionalUSD, o.PerpQuoteVolume24hUSD))
		}
		if o.SpotQuoteVolume24hUSD < c.MinQuoteVolume24hUSD {
			return refuse(GateSpotVolumeTooLow, "%s SPOT 24h volume $%.0f is below the $%.0f floor; the long leg is bought on spot, so spot liquidity binds",
				o.Symbol, o.SpotQuoteVolume24hUSD, c.MinQuoteVolume24hUSD)
		}
	}
	if c.RequireMeasuredLiquidity && !o.LiquidityMeasured {
		return refuse(GateLiquidityUnmeasured, "%s book could not be read; execution cost is unknown and must not be assumed", o.Symbol)
	}
	if c.MinTopOfBookFraction > 0 {
		need := c.NotionalUSD * c.MinTopOfBookFraction
		if o.SpotTopOfBookUSD < need {
			return refuse(GateSpotBookTooThin, "%s spot book holds only $%.2f at the touch, against $%.0f required for a $%.0f order; the measured spread is not a floor here",
				o.Symbol, o.SpotTopOfBookUSD, need, c.NotionalUSD)
		}
		if o.PerpTopOfBookUSD < need {
			return refuse(GatePerpBookTooThin, "%s perp book holds only $%.2f at the touch, against $%.0f required for a $%.0f order",
				o.Symbol, o.PerpTopOfBookUSD, need, c.NotionalUSD)
		}
	}

	// Measured execution cost. Only meaningful when the book was actually read
	// -- otherwise RoundTripSlippageBps is the configured fallback, and
	// refusing on a fallback would be refusing on an assumption.
	if c.MaxRoundTripSlippageBps > 0 && o.LiquidityMeasured {
		if slip := o.RoundTripSlippageBps(); slip > c.MaxRoundTripSlippageBps {
			return refuse(GateSlippageTooHigh, "%s measured round-trip slippage of %.2f bps exceeds the %.2f bps ceiling; the book cannot absorb $%.0f cheaply regardless of what the funding rate says",
				o.Symbol, slip, c.MaxRoundTripSlippageBps, c.NotionalUSD)
		}
	}

	a, err := economics.Assess(opp, fees, c.HoldDays)
	if err != nil {
		return a, err
	}
	// Everything structural passed and the arithmetic still says no. The
	// refuse() closure above cannot label this one, because this refusal
	// comes from economics rather than from a gate here.
	if !a.Viable && a.Gate == "" {
		a.Gate = GateExpectedNetNegative
	}

	// Size against depth. This does not refuse -- working an order over time
	// is legitimate -- but the half-spread stops being an honest floor once
	// the order is bigger than the touch, and the reason must say so.
	if a.Viable {
		if thin, leg, depth := o.exceedsTopOfBook(c.NotionalUSD); thin {
			a.Reason += fmt.Sprintf(" | CAUTION: $%.0f exceeds %s top-of-book depth of $%.0f, so measured slippage of %.2f bps is a FLOOR, not the cost",
				c.NotionalUSD, leg, depth, o.RoundTripSlippageBps())
		}
	}
	return a, nil
}

func (o Observation) exceedsTopOfBook(notional float64) (bool, string, float64) {
	if o.SpotTopOfBookUSD > 0 && notional > o.SpotTopOfBookUSD {
		return true, "spot", o.SpotTopOfBookUSD
	}
	if o.PerpTopOfBookUSD > 0 && notional > o.PerpTopOfBookUSD {
		return true, "perp", o.PerpTopOfBookUSD
	}
	return false, "", 0
}

func pctOf(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole * 100
}

// RateSource is everything VEGA needs from a venue.
//
// Note what is NOT here: no PlaceOrder, no Balance, no Withdraw, no
// credentials. A venue client cannot trade because the interface gives it
// nowhere to put a trade. Live execution is a new interface written
// deliberately, not a flag someone flips at 2am.
type RateSource interface {
	Venue() Venue
	FundingRates(ctx context.Context) ([]Observation, error)
}

// Registry holds the venues VEGA knows about.
type Registry struct {
	sources []RateSource
}

// NewRegistry builds a registry from zero or more venue clients.
func NewRegistry(sources ...RateSource) *Registry {
	return &Registry{sources: sources}
}

// Sources returns the registered venue clients.
func (r *Registry) Sources() []RateSource { return r.sources }

// Verified returns only those venues whose fee schedules have been verified.
// Anything that reports on realistic returns must use this, not Sources.
func (r *Registry) Verified() []RateSource {
	out := make([]RateSource, 0, len(r.sources))
	for _, s := range r.sources {
		if s.Venue().Validate() == nil {
			out = append(out, s)
		}
	}
	return out
}

// CollectAll polls every registered venue and returns all observations plus
// any per-venue errors. One venue being down must never stop the others --
// funding settles on a fixed clock and a missed window is a permanently
// missing data point.
func (r *Registry) CollectAll(ctx context.Context) ([]Observation, map[string]error) {
	var all []Observation
	errs := make(map[string]error)

	for _, s := range r.sources {
		name := s.Venue().Name
		obs, err := s.FundingRates(ctx)
		if err != nil {
			errs[name] = err
			continue
		}
		all = append(all, obs...)
	}
	return all, errs
}
