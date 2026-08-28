package crossvenue

import (
	"fmt"
	"math"
	"time"
)

// THE SETTLEMENT CALENDAR
//
// WHY A CONTINUOUS BREAK-EVEN IS THE WRONG QUESTION
//
// round_trip / spread assumes funding trickles in. It does not. It lands as
// LUMPS, on two clocks that do not line up, and the position's net walks a
// jagged path between them.
//
// Measured 2026-08-12. A position opened 22:15 long Hyperliquid (hourly) and
// short Binance (4-hourly), true spread +5.03 bps/hr, round trip 24.4:
//
//	22:15  open                                     net  -24.4
//	23:00  hyperliquid settles   +36.75             net  +12.4
//	00:00  hyperliquid +36.75, BINANCE -126.88      net  -77.8   <-- STOPPED OUT
//
// Continuous break-even said 4.9 hours and the position was dead in 1.75. It
// held for one Hyperliquid hour, then took a FULL FOUR HOURS of Binance
// funding having collected two hours of Hyperliquid's.
//
// The same trade opened at 00:05 instead:
//
//	01:00 .. 03:00   three hyperliquid settlements  net  +85.9
//	04:00  hyperliquid +36.75, binance -126.88      net   -4.3   <-- survives
//	05:00  hyperliquid +36.75                       net  +32.5   <-- profitable
//
// Identical pair, identical rates, identical costs. The only difference is
// WHERE IN THE SLOW VENUE'S CYCLE the position opened.
//
// So the gate simulates the calendar and asks two questions the continuous
// formula cannot:
//
//	1. How far underwater does this go before it turns?
//	2. Does it turn at all inside the maximum hold?
//
// A path that breaches the stop loss on its way to profit is not a profitable
// trade. It is a realised loss.

// PlanPoint is one settlement in the simulated path.
//
// CollectedBps is funding alone; NetBps also carries the round trip. They are
// kept apart so a divergence can be attributed: funding that did not arrive is
// a different failure from an exit that got more expensive.
type PlanPoint struct {
	AtHours      float64 `json:"at_hours"`
	CollectedBps float64 `json:"collected_bps"`
	NetBps       float64 `json:"net_bps"`
	// Legs is "L", "S" or "LS" -- which side settled at this instant.
	Legs string `json:"legs"`
}

// SettlementPlan is the simulated forward path of one candidate.
type SettlementPlan struct {
	// WorstNetBps is the lowest net the position reaches. This is the number
	// the stop loss will actually see.
	WorstNetBps float64

	// WorstAtHours is when that low point occurs.
	WorstAtHours float64

	// BreakEvenHours is when net first turns non-negative. Valid only when
	// ReachesBreakEven is true.
	BreakEvenHours   float64
	ReachesBreakEven bool

	// NetAtHorizonBps is where the position stands at the end of the simulated
	// window.
	NetAtHorizonBps float64

	// FirstSlowSettleHours is when the slower leg next pays or charges. The
	// lump lands here.
	FirstSlowSettleHours float64
	SlowVenue            string
	SlowIntervalHours    float64

	Events int
	Ok     bool

	// Path is the settlement-by-settlement forecast, FROZEN AT ENTRY.
	//
	// Stored rather than recomputed on demand. A path recalculated later would
	// use the rates prevailing then, which is not a prediction -- it is
	// hindsight dressed as one, and it could never be wrong.
	Path []PlanPoint `json:"path,omitempty"`
}

