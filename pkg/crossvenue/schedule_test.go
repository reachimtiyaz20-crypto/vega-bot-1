package crossvenue

import (
	"strings"
	"testing"
	"time"
)

// lossOne is the position that closed at -82.34 bps on 2026-08-12 00:00.
//
// Opened 22:15:24 long Hyperliquid / short Binance.
// Hyperliquid -36.75 bps/hr on a 1h clock, Binance -31.72 bps/hr on a 4h clock.
// Next settlements: hyperliquid 23:00 (0.744h away), binance 00:00 (1.744h).
// Entry cost 12.19, exit 16.76.
func lossOne() Candidate {
	return Candidate{
		Coin:               "KAITO",
		LongVenue:          "hyperliquid",
		ShortVenue:         "binance",
		LongBpsHr:          -36.75,
		ShortBpsHr:         -31.72,
		LongIntervalHours:  1,
		ShortIntervalHours: 4,
	}
}

// TestReplaysTheFirstRealLoss.
//
// The whole point of the file. This must reproduce the -82 bps the live book
// actually recorded, from the calendar alone.
func TestReplaysTheFirstRealLoss(t *testing.T) {
	p := simulatePlan(lossOne(), 12.19, 16.76, 0.744, 1.744, 72)
	if !p.Ok {
		t.Fatal("simulation refused a well-formed candidate")
	}

	// 22:15 open        net -28.95
	// 23:00 hl  +36.75  net  +7.80
	// 00:00 hl  +36.75, binance -126.88 -> net -82.33
	if p.WorstNetBps > -80 || p.WorstNetBps < -85 {
		t.Fatalf("worst %.2f bps, want about -82.3 (the live book recorded -82.34)", p.WorstNetBps)
	}
	if p.WorstAtHours < 1.7 || p.WorstAtHours > 1.8 {
		t.Fatalf("worst at %.2fh, want about 1.74", p.WorstAtHours)
	}
	if p.SlowVenue != "binance" || p.SlowIntervalHours != 4 {
		t.Fatalf("slow leg %s/%.0fh, want binance/4h", p.SlowVenue, p.SlowIntervalHours)
	}
	if p.WorstNetBps > -60 {
		t.Fatalf("worst %.2f does not breach a -60 stop; the gate would have let this through",
			p.WorstNetBps)
	}
}

// TestTheSameTradeAfterTheSettlementSurvives.
//
// Identical pair, identical rates, identical costs. The ONLY change is opening
// just after the Binance settlement instead of 1.75 hours before it.
func TestTheSameTradeAfterTheSettlementSurvives(t *testing.T) {
	// 00:05 open: hyperliquid settles 01:00 (0.917h), binance 04:00 (3.917h).
	p := simulatePlan(lossOne(), 12.19, 16.76, 0.917, 3.917, 72)
	if !p.Ok {
		t.Fatal("simulation refused")
	}
	if p.WorstNetBps <= -60 {
		t.Fatalf("worst %.2f bps still breaches the stop; entry timing was supposed to fix it",
			p.WorstNetBps)
	}
	if !p.ReachesBreakEven {
		t.Fatal("never breaks even, so the pair is not tradable at any entry time")
	}
	if p.BreakEvenHours > 6 {
		t.Fatalf("break-even %.2fh, want under 6", p.BreakEvenHours)
	}
}

// TestSimultaneousSettlementIsOneEvent.
//
// A 1h and a 4h clock coincide every four hours. Counting that instant twice
// would double one leg's payment at exactly the moment the lump lands.
func TestSimultaneousSettlementIsOneEvent(t *testing.T) {
	c := lossOne()
	// Both settle at t=1.0 exactly.
	p := simulatePlan(c, 0, 0, 1.0, 1.0, 4.5)

	// t=1: hl +36.75 and binance -126.88 together  -> -90.13
	// t=2: hl +36.75                               -> -53.38
	// t=3: hl +36.75                               -> -16.63
	// t=4: hl +36.75                               -> +20.12
	if p.Events != 4 {
		t.Fatalf("%d events in 4.5 hours, want 4", p.Events)
	}
	if p.NetAtHorizonBps < 19 || p.NetAtHorizonBps > 21 {
		t.Fatalf("net %.2f at horizon, want about 20.12", p.NetAtHorizonBps)
	}
}

