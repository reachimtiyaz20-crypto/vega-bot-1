package crossvenue

import (
	"strings"
	"testing"
	"time"
)

// The ONG case, 2026-08-21. Every funding-based rule was satisfied -- net stood
// at +175 bps -- while price divergence took 393 bps out from under it. The
// position closed at -217 all-in. This is the exact situation the other exits
// are designed not to see, so it is the test that matters.
func TestBasisStopFiresWhileFundingLooksProfitable(t *testing.T) {
	b := testBook(t, nil)
	now := fixedNow

	p := heldPosition(6*time.Hour, now)
	p.LongLegBps = 200 // net ~ +174: stop loss, dead money and door-closing all silent
	p.BasisMeasured = true
	p.EntryBasisBps = 530.7
	p.LastBasisBps = 137.9 // drift -392.8

	reason, _, _ := b.exitReason(p, Candidate{}, now)
	if !strings.Contains(reason, "basis stop") {
		t.Fatalf("basis stop silent at %.1f bps drift with funding net %+.1f: reason %q",
			p.LastBasisBps-p.EntryBasisBps, p.NetBps(), reason)
	}
	if !strings.Contains(reason, "funding net") {
		t.Errorf("reason must state the funding net, since that is what makes it surprising: %s", reason)
	}
}

// The UNITREE case. It drifted -63 bps and finished +39 all-in. A threshold
// tight enough to catch it would be closing profitable positions.
func TestBasisStopHoldsInsideTheThreshold(t *testing.T) {
	b := testBook(t, nil)
	now := fixedNow

	p := heldPosition(6*time.Hour, now)
	p.LongLegBps = 200
	p.BasisMeasured = true
	p.EntryBasisBps = 72.0
	p.LastBasisBps = 9.0 // drift -63.0

	reason, _, _ := b.exitReason(p, Candidate{}, now)
	if strings.Contains(reason, "basis stop") {
		t.Fatalf("basis stop closed a position that went on to make money: %s", reason)
	}
}

// Unmeasured basis is not zero basis. Acting on an unmeasured quantity is the
// defect class this project exists because of.
func TestBasisStopSilentWhenBasisWasNeverMeasured(t *testing.T) {
	b := testBook(t, nil)
	now := fixedNow

	p := heldPosition(6*time.Hour, now)
	p.LongLegBps = 200
	p.BasisMeasured = false
	p.EntryBasisBps = 530.7
	p.LastBasisBps = 0 // would read as -530 drift if it counted

	reason, _, _ := b.exitReason(p, Candidate{}, now)
	if strings.Contains(reason, "basis stop") {
		t.Fatalf("acted on basis that was never measured: %s", reason)
	}
}

// A drift in our FAVOUR must never trigger the stop.
func TestBasisStopIgnoresFavourableDrift(t *testing.T) {
	b := testBook(t, nil)
	now := fixedNow

	p := heldPosition(6*time.Hour, now)
	p.LongLegBps = 200
	p.BasisMeasured = true
	p.EntryBasisBps = -200.0
	p.LastBasisBps = 200.0 // drift +400, entirely in our favour

	reason, _, _ := b.exitReason(p, Candidate{}, now)
	if strings.Contains(reason, "basis stop") {
		t.Fatalf("basis stop fired on a favourable move: %s", reason)
	}
}
