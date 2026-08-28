package crossvenue

import (
	"strings"
	"testing"
	"time"
)

// A retired book must leave no position written as "open". That is the whole
// defect: four books were archived by renaming, and 22 positions are still
// recorded as open because nothing closed them.
func TestRetireClosesEveryOpenPosition(t *testing.T) {
	b := testBook(t, nil)
	now := fixedNow

	for i := 0; i < 3; i++ {
		p := heldPosition(6*time.Hour, now)
		p.Coin = string(rune('A' + i))
		b.state.Open[p.Key()] = p
	}
	if open, _ := b.Snapshot(); len(open) != 3 {
		t.Fatalf("fixture: %d open, want 3", len(open))
	}

	n, err := b.Retire("superseded by the union book", now)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if n != 3 {
		t.Errorf("closed %d, want 3", n)
	}
	open, closed := b.Snapshot()
	if len(open) != 0 {
		t.Errorf("%d positions still open after retirement", len(open))
	}
	if len(closed) != 3 {
		t.Errorf("%d closed, want 3", len(closed))
	}
	for _, p := range closed {
		if !strings.Contains(p.CloseReason, "book retired") {
			t.Errorf("%s closed without saying why: %q", p.Coin, p.CloseReason)
		}
		if !strings.Contains(p.CloseReason, "superseded by the union book") {
			t.Errorf("%s lost the operator's reason: %q", p.Coin, p.CloseReason)
		}
	}
}

// Dating matters. A book whose writer stopped 38 hours ago was not watching for
// 38 hours; closing at "now" would inflate held hours and imply a mark that was
// never taken.
func TestRetireDatesTheCloseToTheLastObservation(t *testing.T) {
	b := testBook(t, nil)
	now := fixedNow

	p := heldPosition(6*time.Hour, now)
	lastSeen := now.Add(-38 * time.Hour)
	p.LastObservedAt = lastSeen
	b.state.Open[p.Key()] = p

	if _, err := b.Retire("writer stopped", now); err != nil {
		t.Fatalf("retire: %v", err)
	}
	_, closed := b.Snapshot()
	if len(closed) != 1 {
		t.Fatalf("closed %d, want 1", len(closed))
	}
	got := closed[0]
	if !got.ClosedAt.Equal(lastSeen) {
		t.Errorf("closed at %v, want the last observation %v", got.ClosedAt, lastSeen)
	}
	if got.ClosedAt.Equal(now) {
		t.Error("dated the close to retirement time, inflating held hours")
	}
	if !strings.Contains(got.CloseReason, "last marked") {
		t.Errorf("staleness not recorded in the reason: %q", got.CloseReason)
	}
}

// An unexplained archive is the problem, not the fix.
func TestRetireRefusesWithoutAReason(t *testing.T) {
	b := testBook(t, nil)
	p := heldPosition(6*time.Hour, fixedNow)
	b.state.Open[p.Key()] = p

	if _, err := b.Retire("", fixedNow); err == nil {
		t.Fatal("retired a book with no stated reason")
	}
	if open, _ := b.Snapshot(); len(open) != 1 {
		t.Errorf("a refused retirement must change nothing: %d open", len(open))
	}
}
