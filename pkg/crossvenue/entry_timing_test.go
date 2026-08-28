package crossvenue

import (
	"strings"
	"testing"
)

// BLUAI, 2026-08-18. Round trip 63.5 bps against a -60 stop: the position was
// past the floor the instant it opened and closed 10 minutes later having
// collected nothing. The stop loss was not wrong to fire; the entry was wrong
// to happen.
func TestRefusesAPositionStoppedOnArrival(t *testing.T) {
	b := testBook(t, nil)
	// BLUAI's actual numbers: entry 28.6 (inside the 40 bps entry cap) and exit
	// 34.9, for a 63.5 round trip. The existing entry cap cannot catch this --
	// it only looks at the entry half.
	c := settledCandidate(30.0) // spread rich enough that nothing else objects
	c.LongEntrySlipBps, c.ShortEntrySlipBps = 9.5, 9.5
	c.LongExitSlipBps, c.ShortExitSlipBps = 12.7, 12.7

	a := b.assess(c, fixedNow)
	if a.Gate != GateStoppedOnArrival {
		t.Fatalf("opened a position already past its own stop loss: gate %s (%s)", a.Gate, a.Reason)
	}
	if !strings.Contains(a.Reason, "before it could collect") {
		t.Errorf("refusal does not explain the mechanism: %s", a.Reason)
	}
}

// A cheap round trip must still be allowed through this gate.
func TestAllowsAPositionThatSurvivesItsOwnOpening(t *testing.T) {
	b := testBook(t, nil)
	c := settledCandidate(30.0)
	c.LongEntrySlipBps, c.ShortEntrySlipBps = 2, 2
	c.LongExitSlipBps, c.ShortExitSlipBps = 2, 2 // round trip ~ 30 bps, inside -60

	a := b.assess(c, fixedNow)
	if a.Gate == GateStoppedOnArrival {
		t.Fatalf("refused a position whose round trip is well inside the stop: %s", a.Reason)
	}
}
