package funding

import (
	"strings"
	"testing"
	"time"
)

// TestUnmeasuredFallsBackNeverToZero.
//
// The most dangerous wrong answer this file could give is "free to exit". An
// ExitWatch that has never been fed must defer to the caller's estimate, not
// report a cheap exit.
func TestUnmeasuredFallsBackNeverToZero(t *testing.T) {
	var w ExitWatch

	if got := w.EffectiveExitBps(16); got != 16 {
		t.Fatalf("unmeasured watch returned %v instead of the fallback 16", got)
	}
	if w.Measured {
		t.Fatal("zero value reports itself as measured")
	}
	if closing, _ := w.DoorClosing(2, 3); closing {
		t.Fatal("unmeasured watch claimed the door is closing")
	}
}

// TestZeroReadingIsIgnored. A venue returning 0 means unreadable, not free.
func TestZeroReadingIsIgnored(t *testing.T) {
	var w ExitWatch
	now := time.Now().UTC()

	w.Observe(0, now)
	w.Observe(-5, now)

	if w.Measured {
		t.Fatal("a zero/negative reading was recorded as a measurement")
	}
	if got := w.EffectiveExitBps(16); got != 16 {
		t.Fatalf("got %v, want the fallback", got)
	}
}

// TestLiveReadingOverridesTheEstimate. This is the fix: once the exit has been
// measured, PnL must use the measurement.
func TestLiveReadingOverridesTheEstimate(t *testing.T) {
	var w ExitWatch
	w.Observe(48, time.Now().UTC())

	if got := w.EffectiveExitBps(16); got != 48 {
		t.Fatalf("got %v, want the live 48 -- the stop-loss is still blind", got)
	}
	if d := w.Deterioration(16); d != 3 {
		t.Fatalf("deterioration %v, want 3x", d)
	}
}

// TestFirstReadingInventsNoDrift.
func TestFirstReadingInventsNoDrift(t *testing.T) {
	var w ExitWatch
	w.Observe(16, time.Now().UTC())

	if w.DriftBpsPerDay != 0 {
		t.Fatalf("derived a drift of %v from a single point", w.DriftBpsPerDay)
	}
	if w.ConsecutiveDrift != 0 {
		t.Fatalf("counted %d adverse windows from one reading", w.ConsecutiveDrift)
	}
}

// TestPollNoiseDoesNotBecomeDrift.
//
// Polls are 5 minutes apart. Without the hourly baseline, a 0.1 bps twitch
// would divide by 1/288th of a day and read as 29 bps/day of deterioration.
func TestPollNoiseDoesNotBecomeDrift(t *testing.T) {
	var w ExitWatch
	base := time.Now().UTC()
	w.Observe(16, base)

	for i := 1; i <= 10; i++ {
		w.Observe(16.1, base.Add(time.Duration(i)*5*time.Minute))
	}

	if w.ConsecutiveDrift != 0 {
		t.Fatalf("five-minute noise produced %d adverse windows", w.ConsecutiveDrift)
	}
	if w.DriftBpsPerDay != 0 {
		t.Fatalf("five-minute noise produced %v bps/day of drift", w.DriftBpsPerDay)
	}
}

// TestSustainedDeteriorationClosesTheDoor.
//
// The RPL shape: exit cost climbing steadily while funding stays modest.
func TestSustainedDeteriorationClosesTheDoor(t *testing.T) {
	var w ExitWatch
	base := time.Now().UTC()
	w.Observe(16, base)

	// +4 bps every hour = ~96 bps/day of deterioration.
	cost := 16.0
	for i := 1; i <= 5; i++ {
		cost += 4
		w.Observe(cost, base.Add(time.Duration(i)*time.Hour))
	}

	if w.ConsecutiveDrift < 3 {
		t.Fatalf("only %d adverse windows after five hours of steady worsening", w.ConsecutiveDrift)
	}
	if w.DriftBpsPerDay <= 0 {
		t.Fatalf("drift %v, want positive", w.DriftBpsPerDay)
	}

	closing, why := w.DoorClosing(2.4, 3) // earning 2.4 bps/day
	if !closing {
		t.Fatalf("door not flagged closing at %.2f bps/day drift against 2.4 earned", w.DriftBpsPerDay)
	}
	if !strings.Contains(why, "Waiting costs more than staying earns") {
		t.Fatalf("reason does not state the trade-off: %s", why)
	}
}