// simulate walks the settlement calendar forward.
//
// hoursToLongSettle and hoursToShortSettle are taken from each VENUE'S OWN
// next-funding timestamp. Nothing here reimplements a venue schedule; a
// hardcoded calendar that drifts would produce a confident wrong answer, which
// is the failure this whole file exists to prevent.
func simulatePlan(c Candidate, entryCostBps, exitCostBps float64,
	hoursToLongSettle, hoursToShortSettle, horizonHours float64) SettlementPlan {

	var p SettlementPlan

	if c.LongIntervalHours <= 0 || c.ShortIntervalHours <= 0 || horizonHours <= 0 {
		return p
	}
	if hoursToLongSettle < 0 || hoursToShortSettle < 0 {
		return p
	}

	p.SlowVenue, p.SlowIntervalHours, p.FirstSlowSettleHours = c.ShortVenue, c.ShortIntervalHours, hoursToShortSettle
	if c.LongIntervalHours > c.ShortIntervalHours {
		p.SlowVenue, p.SlowIntervalHours, p.FirstSlowSettleHours = c.LongVenue, c.LongIntervalHours, hoursToLongSettle
	}

	// Sign convention, as in position.go: we are LONG the long venue, so a
	// positive rate is money out; SHORT the short venue, so a positive rate is
	// money in.
	// PROJECT FROM SETTLED RATES WHERE THEY EXIST.
	//
	// Deciding entry on settled funding while projecting repayment from
	// predictions would gate on one signal and size on another. Worse, a
	// candidate re-oriented to follow settled funding carries predicted rates
	// that now point the WRONG way, so the plan would show a guaranteed loss
	// and refuse a trade the evidence supports.
	longRate, shortRate := c.LongBpsHr, c.ShortBpsHr
	if c.SettledKnown {
		longRate, shortRate = c.SettledLongBpsHr, c.SettledShortBpsHr
	}
	longPerSettle := -longRate * c.LongIntervalHours
	shortPerSettle := shortRate * c.ShortIntervalHours

	cost := entryCostBps + exitCostBps
	net := -cost

	p.WorstNetBps = net
	p.WorstAtHours = 0

	tLong, tShort := hoursToLongSettle, hoursToShortSettle
	collected := 0.0

	// Bounded: a horizon of 720 hours against an hourly leg is 720 events, and
	// the loop must terminate even if a venue reports a nonsense interval.
	for i := 0; i < 4000; i++ {
		t := math.Min(tLong, tShort)
		if t > horizonHours {
			break
		}

		// Both legs settling in the same instant is the dangerous case and it
		// must be handled as one event, not two.
		if math.Abs(tLong-t) < 1e-9 {
			collected += longPerSettle
			tLong += c.LongIntervalHours
		}
		if math.Abs(tShort-t) < 1e-9 {
			collected += shortPerSettle
			tShort += c.ShortIntervalHours
		}

		net = collected - cost
		p.Events++

		// Cap the stored path. An hourly leg over a 168-hour horizon is 168
		// points; past that the forecast is fiction anyway and the position
		// file should not carry it.
		if len(p.Path) < 200 {
			legs := ""
			if math.Abs(tLong-c.LongIntervalHours-t) < 1e-9 {
				legs += "L"
			}
			if math.Abs(tShort-c.ShortIntervalHours-t) < 1e-9 {
				legs += "S"
			}
			p.Path = append(p.Path, PlanPoint{
				AtHours: t, CollectedBps: collected, NetBps: net, Legs: legs,
			})
		}

		if net < p.WorstNetBps {
			p.WorstNetBps = net
			p.WorstAtHours = t
		}
		if !p.ReachesBreakEven && net >= 0 {
			p.ReachesBreakEven = true
			p.BreakEvenHours = t
		}
	}

	p.NetAtHorizonBps = net
	p.Ok = p.Events > 0
	return p
}

// hoursUntil converts a venue's next-funding timestamp into hours from now.
//
// A timestamp in the past means the venue's clock has rolled and our snapshot
// is stale; it is clamped to zero rather than producing a negative wait, which
// would make the simulation credit a settlement that already happened.
// ExecutionLeadHours is the minimum time before a settlement at which an entry
// may still count on receiving it. Fifteen minutes against 4h and 8h intervals
// costs roughly 6% of entry opportunities and removes a whole class of
// phantom income.
const ExecutionLeadHours = 0.25

func hoursUntil(nextMs int64, now time.Time) (float64, bool) {
	if nextMs <= 0 {
		return 0, false
	}
	d := time.UnixMilli(nextMs).UTC().Sub(now).Hours()
	if d < ExecutionLeadHours {
		// STALE, OR TOO CLOSE TO RELY ON. REFUSE -- DO NOT CLAMP.
		//
		// Clamping a past timestamp to zero told simulatePlan that funding
		// arrives immediately, so a plan could be built on a settlement that
		// had ALREADY HAPPENED and would never be received. Found by external
		// code review 2026-08-19.
		//
		// The same refusal covers a settlement that is imminent but not yet
		// past: both legs must be submitted, filled and registered by the venue
		// before its funding snapshot. Counting a settlement 90 seconds out is
		// crediting income the position will not receive.
		//
		// The caller's response should be to refresh both legs' calendars, not
		// to substitute a guess.
		return 0, false
	}
	return d, true
}

// Describe renders the plan for a log line or a refusal.
func (p SettlementPlan) Describe() string {
	be := "never inside the horizon"
	if p.ReachesBreakEven {
		be = fmt.Sprintf("%.2fh", p.BreakEvenHours)
	}
	return fmt.Sprintf(
		"worst %.2f bps at %.2fh, break-even %s, %d settlements; "+
			"slow leg %s every %.0fh, next in %.2fh",
		p.WorstNetBps, p.WorstAtHours, be, p.Events,
		p.SlowVenue, p.SlowIntervalHours, p.FirstSlowSettleHours)
}
