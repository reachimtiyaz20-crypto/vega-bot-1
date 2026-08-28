package crossvenue

import (
	"encoding/json"
	"fmt"
	"github.com/imtiyaz/vega-bot/pkg/capital"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// THE CROSS-VENUE PAPER BOOK
//
// Same discipline as pkg/funding, one venue further out:
//
//   - Nothing is assumed. An unmeasured book is a REFUSAL, never a default.
//   - Refusals are structural and carry a reason, so "0 positions" can always
//     be explained rather than guessed at.
//   - Money is recomputed from the positions on every pass. Nothing is cached.
//   - The exit rule holds while underwater. Closing a losing position early
//     converts a paper loss into a paid round trip, which is precisely how the
//     cash-and-carry book lost 186 bps over four days.
//
// AND ONE RULE THAT IS NEW HERE
//
// A cross-venue exit crosses four legs on two venues. It is far more fragile
// than a single-venue one, so ExitWatch is wired from the start: the cost of
// leaving is measured every pass, and a position whose door is closing faster
// than it earns is closed even while profitable.

// --- refusal codes ------------------------------------------------------------

const (
	GateOK                   = ""
	GateNoSettledHistory     = "NO_SETTLED_HISTORY"
	GateSettledTooSmall      = "SETTLED_SPREAD_TOO_SMALL"
	GateSettledInconsistent  = "SETTLED_SPREAD_INCONSISTENT"
	GateSettledReversed      = "SETTLED_SPREAD_REVERSED"
	GateSettledStale         = "SETTLED_SPREAD_ALREADY_OVER"
	GateSignalsDisagree      = "SIGNALS_DISAGREE"
	GateCostUnmeasured       = "COST_UNMEASURED"
	GateExpectedNetNegative  = "EXPECTED_NET_NEGATIVE"
	GateFeesUnverified       = "FEES_UNVERIFIED"
	GateUnknownInterval      = "UNKNOWN_SETTLEMENT_INTERVAL"
	GateBookUnmeasured       = "BOOK_UNMEASURED"
	GateBookTooThin          = "BOOK_TOO_THIN"
	GateBelowMinNotional     = "BELOW_EXCHANGE_MINIMUM"
	GateSpreadTooSmall       = "SPREAD_TOO_SMALL"
	GateSpreadInverted       = "SPREAD_INVERTED"
	GateEntryCostTooHigh     = "ENTRY_COST_TOO_HIGH"
	GateCannotRepay          = "CANNOT_REPAY_WITHIN_MAX_HOLD"
	GateVolumeTooLow         = "VOLUME_TOO_LOW"
	GateAlreadyHeld          = "ALREADY_HELD"
	GateCooldown             = "REENTRY_COOLDOWN"
	GateAtCapacity           = "AT_CAPACITY"
	GateSameVenue            = "SAME_VENUE_BOTH_LEGS"
	GateBasisTooWide         = "PRICE_BASIS_TOO_WIDE"
	GateStopsOutBeforeProfit = "STOPS_OUT_BEFORE_PROFIT"
	GateNoSchedule           = "SETTLEMENT_SCHEDULE_UNKNOWN"
	GateSpreadUnmeasured     = "SPREAD_ABOVE_MEASURED_RANGE"
	GateStoppedOnArrival     = "STOPPED_ON_ARRIVAL"
	GateSettlesAfterHold     = "SETTLES_AFTER_HOLD"
)

// --- config -------------------------------------------------------------------

// Config governs entry and exit. Every zero value is replaced by a documented
// default in withDefaults -- a zero must never read as "filter disabled", which
// was the single worst bug in the cash-and-carry book.
type Config struct {
	NotionalUSD   float64 `json:"notional_usd"`
	MaxConcurrent int     `json:"max_concurrent"`

	// --- sizing ---
	Adaptive      bool    `json:"adaptive"`
	DepthFraction float64 `json:"depth_fraction"`

	// MinNotionalUSD is a PREFERENCE: the smallest position worth running.
	//
	// It was set to 200 and commented "exchange minimum, not preference".
	// That was wrong -- Binance's futures minimum is about $5 and Lighter's
	// is $10. The 200 was mine, wearing somebody else's authority, and it
	// refused 48 positions the book had found and wanted.
	//
	// The real exchange rules live in MinNotionalByVenue. This is the separate
	// question of how small a position is worth the operational risk, and it
	// is answered here as a judgement rather than smuggled in as a fact.
	MinNotionalUSD float64 `json:"min_notional_usd"`

	// MinNotionalByVenue is each venue's ACTUAL minimum order value.
	//
	// A pair must clear BOTH legs' minimums, so the binding figure is the
	// larger of the two -- a position the cheap venue would accept is still
	// impossible if the other side rejects it.
	MinNotionalByVenue map[string]float64 `json:"min_notional_by_venue"`

	// --- entry gate ---
	MinSpreadBpsHr    float64 `json:"min_spread_bps_hr"`
	MaxEntryCostBps   float64 `json:"max_entry_cost_bps"`
	MaxBreakEvenHours float64 `json:"max_break_even_hours"`
	MinVolUSD         float64 `json:"min_vol_usd"`

	// MaxEntryBasisBps refuses a pair whose two venues disagree on price by
	// more than this.
	//
	// DEFAULTS TO ZERO, WHICH MEANS OFF -- deliberately. Nobody yet knows the
	// distribution of cross-venue basis, and picking a threshold from one
	// observation is exactly the guessing this project refuses everywhere
	// else. Every candidate's basis is measured and logged from today; set
	// this once the data says what it should be.
	MaxEntryBasisBps float64 `json:"max_entry_basis_bps"`

	// --- exit ---
	MinHoldHours                  float64 `json:"min_hold_hours"`
	MaxHoldHours                  float64 `json:"max_hold_hours"`
	NegativeSettlementsBeforeExit int     `json:"negative_settlements_before_exit"`
	StopLossBps                   float64 `json:"stop_loss_bps"`

	// BasisStopBps closes a position when price divergence between the two
	// venues moves this far against it, whatever the funding is doing.
	//
	// This is the ONLY exit that looks at basis. Every other rule below measures
	// NetBps, which excludes price P&L by design so that a paper gain cannot mask
	// a funding loss. That design is right, and it is also why ONG ran to the
	// end: it showed +175 bps of funding net while basis took 393 bps out from
	// under it, and closed at -217 all-in.
	//
	// Measured across 24 closed positions carrying basis, drift against the
	// position:
	//
	//	worst survivor   UNITREE   -63 bps, finished  +39 all-in
	//	best casualty    COTI     -256 bps, finished -324 all-in
	//	                 ONG      -393 bps, finished -217 all-in
	//
	// Only 2 of 24 breached -100 and both were catastrophic. -100 separates them
	// with margin either side. Deliberately wider than the -60 net stop because
	// basis is unrealised and reverses: this fires on divergence that has stopped
	// looking like noise, not on a wobble.
	BasisStopBps            float64 `json:"basis_stop_bps"`
	ExitDriftMinConsecutive int     `json:"exit_drift_min_consecutive"`

	ReenterCooldown time.Duration `json:"reenter_cooldown"`

	// TakerBps per venue. A venue absent from this map is refused rather than
	// charged a guessed fee.
	TakerBps map[string]float64 `json:"taker_bps"`

	// FeesVerified must be asserted by whoever read the fee pages. False makes
	// every entry refuse. Unverified fees on a trade whose entire edge is a few
	// bps per hour is not a trade, it is a hope.
	FeesVerified bool `json:"fees_verified"`
}

// DefaultConfig is deliberately strict.
//
// MaxBreakEvenHours of 24 is the important one. A pair needing 200 hours to
// repay its round trip is not an opportunity, and 44 of the 46 pairs measured
// on 2026-08-11 were exactly that. Holding all 46 for one day lost $720.
func DefaultConfig(notionalUSD float64) Config {
	if notionalUSD <= 0 {
		notionalUSD = 400
	}
	return Config{
		NotionalUSD:   notionalUSD,
		MaxConcurrent: 5,

		Adaptive:      true,
		DepthFraction: 0.5,

		// 50, down from 200. A $150 position at 1 bps/hr over a day earns
		// about 36 cents -- small, but real, and 48 of those were refused.
		// Below roughly $50 the arithmetic stops being worth the operational
		// risk, and that is a preference, stated as one.
		MinNotionalUSD: 50,

		// Read from each venue, not assumed:
		//   lighter      min_quote_amount "10.000000", every one of 210 markets
		//   hyperliquid  $10 documented minimum order value
		//   binance      ~5 USDT MIN_NOTIONAL on USD-M futures
		//   bybit        ~5 USDT
		//   okx          varies with ctVal; 20 is a conservative stand-in and
		//                should be read per instrument before it is trusted
		MinNotionalByVenue: map[string]float64{
			"lighter":     10,
			"hyperliquid": 10,
			"binance":     5,
			"bybit":       5,
			"okx":         20,
			"bitget":      5,
		},

		MinSpreadBpsHr:    1.0,
		MaxEntryCostBps:   40,
		MaxBreakEvenHours: 24,
		MinVolUSD:         10_000_000,

		MinHoldHours:                  4,
		MaxHoldHours:                  720,
		NegativeSettlementsBeforeExit: 3,
		StopLossBps:                   -60,
		BasisStopBps:                  -100,
		ExitDriftMinConsecutive:       3,

		ReenterCooldown: 6 * time.Hour,

		TakerBps: map[string]float64{
			"hyperliquid": 4.5,
			"binance":     5.0,
			"bybit":       5.5,
			// OKX standard taker on USDT swaps. Read off their public fee
			// page, NOT from an API, so it carries the same status as the
			// other three: asserted by a person via -fees-verified, and
			// refused outright without it.
			// 25.0, NOT 5.0. Read off okx.com/en-ae/fees on 2026-08-15:
			// Futures, Regular user, taker 0.2500%. The 0.05% figure that was
			// here from memory is the VIP 9 rate, which needs 1.8 BILLION AED
			// in assets. Five times too low, on a strategy whose entire edge is
			// a few bps an hour.
			"okx": 25.0,
			// ZERO, and the only fee here that comes from an API rather than a
			// person reading a page. All 210 markets report taker_fee "0.0000"
			// with is_taker_fee_enabled true, and the docs say funding is
			// "fully peer-to-peer with no fees taken by the exchange".
			// Read 2026-08-15.
			//
			// Zero fee is NOT zero cost. Lighter's bid-ask ran 5.4 to 17.5 bps
			// round trip at $400 on the markets that had funding spreads. That
			// cost is measured by the book sweep, not declared here.
			"lighter": 0.0,
		},
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig(c.NotionalUSD)
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = d.MaxConcurrent
	}
	if c.DepthFraction <= 0 {
		c.DepthFraction = d.DepthFraction
	}
	if c.MinNotionalUSD <= 0 {
		c.MinNotionalUSD = d.MinNotionalUSD
	}
	if len(c.MinNotionalByVenue) == 0 {
		c.MinNotionalByVenue = d.MinNotionalByVenue
	}
	if c.MinSpreadBpsHr <= 0 {
		c.MinSpreadBpsHr = d.MinSpreadBpsHr
	}
	if c.MaxEntryCostBps <= 0 {
		c.MaxEntryCostBps = d.MaxEntryCostBps
	}
	if c.MaxBreakEvenHours <= 0 {
		c.MaxBreakEvenHours = d.MaxBreakEvenHours
	}
	if c.MinVolUSD <= 0 {
		c.MinVolUSD = d.MinVolUSD
	}
	if c.MinHoldHours <= 0 {
		c.MinHoldHours = d.MinHoldHours
	}
	if c.MaxHoldHours <= 0 {
		c.MaxHoldHours = d.MaxHoldHours
	}
	if c.NegativeSettlementsBeforeExit <= 0 {
		c.NegativeSettlementsBeforeExit = d.NegativeSettlementsBeforeExit
	}
	if c.StopLossBps >= 0 {
		c.StopLossBps = d.StopLossBps
	}
	if c.BasisStopBps >= 0 {
		c.BasisStopBps = d.BasisStopBps
	}
	if c.ExitDriftMinConsecutive <= 0 {
		c.ExitDriftMinConsecutive = d.ExitDriftMinConsecutive
	}
	if c.ReenterCooldown <= 0 {
		c.ReenterCooldown = d.ReenterCooldown
	}
	if len(c.TakerBps) == 0 {
		c.TakerBps = d.TakerBps
	}
	if c.NotionalUSD <= 0 {
		c.NotionalUSD = d.NotionalUSD
	}
	return c
}

// --- candidate ----------------------------------------------------------------

// Candidate is one measured cross-venue pair, ready to be judged.
//
// The caller MUST have read both books before building this. Every slippage
// field is a measurement from hyperliquid.Book.SweepCost or its equivalent, at
// the intended notional -- not the venue's own impact-spread proxy, which is
// computed at a size the venue chooses rather than ours.
type Candidate struct {
	Coin       string
	LongVenue  string
	ShortVenue string

	LongBpsHr  float64
	ShortBpsHr float64

	LongIntervalHours  float64
	ShortIntervalHours float64

	// Whether the VENUE published the interval for this symbol, or whether it
	// is a documented venue-wide default.
	//
	// Binance publishes only the symbols that differ from its 8-hour default,
	// so an absent symbol is 8h -- correct for most, and catastrophic for the
	// ones it isn't. KAITOUSDT is 4h and IS published; the old code never
	// asked, assumed 8, and read that pair's spread 4.4x too rich for a day.
	LongIntervalExplicit  bool
	ShortIntervalExplicit bool

	LongNextFundingMs  int64
	ShortNextFundingMs int64

	// Books measured?
	LongMeasured  bool
	ShortMeasured bool

	// Slippage, one way each, at the intended notional.
	LongEntrySlipBps  float64 // buying the long leg
	ShortEntrySlipBps float64 // selling the short leg
	LongExitSlipBps   float64 // selling the long leg back
	ShortExitSlipBps  float64 // buying the short leg back

	// Exhausted means the book could not fill the size at all. A partial fill
	// on the second leg leaves a naked position, so this is a refusal.
	LongExhausted  bool
	ShortExhausted bool

	// Depth available within an acceptable slippage band, USD, per venue --
	// orderbook.Book.DepthWithinBps, NOT the touch.
	//
	// Measured 2026-08-12: KAITO's touch held $15 on Binance while $400 filled
	// for 3.176 bps round trip. Sizing off the touch would have refused a pair
	// that fills perfectly well. The touch is the front level, not the book.
	LongDepthUSD  float64
	ShortDepthUSD float64

	VolUSD float64

	// BasisBps is the PRICE gap between the two venues' mids, signed:
	//
	//	(midLong - midShort) / avgMid * 10000
	//
	// This is the position's entire price P&L. Long one perp and short
	// another, the result is basis_at_exit minus basis_at_entry and nothing
	// else.
	//
	// Measured 2026-08-12: KAITO showed 0.618150 on Binance against 0.626650
	// on Bybit -- 136.6 bps of basis, roughly 5.4 HOURS of that pair's funding
	// spread, sitting in a variable nothing was checking.
	//
	// It is not a coincidence. Funding is the mechanism that drags a perp back
	// to spot, so funding is high on a venue precisely BECAUSE its perp trades
	// at a premium there. The funding spread and the price basis are the same
	// phenomenon measured two ways, and collecting the first means holding
	// through the second.
	BasisBps float64

	// BasisMeasured is false if either mid was unreadable.
	BasisMeasured bool

	// --- SETTLED funding, the signal entries are actually decided on ---
	//
	// LongBpsHr and ShortBpsHr above are PREDICTIONS. Measured across fifteen
	// closed positions on 2026-08-19, predictions averaged 7.30 bps/hr against
	// 0.45 bps/hr actually settled, missing low in 8 of 8 cases. They are kept
	// because they drive the settlement calendar and tell an open position what
	// is happening now -- but they no longer decide anything.
	SettledSpreadBpsHr float64
	// Per-leg settled rates. The spread alone is not enough: each leg settles
	// on its own interval, so the repayment plan needs them separately.
	SettledLongBpsHr  float64
	SettledShortBpsHr float64
	// Recent settled spread: is the episode still running, or already over?
	SettledRecentSpreadBpsHr float64
	SettledIntervals         int
	// SettledSameSign is the weaker leg's persistence, 0..1. A mean of +6 built
	// from +30,-20,+12,-4 is estimation noise; the same mean from +6,+7,+5,+6
	// is an economic difference. The mean alone cannot tell them apart.
	SettledSameSign float64
	SettledKnown    bool
}

// SpreadBpsHr is positive by construction when the venues are correctly
// assigned.
// reversed exchanges a candidate's legs.
//
// NEEDED BECAUSE A SPREAD CAN CHANGE SIGN WHILE A POSITION IS OPEN.
//
// Candidates are always emitted with the profitable leg long, so when ONG's
// spread flipped on 2026-08-17 the feed started publishing ONG|bybit|binance
// while the open position was keyed ONG|binance|bybit. The keys stopped
// matching, Update treated the pair as vanished, and the position froze: no
// settlements booked for over ten hours, next-funding clock stuck at 08:00,
// and exitReason handed a zero Candidate so it could not close either. Held,
// unmeasured, unexitable, for up to max-hold.
//
// THIS IS NOT A FIELD-FOR-FIELD SWAP.
//
// Slippage maps CROSS-WISE. LongEntrySlipBps is the cost of BUYING the long
// venue; after reversal the long venue is the old short one, and the cost of
// buying that is the old ShortExitSlipBps -- buying the short leg back. Swap
// entry-for-entry and every cost is the wrong side of its own book.
//
// BasisBps is (midLong - midShort), so exchanging the legs NEGATES it. Copying
// it unchanged would flip the sign of the position's entire price P&L.
func (c Candidate) reversed() Candidate {
	r := c
	r.LongVenue, r.ShortVenue = c.ShortVenue, c.LongVenue
	r.LongBpsHr, r.ShortBpsHr = c.ShortBpsHr, c.LongBpsHr
	r.LongIntervalHours, r.ShortIntervalHours = c.ShortIntervalHours, c.LongIntervalHours
	r.LongIntervalExplicit, r.ShortIntervalExplicit = c.ShortIntervalExplicit, c.LongIntervalExplicit
	r.LongNextFundingMs, r.ShortNextFundingMs = c.ShortNextFundingMs, c.LongNextFundingMs
	r.LongMeasured, r.ShortMeasured = c.ShortMeasured, c.LongMeasured
	r.LongExhausted, r.ShortExhausted = c.ShortExhausted, c.LongExhausted
	r.LongDepthUSD, r.ShortDepthUSD = c.ShortDepthUSD, c.LongDepthUSD
	r.LongEntrySlipBps = c.ShortExitSlipBps
	r.ShortEntrySlipBps = c.LongExitSlipBps
	r.LongExitSlipBps = c.ShortEntrySlipBps
	r.ShortExitSlipBps = c.LongEntrySlipBps
	r.SettledLongBpsHr, r.SettledShortBpsHr = c.SettledShortBpsHr, c.SettledLongBpsHr
	r.SettledSpreadBpsHr = -c.SettledSpreadBpsHr
	r.SettledRecentSpreadBpsHr = -c.SettledRecentSpreadBpsHr
	r.BasisBps = -c.BasisBps
	return r
}

// candOrReversed finds a position's candidate, accepting the reversed legs.
//
// Takes the three key parts rather than a Position so it works from any loop
// regardless of what is in scope there.
// candOrReversed finds a position's candidate and ORIENTS it to that
// position's stored legs. One lookup now, because the key no longer encodes
// direction; what remains is orientation.
func candOrReversed(byKey map[string]Candidate, coin, long, short string) Candidate {
	c, ok := byKey[pairKeyOf(coin, long, short)]
	if !ok {
		return Candidate{}
	}
	if c.LongVenue != long {
		return c.reversed()
	}
	return c
}

func (c Candidate) SpreadBpsHr() float64 { return c.ShortBpsHr - c.LongBpsHr }

// Key matches Position.Key.
// Key uses the same unordered form as Position.Key, so a candidate matches the
// position it belongs to regardless of which way the spread currently points.
func (c Candidate) Key() string { return pairKeyOf(c.Coin, c.LongVenue, c.ShortVenue) }

// --- assessment ---------------------------------------------------------------

// Assessment is the full reasoning about one candidate, kept whether it passed
// or not. A refusal that cannot be read is indistinguishable from a bug.
type Assessment struct {
	Candidate Candidate

	Gate   string
	Reason string

	NotionalUSD  float64
	Reduced      bool
	LimitedBy    string
	EntryCostBps float64
	ExitCostBps  float64
	RoundTripBps float64
	BreakEvenHrs float64

	// Plan is the simulated forward walk of the settlement calendar. Its
	// WorstNetBps is what the stop loss will actually see, and it is the only
	// honest answer to "how far underwater does this go first".
	Plan SettlementPlan

	Viable bool
}

// assess applies the entry gate. Ordered cheapest-to-check first, but every
// structural refusal precedes every numeric one -- there is no point costing a
// pair whose fees are unknown.
// Constants for the conservative net-edge gate. Specified in
// docs/JULES-PROMPT.md. Deliberately constants, not flags: a parameter that can
// be raised until trades start passing is how a system gets fitted to a target
// return rather than measured against reality.
const (
	// expectedHoldHours is eight hours of clock time discounted by measured decay.
	//
	// cmd/decay over 27,530 observations: median half-life 4.4h, median pair
	// lifespan 7.0h, and the area under the falling curve across an 8-hour hold is
	// 5.86 spread-hours -- the flat 8.0 overstated collection by 1.37x. Was 8.0
	// until 2026-08-20.
	//
	// TWO REASONS 5.86 IS STILL OPTIMISTIC, both pointing the same way:
	//
	// Censoring. passes.jsonl records only pairs above the entry floor, so pairs
	// decaying below it leave the sample instead of registering as small numbers.
	// The fastest-dying pairs are absent from the measurement.
	//
	// Reversal. Of fifteen closed positions, SEVEN had their spread REVERSE rather
	// than decay. An area-under-the-curve model assumes a smooth tail; half the
	// time there is no tail but a sign flip, and the position pays funding out
	// while it is open. No decay integral captures that.
	//
	// Re-run cmd/decay as the sample grows. A constant, not a flag: a parameter
	// that can be raised until trades pass is how a system gets fitted to a target.
	expectedHoldHours = 5.86

	// maxMeasuredSpreadBpsHr is the top of the range where expectedHoldHours has
	// any evidence behind it. cmd/decay on 2026-08-21 bucketed 35,000 samples by
	// entry spread: 21,938 below 2 bps/hr, 7,199 from 2-5, 6,060 from 5-20, and
	// THREE above 20. There is no measurement of how a violent dislocation decays.
	//
	// Multiplying a 138 bps/hr spread by a constant fitted to sub-20 pairs is the
	// same error as defaulting an unknown settlement interval to eight hours, and
	// it cost the same way: ONG entered at 138.004 on 2026-08-21, the gate expected
	// 138 x 5.86 = 808 bps, it collected 215 before the spread crossed zero inside
	// three hours, and the basis converged 405 bps against the position.
	//
	// Raise this only when cmd/decay reports a real sample above it.
	maxMeasuredSpreadBpsHr = 20.0

	// basisReserveBps covers venue price divergence during the hold. Measured
	// drift tails ran -30.6 to +22.5 bps against a median of +0.9. A judgement,
	// not a measurement.
	basisReserveBps = 15.0

	// borrowReserveBps is zero for cross-venue perp-perp: there is no spot leg
	// and nothing is borrowed. Kept in the arithmetic so the cash-and-carry
	// book can supply a real figure without changing the shape.
	borrowReserveBps = 0.0

	// minCostSamples is the sample floor for a per-pair p95. Below this the
	// pair is REFUSED, never estimated from a global figure.
	minCostSamples = 40
)

func (b *Book) assess(c Candidate, now time.Time) Assessment {
	a := Assessment{Candidate: c}
	cfg := b.cfg

	refuse := func(gate, format string, args ...any) Assessment {
		a.Gate = gate
		a.Reason = fmt.Sprintf(format, args...)
		a.Viable = false
		return a
	}

	if c.LongVenue == c.ShortVenue {
		return refuse(GateSameVenue, "both legs on %s is not a cross-venue trade", c.LongVenue)
	}
	if !cfg.FeesVerified {
		return refuse(GateFeesUnverified,
			"nobody has asserted the fee schedules were read; an edge of a few bps per hour "+
				"cannot survive a wrong fee")
	}
	takerLong, okL := b.takerFor(c.LongVenue, c.Coin)
	takerShort, okS := b.takerFor(c.ShortVenue, c.Coin)
	if !okL || !okS {
		return refuse(GateFeesUnverified, "no taker fee recorded for %s or %s", c.LongVenue, c.ShortVenue)
	}
	// THE ENTRY DECISION IS MADE ON SETTLED FUNDING.
	//
	// Predictions were used until 2026-08-19. Across fifteen closed positions
	// they averaged 7.30 bps/hr against 0.45 actually settled, missing low in
	// 8 of 8 cases, and total funding collected was NEGATIVE 158.7 bps.
	//
	// Funding is a time-weighted average premium over its interval. Early in an
	// interval that average comes from a short, noisy window, so the published
	// prediction is an extreme that converges as the interval fills. Screening
	// thousands of pairs for the widest spread selects the pairs where that
	// error is largest, then differences two of them.
	//
	// An unreadable settled history is a REFUSAL, never a fallback to the
	// prediction. That fallback is the whole bug.
	if b.Costs != nil {
		if !c.SettledKnown {
			return refuse(GateNoSettledHistory,
				"no settled funding history for %s on %s/%s; will not fall back to a prediction",
				c.Coin, c.LongVenue, c.ShortVenue)
		}
		if b.MinSettledIntervals > 0 && c.SettledIntervals < b.MinSettledIntervals {
			return refuse(GateNoSettledHistory,
				"only %d settled intervals, need %d", c.SettledIntervals, b.MinSettledIntervals)
		}
		if c.SpreadBpsHr() <= 0 {
			return refuse(GateSignalsDisagree,
				"settled says %+.3f bps/hr but the current prediction says %+.3f; the spread is turning",
				c.SettledRecentSpreadBpsHr, c.SpreadBpsHr())
		}
		if b.MinSettledSameSign > 0 && c.SettledSameSign < b.MinSettledSameSign {
			return refuse(GateSettledInconsistent,
				"settled averages %+.3f but only %.0f%% of settlements share a sign",
				c.SettledSpreadBpsHr, c.SettledSameSign*100)
		}

		// THE ENTRY DECISION: EXPECTED NET AFTER MEASURED COST.
		//
		// Replaces a flat spread threshold, which judged a pair costing 25 bps
		// to trade identically to one costing 130 -- most of the measured range.
		// Stressed, not measured: listslip prices both halves of the round trip
		// against the same calm book, but exits happen when the position has gone
		// wrong and the book has thinned. See exitStressSlipMultiple.
		p95, n := b.Costs.P95StressedCostBps(c.LongVenue, c.ShortVenue)
		if n < minCostSamples {
			return refuse(GateCostUnmeasured,
				"only %d fill-cost samples for %s/%s, need %d; measure it rather than estimate it",
				n, c.LongVenue, c.ShortVenue, minCostSamples)
		}
		if c.SettledRecentSpreadBpsHr > maxMeasuredSpreadBpsHr {
			return refuse(GateSpreadUnmeasured,
				"settled spread %+.3f bps/hr is above the measured decay range (%.0f); cmd/decay has no sample up there, so the hold assumption does not apply",
				c.SettledRecentSpreadBpsHr, maxMeasuredSpreadBpsHr)
		}
		expected := c.SettledRecentSpreadBpsHr * expectedHoldHours
		net := expected - p95 - basisReserveBps - borrowReserveBps
		if net <= 0 {
			fading := ""
			if c.SettledSpreadBpsHr > 0 && c.SettledRecentSpreadBpsHr < c.SettledSpreadBpsHr*0.5 {
				fading = fmt.Sprintf("; recent %+.3f is less than half the twelve-settlement mean %+.3f, so the episode is passing",
					c.SettledRecentSpreadBpsHr, c.SettledSpreadBpsHr)
			}
			return refuse(GateExpectedNetNegative,
				"expected %+.1f bps over %.0fh at %+.3f bps/hr, less p95 STRESSED cost %.1f (n=%d), basis %.0f, borrow %.0f = %+.1f%s",
				expected, expectedHoldHours, c.SettledRecentSpreadBpsHr,
				p95, n, basisReserveBps, borrowReserveBps, net, fading)
		}
	}

	if c.LongIntervalHours <= 0 || c.ShortIntervalHours <= 0 {
		return refuse(GateUnknownInterval,
			"settlement interval unknown (long %v, short %v); guessing is an 8x error",
			c.LongIntervalHours, c.ShortIntervalHours)
	}
	if !c.LongMeasured || !c.ShortMeasured {
		return refuse(GateBookUnmeasured,
			"book not read on %s=%v %s=%v; sizing against an unread book is an assumption",
			c.LongVenue, c.LongMeasured, c.ShortVenue, c.ShortMeasured)
	}
	if c.LongExhausted || c.ShortExhausted {
		return refuse(GateBookTooThin,
			"book cannot fill the size (long exhausted=%v, short exhausted=%v); "+
				"a partial second leg leaves a naked position",
			c.LongExhausted, c.ShortExhausted)
	}

	// Gate on the SETTLED spread wherever we have it. The predicted spread is
	// the cheap pre-filter that brought this candidate here; it is not
	// evidence, and on 2026-08-19 it averaged 7.30 bps/hr against 0.45 paid.
	spread := c.SpreadBpsHr()
	if c.SettledKnown {
		spread = c.SettledSpreadBpsHr
	}
	if spread <= 0 {
		return refuse(GateSpreadInverted, "spread %+.4f bps/hr: the venues are the wrong way round", spread)
	}
	if spread < cfg.MinSpreadBpsHr {
		return refuse(GateSpreadTooSmall, "spread %.4f bps/hr below the %.4f floor", spread, cfg.MinSpreadBpsHr)
	}
	if c.VolUSD > 0 && c.VolUSD < cfg.MinVolUSD {
		return refuse(GateVolumeTooLow, "24h volume $%.0f below the $%.0f floor", c.VolUSD, cfg.MinVolUSD)
	}

	// Off by default. When enabled, an unmeasured basis is also refused: an
	// unread price gap and a small one must not produce the same answer.
	if cfg.MaxEntryBasisBps > 0 {
		if !c.BasisMeasured {
			return refuse(GateBasisTooWide, "price basis could not be measured on both venues")
		}
		if ab := math.Abs(c.BasisBps); ab > cfg.MaxEntryBasisBps {
			return refuse(GateBasisTooWide,
				"venues disagree on price by %.1f bps, past the %.1f cap; that gap is the "+
					"position's entire price P&L", ab, cfg.MaxEntryBasisBps)
		}
	}

	// --- sizing, against the THINNER leg ---
	notional := cfg.NotionalUSD
	a.LimitedBy = "target"
	if cfg.Adaptive {
		capL := c.LongDepthUSD * cfg.DepthFraction
		capS := c.ShortDepthUSD * cfg.DepthFraction
		cap := capL
		limited := "long book"
		if capS < cap {
			cap, limited = capS, "short book"
		}
		if cap < notional {
			notional = cap
			a.Reduced = true
			a.LimitedBy = limited
		}
	}
	// Both legs must clear their own venue's minimum, so the LARGER binds --
	// a size the cheap venue accepts is still impossible if the other side
	// rejects it, and a rejected second leg leaves the first one naked.
	venueMin := 0.0
	whichVenue := ""
	for _, v := range []string{c.LongVenue, c.ShortVenue} {
		m, ok := cfg.MinNotionalByVenue[v]
		if !ok {
			// A venue whose minimum nobody has read is not assumed to be
			// permissive.
			m = cfg.MinNotionalUSD
		}
		if m > venueMin {
			venueMin, whichVenue = m, v
		}
	}
	floor, reason := venueMin, whichVenue+" minimum"
	if cfg.MinNotionalUSD > floor {
		floor, reason = cfg.MinNotionalUSD, "our own smallest-position preference"
	}
	if notional < floor {
		return refuse(GateBelowMinNotional,
			"$%.2f is below the $%.2f floor set by %s (sized by %s); a rejected "+
				"second leg leaves the first leg naked",
			notional, floor, reason, a.LimitedBy)
	}
	a.NotionalUSD = notional

	// --- cost: four legs, fees plus MEASURED slippage ---
	a.EntryCostBps = takerLong + takerShort + c.LongEntrySlipBps + c.ShortEntrySlipBps
	a.ExitCostBps = takerLong + takerShort + c.LongExitSlipBps + c.ShortExitSlipBps
	a.RoundTripBps = a.EntryCostBps + a.ExitCostBps

	if a.EntryCostBps > cfg.MaxEntryCostBps {
		return refuse(GateEntryCostTooHigh, "entry costs %.2f bps against a %.2f cap",
			a.EntryCostBps, cfg.MaxEntryCostBps)
	}

	// STOPPED ON ARRIVAL.
	//
	// NetBps is funding minus the round trip, so a position opens at minus its
	// own round trip and climbs from there. When that round trip already exceeds
	// the stop loss, the position is past the floor the moment it exists and is
	// closed before funding can arrive.
	//
	// Measured 2026-08-22: BLUAI opened at a 63.5 bps round trip against a -60
	// stop and was closed 10 minutes later at net -63.51, having collected
	// nothing. 1000CAT at 64.6 lasted 0.11h. CXMT at 79.8 lasted 0.17h. TOWNS at
	// 96.5 lasted 1.5h. Four round trips paid for four positions that were never
	// capable of surviving their own opening.
	//
	// The stop loss is not wrong to fire. The entry was wrong to happen.
	if cfg.StopLossBps < 0 && -a.RoundTripBps <= cfg.StopLossBps {
		return refuse(GateStoppedOnArrival,
			"round trip %.2f bps already breaches the %.2f stop loss at zero funding; "+
				"this position would be closed before it could collect anything",
			a.RoundTripBps, cfg.StopLossBps)
	}

	// --- the settlement calendar ---
	//
	// round_trip / spread assumes funding trickles in. It arrives as LUMPS on
	// two unaligned clocks, and the path between them can breach the stop loss
	// long before the position turns profitable. Both losses on 2026-08-12 were
	// exactly that: continuous break-even said 4.9 hours, and a full 4-hour
	// Binance charge landed at hour 1.75 against two hours of collected
	// Hyperliquid funding.
	tLong, okL := hoursUntil(c.LongNextFundingMs, now)
	tShort, okS := hoursUntil(c.ShortNextFundingMs, now)
	if !okL || !okS {
		return refuse(GateNoSchedule,
			"no next-funding timestamp from %s or %s; without the calendar there is no "+
				"way to know whether a lump charge lands before this can pay for itself",
			c.LongVenue, c.ShortVenue)
	}

	// BOTH LEGS MUST SETTLE INSIDE THE HOLD.
	//
	// Funding arrives in lumps on two unaligned clocks. A leg whose next
	// settlement falls outside the expected hold contributes nothing while still
	// paying its half of the round trip.
	//
	// Measured 2026-08-22 across 36 closed cross-venue positions: NINE collected
	// zero funding. SNXX and ZHIPU held 8-hour symbols for roughly 4 hours,
	// which cannot collect. Leg mismatch is the same fault in a subtler form --
	// ASTS settled 1/0 and CXMT 0/6, so one leg paid and the other did not,
	// leaving the position carrying unhedged funding exposure for the gap.
	if tLong > expectedHoldHours || tShort > expectedHoldHours {
		return refuse(GateSettlesAfterHold,
			"next settlement is %.1fh away on %s and %.1fh on %s, against a %.2fh expected hold; "+
				"a leg that does not settle collects nothing and still pays its half of the round trip",
			tLong, c.LongVenue, tShort, c.ShortVenue, expectedHoldHours)
	}

	horizon := cfg.MaxHoldHours
	if horizon > 168 {
		// A week is more than enough to judge an ENTRY. Beyond that the rates
		// used here are fiction anyway.
		horizon = 168
	}
	a.Plan = simulatePlan(c, a.EntryCostBps, a.ExitCostBps, tLong, tShort, horizon)
	if !a.Plan.Ok {
		return refuse(GateNoSchedule, "settlement calendar could not be simulated")
	}

	if !a.Plan.ReachesBreakEven {
		return refuse(GateCannotRepay,
			"never repays %.2f bps within %.0f hours at %.4f bps/hr (%s)",
			a.RoundTripBps, horizon, spread, a.Plan.Describe())
	}
	a.BreakEvenHrs = a.Plan.BreakEvenHours
	if a.BreakEvenHrs > cfg.MaxBreakEvenHours {
		return refuse(GateCannotRepay,
			"needs %.1f hours to repay %.2f bps, past the %.0f hour cap (%s)",
			a.BreakEvenHrs, a.RoundTripBps, cfg.MaxBreakEvenHours, a.Plan.Describe())
	}

	// THE ENTRY-TIMING GATE.
	//
	// A path that breaches the stop loss on the way to profit is not a
	// profitable trade, it is a realised loss. Nothing downstream can save it:
	// the drop is instantaneous at settlement, so there is no drift for the
	// exit rule to catch.
	if a.Plan.WorstNetBps <= cfg.StopLossBps {
		return refuse(GateStopsOutBeforeProfit,
			"reaches %.2f bps at hour %.2f, at or past the %.2f stop, before breaking even "+
				"at hour %.2f. The %s leg settles %.0f hours of funding in %.2fh. Entering "+
				"just after that settlement instead would survive it",
			a.Plan.WorstNetBps, a.Plan.WorstAtHours, cfg.StopLossBps,
			a.Plan.BreakEvenHours, a.Plan.SlowVenue,
			a.Plan.SlowIntervalHours, a.Plan.FirstSlowSettleHours)
	}

	a.Viable = true
	a.Gate = GateOK
	return a
}

// Assess exposes the entry gate for reporting.
//
// A refusal that cannot be read is indistinguishable from a bug, and "0
// positions opened" has been the wrong answer often enough in this project
// that the reason has to be printable on demand.
func (b *Book) Assess(c Candidate, now time.Time) Assessment { return b.assess(c, now) }

// --- the book -----------------------------------------------------------------

type bookState struct {
	Open     map[string]*Position `json:"open"`
	Closed   []*Position          `json:"closed"`
	Cooldown map[string]time.Time `json:"cooldown"`
}

// Book holds the cross-venue paper positions and persists them.
type Book struct {
	// TakerLookup supplies a PER-SYMBOL taker fee for venues that price fees
	// per symbol rather than venue-wide. Returning ok=false falls back to
	// Config.TakerBps.
	//
	// MEXC needs this: probed 2026-08-17 across 1,116 live contracts, 540
	// charge ZERO taker, 472 charge 2 bps, 66 charge 4 and 25 charge 10. Any
	// single venue-level number is wrong for nearly every symbol, so MEXC was
	// left out of the map entirely and GateFeesUnverified refused it 957 times
	// in one day -- on the cheapest venue measured.
	TakerLookup func(venue, coin string) (float64, bool)

	// DeadAfterHours closes a position that cannot repay what it still owes
	// within this many hours at its CURRENT spread. 0 disables.
	// Costs holds measured round-trip fill costs per venue pair.
	//
	// It replaces a fixed spread threshold. A flat "3 bps/hr" bar treats a pair
	// costing 25 bps to trade the same as one costing 130, which is most of the
	// range actually measured. nil disables the gate entirely.
	Costs *Costs

	// MinSettledSameSign rejects a wide mean built from alternating signs.
	MinSettledSameSign float64

	// MinSettledIntervals is how many settlements must back the estimate.
	MinSettledIntervals int

	DeadAfterHours float64

	// ReversedBleedHours closes a REPAID position whose spread has turned
	// against it, once the bleed would give back its own exit cost within this
	// many hours. 0 disables.
	ReversedBleedHours float64

	// Capital enforces this book's TOTAL capital ceiling across every leg
	// of every open position.
	//
	// Nil means unlimited, which is what every earlier run and every test
	// assumed. Wiring a ledger in is what makes "$400" mean $400 for the
	// whole book rather than $400 per leg.
	Capital *capital.Ledger

	// Leverage on each perp leg, used to convert notional into the capital
	// that must actually sit on the venues. Zero means 1 -- unlevered --
	// which reproduces exactly the CapitalUSD = 2 x notional this book has
	// recorded since it was written. Introducing the ledger therefore moves
	// no existing number.
	Leverage float64

	dir   string
	cfg   Config
	state bookState
}

// Result is what one Update pass did.
type Result struct {
	At time.Time

	Opened []string
	Closed []string

	Assessed  int
	Viable    int
	Refusals  map[string]int
	Settled   int
	OpenCount int
}

// NewBook loads or creates the book at dir/cross_positions.json.
func NewBook(dir string, cfg Config) (*Book, error) {
	b := &Book{dir: dir, cfg: cfg.withDefaults()}
	b.state = bookState{
		Open:     map[string]*Position{},
		Cooldown: map[string]time.Time{},
	}

	path := b.path()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return b, nil
	}
	if err != nil {
		return nil, fmt.Errorf("crossvenue: reading %s: %w", path, err)
	}
	if len(raw) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(raw, &b.state); err != nil {
		return nil, fmt.Errorf("crossvenue: decoding %s: %w", path, err)
	}
	if b.state.Open == nil {
		b.state.Open = map[string]*Position{}
	}
	if b.state.Cooldown == nil {
		b.state.Cooldown = map[string]time.Time{}
	}

	// MIGRATE DIRECTIONAL KEYS WRITTEN BEFORE 2026-08-19.
	//
	// Re-keying from each position's OWN fields converts an old file in place:
	// no state lost, no measurement baseline reset. A stored key is never
	// trusted, which also repairs a file edited into an inconsistent state.
	migrated := make(map[string]*Position, len(b.state.Open))
	for _, pos := range b.state.Open {
		k := pos.Key()
		if prev, clash := migrated[k]; clash && prev.OpenedAt.Before(pos.OpenedAt) {
			continue
		}
		migrated[k] = pos
	}
	b.state.Open = migrated
	return b, nil
}