// TestNoCalendarIsRefusedNotAssumed.
func TestNoCalendarIsRefusedNotAssumed(t *testing.T) {
	if _, ok := hoursUntil(0, time.Now().UTC()); ok {
		t.Fatal("accepted a missing next-funding timestamp")
	}
	// A stale timestamp is REFUSED, not clamped.
	//
	// This previously asserted the opposite, with a comment claiming the clamp
	// avoided "crediting a settlement that has already happened". It did the
	// reverse: zero hours with ok=true tells simulatePlan the settlement is
	// arriving immediately. The test was guarding the defect while describing
	// the correct principle.
	if _, ok := hoursUntil(time.Now().Add(-3*time.Hour).UnixMilli(), time.Now().UTC()); ok {
		t.Fatal("accepted a stale next-funding timestamp")
	}
	// A settlement inside the execution lead window is refused for the same
	// reason: it cannot be relied upon to arrive.
	if _, ok := hoursUntil(time.Now().Add(5*time.Minute).UnixMilli(), time.Now().UTC()); ok {
		t.Fatal("accepted a settlement inside the execution lead window")
	}
	// Comfortably ahead is usable.
	if h, ok := hoursUntil(time.Now().Add(3*time.Hour).UnixMilli(), time.Now().UTC()); !ok || h < 2.9 {
		t.Fatalf("refused a usable timestamp: %v ok=%v", h, ok)
	}
}

// TestGateRefusesTheLosingEntry, end to end through assess.
func TestGateRefusesTheLosingEntry(t *testing.T) {
	b := testBook(t, nil)
	now := time.Now().UTC()

	c := lossOne()
	c.LongNextFundingMs = now.Add(45 * time.Minute).UnixMilli()   // 0.75h
	c.ShortNextFundingMs = now.Add(105 * time.Minute).UnixMilli() // 1.75h
	c.LongMeasured, c.ShortMeasured = true, true
	c.LongEntrySlipBps, c.ShortEntrySlipBps = 1.3, 1.4
	c.LongExitSlipBps, c.ShortExitSlipBps = 3.6, 3.7
	c.LongDepthUSD, c.ShortDepthUSD = 5000, 5000
	c.VolUSD = 50_000_000

	a := b.Assess(c, now)
	if a.Viable {
		t.Fatalf("accepted the entry that lost $3.29 in the live book (plan: %s)", a.Plan.Describe())
	}
	if a.Gate != GateStopsOutBeforeProfit {
		t.Fatalf("refused as %s: %s\nwant %s", a.Gate, a.Reason, GateStopsOutBeforeProfit)
	}
	if !strings.Contains(a.Reason, "just after that settlement") {
		t.Fatalf("refusal does not say what to do instead: %s", a.Reason)
	}
}

// TestEveryElapsedIntervalIsBooked.
//
// Measured 2026-08-13: a position held 23.03 hours had booked 19 hourly
// settlements instead of 23. The detector fired once per poll regardless of
// how many intervals had passed, and every dropped one was on the leg that
// pays -- so the position read ~38 bps richer than reality.
func TestEveryElapsedIntervalIsBooked(t *testing.T) {
	b := testBook(t, nil)
	if _, err := b.Update(fixedNow, []Candidate{goodCandidate()}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Three hours pass in one poll: the hourly leg's next-funding timestamp
	// has advanced by three intervals.
	c := goodCandidate()
	c.LongNextFundingMs = fixedNow.Add(3*time.Hour + 30*time.Minute).UnixMilli()
	if _, err := b.Update(fixedNow.Add(3*time.Hour), []Candidate{c}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	open, _ := b.Snapshot()
	p := open[0]
	if p.LongSettlements != 3 {
		t.Fatalf("booked %d settlements across a 3-hour gap, want 3", p.LongSettlements)
	}
	if p.MissedSettlements != 2 {
		t.Fatalf("missed counter %d, want 2", p.MissedSettlements)
	}
	// -24 bps/hr on a 1h clock -> +24 to a long, three times.
	if !near(p.LongLegBps, 72, 1e-9) {
		t.Fatalf("accrued %.4f bps, want 72", p.LongLegBps)
	}
}

// TestAbsurdJumpBooksOneNotHundreds.
func TestAbsurdJumpBooksOneNotHundreds(t *testing.T) {
	b := testBook(t, nil)
	if _, err := b.Update(fixedNow, []Candidate{goodCandidate()}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	c := goodCandidate()
	c.LongNextFundingMs = fixedNow.Add(9000 * time.Hour).UnixMilli()
	if _, err := b.Update(fixedNow.Add(time.Hour), []Candidate{c}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	open, _ := b.Snapshot()
	if n := open[0].LongSettlements; n != 1 {
		t.Fatalf("booked %d settlements from a nonsense timestamp, want 1", n)
	}
}