// TestEarningFasterThanTheDoorClosesHolds.
//
// Deterioration alone is not a reason to leave. If funding still outpaces it,
// staying is correct -- otherwise this becomes another churn rule.
func TestEarningFasterThanTheDoorClosesHolds(t *testing.T) {
	var w ExitWatch
	base := time.Now().UTC()
	w.Observe(16, base)

	cost := 16.0
	for i := 1; i <= 5; i++ {
		cost += 0.5 // ~12 bps/day
		w.Observe(cost, base.Add(time.Duration(i)*time.Hour))
	}

	if closing, why := w.DoorClosing(50, 3); closing { // earning 50 bps/day
		t.Fatalf("left a position earning 50 bps/day over %v bps/day of drift: %s", w.DriftBpsPerDay, why)
	}
}

// TestImprovingBookResetsTheCount. A book that recovers must clear the
// trend, not carry an old grudge into a new regime.
func TestImprovingBookResetsTheCount(t *testing.T) {
	var w ExitWatch
	base := time.Now().UTC()
	w.Observe(16, base)

	cost := 16.0
	for i := 1; i <= 4; i++ {
		cost += 4
		w.Observe(cost, base.Add(time.Duration(i)*time.Hour))
	}
	if w.ConsecutiveDrift == 0 {
		t.Fatal("fixture never accumulated a trend")
	}

	// Book recovers.
	w.Observe(10, base.Add(5*time.Hour))
	if w.ConsecutiveDrift != 0 {
		t.Fatalf("improvement left %d adverse windows on the counter", w.ConsecutiveDrift)
	}
	if closing, _ := w.DoorClosing(2, 3); closing {
		t.Fatal("closed a position whose exit book had recovered")
	}
}

// TestWorstIsRemembered. The peak is what a stress moment would have cost and
// it is gone from the live reading by the time anyone looks at it.
func TestWorstIsRemembered(t *testing.T) {
	var w ExitWatch
	base := time.Now().UTC()

	w.Observe(16, base)
	w.Observe(92, base.Add(time.Hour))
	w.Observe(18, base.Add(2*time.Hour))

	if w.WorstExitCostBps != 92 {
		t.Fatalf("worst %v, want 92", w.WorstExitCostBps)
	}
	if w.LiveExitCostBps != 18 {
		t.Fatalf("live %v, want 18", w.LiveExitCostBps)
	}
}

// TestNegativeFundingWithRisingExitCloses. A position that is PAYING funding
// and getting harder to leave has nothing to wait for.
func TestNegativeFundingWithRisingExitCloses(t *testing.T) {
	var w ExitWatch
	base := time.Now().UTC()
	w.Observe(16, base)

	cost := 16.0
	for i := 1; i <= 4; i++ {
		cost += 1
		w.Observe(cost, base.Add(time.Duration(i)*time.Hour))
	}

	if closing, _ := w.DoorClosing(-3, 3); !closing {
		t.Fatalf("held a position paying funding into a worsening exit (drift %v)", w.DriftBpsPerDay)
	}
}

// TestOneBadWindowIsNotATrend.
func TestOneBadWindowIsNotATrend(t *testing.T) {
	var w ExitWatch
	base := time.Now().UTC()

	w.Observe(16, base)
	w.Observe(80, base.Add(time.Hour)) // one violent window

	if closing, _ := w.DoorClosing(2, 3); closing {
		t.Fatal("closed on a single adverse window")
	}
}