func (b *Book) path() string { return filepath.Join(b.dir, "cross_positions.json") }

// Config returns the effective config, defaults applied.
func (b *Book) Config() Config { return b.cfg }

// Update runs one pass: accrue, then exit, then enter.
//
// The order matters. Accruing before exiting means a position closed this pass
// is credited with funding it actually received. Exiting before entering frees
// a slot in the same pass rather than the next one.
func (b *Book) Update(now time.Time, cands []Candidate) (Result, error) {
	res := Result{At: now, Refusals: map[string]int{}}

	// Free capital still held against positions this book no longer has
	// open. One crash between closing a position and releasing its capital
	// would otherwise leak that capital permanently, until the book began
	// refusing trades it could plainly afford and nobody could say why.
	//
	// This is the same orphan failure that left 22 positions written as open
	// forever after a book was superseded, caught one layer lower.
	if b.Capital != nil {
		open := make([]string, 0, len(b.state.Open))
		for k := range b.state.Open {
			open = append(open, k)
		}
		if _, err := b.Capital.Reconcile(open); err != nil {
			return res, fmt.Errorf("crossvenue: reconciling capital: %w", err)
		}
	}

	byKey := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		byKey[c.Key()] = c
	}

	// --- 1. accrue ---
	for key, p := range b.state.Open {
		c, ok := byKey[key]
		if ok && c.LongVenue != p.LongVenue {
			// Same pair, presented the other way round because the spread
			// turned. Orient it to this position's legs before anything
			// reads a Long or Short field off it.
			c = c.reversed()
		}
		if !ok {
			// Genuinely absent from the feed. Do NOT accrue and do NOT close:
			// a missing observation is missing information, not a settlement
			// and not an exit signal.
			continue
		}

		// Capture the rates that applied DURING the interval now ending,
		// BEFORE the new observation overwrites them.
		//
		// predictedFundings returns the rate for the NEXT settlement. Detecting
		// a settlement and then booking it at that number accrues the coming
		// hour's prediction against the hour that just finished. Hyperliquid
		// moved 24.806 -> 24.298 bps/hr in twenty minutes on 2026-08-11, and
		// it settles 24 times a day, so the error compounds.
		prevLong, prevShort := p.LastLongBpsHr, p.LastShortBpsHr

		p.ObserveRates(c.LongBpsHr, c.ShortBpsHr, now)
		if c.BasisMeasured {
			p.LastBasisBps = c.BasisBps
			p.BasisMeasured = true
		}

		// Exit cost, measured fresh every pass. This is what lets the stop-loss
		// see a deteriorating exit.
		takerLong, _ := b.takerFor(p.LongVenue, p.Coin)
		takerShort, _ := b.takerFor(p.ShortVenue, p.Coin)
		if c.LongMeasured && c.ShortMeasured && !c.LongExhausted && !c.ShortExhausted {
			p.ObserveExitCost(takerLong+takerShort+c.LongExitSlipBps+c.ShortExitSlipBps, now)
		}

		// Settlement detection: the venue's own next-funding timestamp
		// ADVANCING is the event. Nothing here reimplements a venue schedule.
		// Book EVERY interval that elapsed, not one per poll.
		//
		// The venue's next-funding timestamp advancing by N intervals means N
		// settlements happened. Booking one drops the rest, and because the
		// dropped ones are whichever leg settles faster, the error is not
		// symmetric -- it silently flatters the position.
		//
		// All missed intervals are booked at the same rate, which is an
		// approximation. It is a far smaller one than pretending they did not
		// happen.
		settleN := func(nextMs int64, prevNext int64, intervalHours float64,
			settle func(float64, time.Time), rate float64) int {
			if nextMs <= 0 || prevNext <= 0 || nextMs <= prevNext || intervalHours <= 0 {
				return 0
			}
			step := intervalHours * 3600 * 1000
			n := int(math.Round(float64(nextMs-prevNext) / step))
			if n < 1 {
				n = 1
			}
			if n > 200 {
				// A jump this large means the venue's clock or our snapshot is
				// nonsense. Book one and let the count show the anomaly.
				n = 1
			}
			for i := 0; i < n; i++ {
				settle(rate, now)
			}
			return n
		}

		if c.LongNextFundingMs > 0 {
			n := settleN(c.LongNextFundingMs, p.LongNextFundingMs,
				p.LongIntervalHours, p.SettleLong, prevLong)
			res.Settled += n
			if n > 1 {
				p.MissedSettlements += n - 1
			}
			p.LongNextFundingMs = c.LongNextFundingMs
		}
		if c.ShortNextFundingMs > 0 {
			n := settleN(c.ShortNextFundingMs, p.ShortNextFundingMs,
				p.ShortIntervalHours, p.SettleShort, prevShort)
			res.Settled += n
			if n > 1 {
				p.MissedSettlements += n - 1
			}
			p.ShortNextFundingMs = c.ShortNextFundingMs
		}

		// Compare against the forecast made at entry. Done AFTER accrual so
		// the comparison sees the settlement that just landed.
		p.CheckAgainstPlan(now)
	}

	// --- 2. exit ---
	for key, p := range b.state.Open {
		reason, exitBps, measured := b.exitReason(p, candOrReversed(byKey, p.Coin, p.LongVenue, p.ShortVenue), now)
		if reason == "" {
			continue
		}
		p.Close(reason, exitBps, measured, now)
		b.state.Closed = append(b.state.Closed, p)
		delete(b.state.Open, key)
		if b.Capital != nil {
			// A failed release is deliberately not fatal here. Reconcile at the
			// top of the next Update frees any hold whose position is no longer
			// open, so the error self-heals within one cycle. Aborting instead
			// would abandon Update with the book half-processed and unsaved,
			// which is the worse of the two failures.
			_, _, _ = b.Capital.Release(key)
		}
		b.state.Cooldown[p.Coin] = now
		res.Closed = append(res.Closed, fmt.Sprintf("%s: %s", p.Pair(), reason))
	}

	// --- 3. enter ---
	assessments := make([]Assessment, 0, len(cands))
	for _, c := range cands {
		a := b.assess(c, now)
		res.Assessed++
		if !a.Viable {
			res.Refusals[a.Gate]++
			continue
		}
		res.Viable++
		assessments = append(assessments, a)
	}

	// Best first: shortest break-even, because time in the position is the
	// risk. A pair that repays in 2 hours is worth more than one paying twice
	// as much that needs 20.
	sort.Slice(assessments, func(i, j int) bool {
		return assessments[i].BreakEvenHrs < assessments[j].BreakEvenHrs
	})

	for _, a := range assessments {
		if len(b.state.Open) >= b.cfg.MaxConcurrent {
			res.Refusals[GateAtCapacity]++
			continue
		}
		c := a.Candidate
		if b.holdsCoin(c.Coin) {
			res.Refusals[GateAlreadyHeld]++
			continue
		}
		if t, ok := b.state.Cooldown[c.Coin]; ok && now.Sub(t) < b.cfg.ReenterCooldown {
			res.Refusals[GateCooldown]++
			continue
		}

		// Capital is not notional. Both legs of a cross-venue position post
		// margin, and neither venue offsets the other, so the money that must
		// actually sit on the two venues is larger than the exposure it buys.
		capUSD, capErr := capital.CapitalForNotional(
			capital.CrossVenue, a.NotionalUSD, b.leverage(), false)
		if capErr != nil {
			res.Refusals[GateNoCapital]++
			continue
		}

		p := &Position{
			Coin:               c.Coin,
			LongVenue:          c.LongVenue,
			ShortVenue:         c.ShortVenue,
			OpenedAt:           now,
			NotionalUSD:        a.NotionalUSD,
			CapitalUSD:         capUSD,
			LongIntervalHours:  c.LongIntervalHours,
			ShortIntervalHours: c.ShortIntervalHours,
			EntryLongBpsHr:     c.LongBpsHr,
			EntryShortBpsHr:    c.ShortBpsHr,
			EntrySpreadBpsHr:   c.SpreadBpsHr(),
			LastLongBpsHr:      c.LongBpsHr,
			LastShortBpsHr:     c.ShortBpsHr,
			LastObservedAt:     now,
			EntryCostBps:       a.EntryCostBps,
			EntryCostMeasured:  true,
			PlanPath:           a.Plan.Path,
			PlanCostBps:        a.EntryCostBps + a.ExitCostBps,
			EntryBasisBps:      c.BasisBps,
			LastBasisBps:       c.BasisBps,
			BasisMeasured:      c.BasisMeasured,
			LongNextFundingMs:  c.LongNextFundingMs,
			ShortNextFundingMs: c.ShortNextFundingMs,
		}
		// Seed the exit watch with the exit cost we JUST MEASURED.
		//
		// Without this the position falls back to assuming the exit mirrors the
		// entry, having had a real measurement in hand a microsecond earlier.
		// Observed 2026-08-11: the candidate priced a 23.1 bps round trip and
		// the position immediately reported 24.39 for the same trade.
		//
		// It also anchors the drift baseline at entry, so the first hourly
		// window measures against a real number rather than an estimate.
		p.ObserveExitCost(a.ExitCostBps, now)

		// Take the capital BEFORE the position is recorded, and skip the
		// candidate if the book cannot fund it. Refusing here is the entire
		// point of the ledger: a position whose capital was never actually
		// available is not a smaller position, it is a false measurement.
		if b.Capital != nil {
			if err := b.Capital.Hold(p.Key(), capUSD, a.NotionalUSD,
				capital.CrossVenue, c.Coin, now); err != nil {
				res.Refusals[GateNoCapital]++
				continue
			}
		}

		b.state.Open[p.Key()] = p
		res.Opened = append(res.Opened, p.Describe(now))
	}

	res.OpenCount = len(b.state.Open)
	return res, b.save()
}

func (b *Book) holdsCoin(coin string) bool {
	for _, p := range b.state.Open {
		if p.Coin == coin {
			return true
		}
	}
	return false
}

// exitReason decides whether to close, and returns the exit cost to book.
//
// ORDER IS THE RULE. Read top to bottom:
//
//  1. Max hold -- an absolute cap, no exceptions.
//  2. Stop loss -- always active, including inside the minimum hold. A floor
//     that switches off is not a floor.
//  3. Exit door closing -- leaving costs more each hour than staying earns.
//  4. Sustained negative spread -- but ONLY once past the minimum hold AND
//     only once actually profitable.
//
// Rule 4's second condition is the one that matters. Closing underwater turns
// an unrealised loss into a paid round trip, and doing that six times in four
// days is exactly how the cash-and-carry book lost 186 bps.
// takerFor prefers a per-symbol fee where the venue publishes one.
func (b *Book) takerFor(venue, coin string) (float64, bool) {
	if b.TakerLookup != nil {
		if v, ok := b.TakerLookup(venue, coin); ok {
			return v, true
		}
	}
	v, ok := b.cfg.TakerBps[venue]
	return v, ok
}

func (b *Book) exitReason(p *Position, c Candidate, now time.Time) (reason string, exitBps float64, measured bool) {
	takerLong, _ := b.takerFor(p.LongVenue, p.Coin)
	takerShort, _ := b.takerFor(p.ShortVenue, p.Coin)

	exitBps = p.EffectiveExitBps()
	measured = p.ExitWatch.Measured
	if c.LongMeasured && c.ShortMeasured && !c.LongExhausted && !c.ShortExhausted {
		exitBps = takerLong + takerShort + c.LongExitSlipBps + c.ShortExitSlipBps
		measured = true
	}

	held := p.HeldHours(now)

	if held >= b.cfg.MaxHoldHours {
		return fmt.Sprintf("max hold %.0fh reached", b.cfg.MaxHoldHours), exitBps, measured
	}

	if p.NetBps() <= b.cfg.StopLossBps {
		return fmt.Sprintf("stop loss: net %.2f bps at or below %.2f",
			p.NetBps(), b.cfg.StopLossBps), exitBps, measured
	}

	// Not gated on MinHoldHours. A hundred basis points of divergence is a risk
	// event whenever it appears, and waiting out a minimum hold is how a bad one
	// becomes a worse one. The reason states the funding net alongside the drift,
	// because the whole point is that this fires when funding looks fine.
	if d, ok := p.BasisDriftBps(); ok && b.cfg.BasisStopBps < 0 && d <= b.cfg.BasisStopBps {
		return fmt.Sprintf("basis stop: divergence %+.1f bps at or below %.0f (funding net %+.1f, all-in %+.1f)",
			d, b.cfg.BasisStopBps, p.NetBps(), p.AllInNetBps()), exitBps, measured
	}

	if held >= b.cfg.MinHoldHours {
		if closing, why := p.ExitWatch.DoorClosing(p.FundingPerDayBps(), b.cfg.ExitDriftMinConsecutive); closing {
			return why, exitBps, measured
		}
	}

	// DEAD MONEY.
	//
	// This deliberately contradicts the rule below it, which refuses to close
	// an underwater position because "the spread may recover; the round trip
	// never comes back". That reasoning is sound when slots are free. It is
	// wrong when they are not.
	//
	// Measured 2026-08-18: the book sat at OPEN POSITIONS (5) on 146 of 146
	// passes and refused 427 candidates for AT_CAPACITY, while CXMT offered
	// +11 bps/hr repaying in 2.6h and HOME offered +36. Five slots were held
	// by positions decayed to 1-2 bps/hr that could not repay their own cost
	// this side of max-hold, which is 720 hours.
	//
	// So the exit cost is paid and the loss locked, on purpose: the slot is
	// worth more than the position occupying it. That is a judgement about
	// opportunity cost, not a correction of a bug, and it is only right while
	// better candidates are genuinely being refused for capacity.
	if b.DeadAfterHours > 0 && held >= b.cfg.MinHoldHours && p.NetBps() < 0 {
		sp := p.CurrentSpreadBpsHr()
		if sp <= 0 {
			return fmt.Sprintf("dead money: spread %+.3f bps/hr, position pays out rather than in", sp), exitBps, measured
		}
		if repay := -p.NetBps() / sp; repay > b.DeadAfterHours {
			return fmt.Sprintf("dead money: %.1fh to repay %.1f bps at %+.3f bps/hr, past the %.0fh limit",
				repay, -p.NetBps(), sp, b.DeadAfterHours), exitBps, measured
		}
	}

	if p.NegativeSettlements >= b.cfg.NegativeSettlementsBeforeExit {
		if held < b.cfg.MinHoldHours {
			return "", exitBps, measured
		}
		if p.NetBps() < 0 {
			// STILL UNDERWATER. Closing here pays the exit to lock in a loss.
			// The spread may recover; the round trip never comes back.
			// REPAID AND REVERSED.
			//
			// Placed AFTER the negative-settlement path deliberately. That path owns
			// the case once the counter has tripped; this one is the faster route to
			// the same conclusion before it does, so NegativeSettlementsBeforeExit
			// stays meaningful rather than becoming dead code.
			//
			// The dead-money test above requires NetBps() < 0, so a position that has
			// already repaid and then reversed falls through it. 1000RATS sat at +22.57
			// bps net with the spread at -0.035 bps/hr on 2026-08-18: repaid, earning
			// nothing, holding one of five slots while 427 candidates a day were
			// refused for AT_CAPACITY.
			//
			// The threshold is expressed in measured quantities rather than a chosen
			// spread floor: how long until the bleed gives back this position's own
			// exit cost.
			//
			//	-0.400 bps/hr against a 20 bps exit ->  50h  -> close
			//	-0.035 bps/hr against the same exit -> 571h  -> noise, hold
			//
			// NetBps already carries the full round trip, so closing banks the figure
			// shown rather than paying an unaccounted exit.
			if b.ReversedBleedHours > 0 && held >= b.cfg.MinHoldHours && p.NetBps() > 0 && exitBps > 0 {
				if sp := p.CurrentSpreadBpsHr(); sp < 0 {
					if h := exitBps / -sp; h <= b.ReversedBleedHours {
						return fmt.Sprintf("repaid and reversed: net %+.2f bps banked, bleeding %+.3f bps/hr, %.0fh to give back the exit",
							p.NetBps(), sp, h), exitBps, measured
					}
				}
			}

			return "", exitBps, measured
		}
		return fmt.Sprintf("spread gone: %d consecutive settlements at %+.4f bps/hr, net %+.2f bps",
			p.NegativeSettlements, p.CurrentSpreadBpsHr(), p.NetBps()), exitBps, measured
	}

	return "", exitBps, measured
}

// Snapshot returns copies of the open and closed positions.
func (b *Book) Snapshot() (open, closed []Position) {
	for _, p := range b.state.Open {
		open = append(open, *p)
	}
	for _, p := range b.state.Closed {
		closed = append(closed, *p)
	}
	sort.Slice(open, func(i, j int) bool { return open[i].NetBps() < open[j].NetBps() })
	sort.Slice(closed, func(i, j int) bool { return closed[i].ClosedAt.After(closed[j].ClosedAt) })
	return open, closed
}

// Retire closes every open position and records why, so a superseded book
// leaves a complete record instead of positions written as "open" forever.
//
// Four books were retired between 5 and 20 August by renaming their files. None
// closed its positions first, so 22 of them are still written as open: never
// marked, never exited, never reconciled. The dashboard rendered one of those
// dead books as live for 38 hours because nothing in the file said otherwise.
//
// DATING. Each position is closed at its LAST OBSERVATION, not at now. A book
// whose writer stopped 38 hours ago has not been watching for 38 hours, and
// dating the close to the present would inflate every held-hours figure and
// imply a mark that was never taken.
//
// EXIT COST. The last measured exit is used, carrying its own measured flag. No
// exit was actually paid because nothing was traded -- but the position still
// owed its round trip, and recording zero would flatter the result.
func (b *Book) Retire(reason string, now time.Time) (int, error) {
	if reason == "" {
		return 0, fmt.Errorf("crossvenue: retiring a book needs a reason; an unexplained archive is the problem, not the fix")
	}
	closed := 0
	for _, p := range b.state.Open {
		at := p.LastObservedAt
		stale := ""
		if at.IsZero() {
			at = p.OpenedAt
			stale = ", never marked after entry"
		} else if gap := now.Sub(at); gap > time.Hour {
			stale = fmt.Sprintf(", last marked %s before retirement", gap.Truncate(time.Minute))
		}
		p.Close(fmt.Sprintf("book retired: %s%s", reason, stale),
			p.EffectiveExitBps(), p.ExitWatch.Measured, at)
		b.state.Closed = append(b.state.Closed, p)
		closed++
	}
	b.state.Open = map[string]*Position{}
	return closed, b.save()
}

func (b *Book) save() error {
	if b.dir == "" {
		return nil
	}
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return fmt.Errorf("crossvenue: %w", err)
	}
	raw, err := json.MarshalIndent(b.state, "", "  ")
	if err != nil {
		return fmt.Errorf("crossvenue: encoding state: %w", err)
	}
	tmp := b.path() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("crossvenue: writing %s: %w", tmp, err)
	}
	// Atomic replace: a crash mid-write must not leave a truncated book.
	if err := os.Rename(tmp, b.path()); err != nil {
		return fmt.Errorf("crossvenue: replacing %s: %w", b.path(), err)
	}
	return nil
}
