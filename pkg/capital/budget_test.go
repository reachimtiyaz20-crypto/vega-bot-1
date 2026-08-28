package capital

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func newTestLedger(t *testing.T, principal, reserve float64, path string) *Ledger {
	t.Helper()
	l, err := NewLedger(Config{
		Name: "test", Service: "vega", Principal: principal, ReserveFrac: reserve,
	}, path)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	return l
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// The headline claim of this package: a $400 book commits at most $400 in
// total, not $400 per leg. If this test ever fails the package has no purpose.
func TestPrincipalIsTotalNotPerLeg(t *testing.T) {
	l := newTestLedger(t, 400, 0, "")

	// Four cash-and-carry positions at $50 notional, unlevered. Each locks
	// $100: $50 of spot owned outright plus $50 of margin against the short.
	perPos, err := CapitalForNotional(CashAndCarry, 50, 1, false)
	if err != nil {
		t.Fatalf("CapitalForNotional: %v", err)
	}
	if !near(perPos, 100) {
		t.Fatalf("capital for $50 unlevered cash-and-carry = $%.2f, want $100", perPos)
	}

	for i := 0; i < 4; i++ {
		if err := l.Hold(fmt.Sprintf("p%d", i), perPos, 50, CashAndCarry, "BTC", t0); err != nil {
			t.Fatalf("hold %d rejected: %v", i, err)
		}
	}
	if free := l.Free(); !near(free, 0) {
		t.Fatalf("after 4 positions free = $%.2f, want $0", free)
	}

	// The fifth must be refused. Before this package it would have opened, and
	// the book would have carried $500 while reporting $400.
	err = l.Hold("p4", perPos, 50, CashAndCarry, "ETH", t0)
	if !errors.Is(err, ErrNoCapital) {
		t.Fatalf("fifth position error = %v, want ErrNoCapital", err)
	}
	var se *ShortfallError
	if !errors.As(err, &se) {
		t.Fatalf("error %v does not carry a *ShortfallError", err)
	}
	if !near(se.Want, 100) || !near(se.Free, 0) || !near(se.Allocated, 400) {
		t.Fatalf("shortfall = want $%.2f free $%.2f allocated $%.2f; want 100/0/400",
			se.Want, se.Free, se.Allocated)
	}
}

func TestReserveIsNotAllocatable(t *testing.T) {
	l := newTestLedger(t, 400, 0.20, "")

	if free := l.Free(); !near(free, 320) {
		t.Fatalf("free with 20%% reserve = $%.2f, want $320", free)
	}
	// $320 deployable funds three $100 positions, not four.
	for i := 0; i < 3; i++ {
		if err := l.Hold(fmt.Sprintf("p%d", i), 100, 50, CashAndCarry, "BTC", t0); err != nil {
			t.Fatalf("hold %d rejected: %v", i, err)
		}
	}
	if err := l.Hold("p3", 100, 50, CashAndCarry, "BTC", t0); !errors.Is(err, ErrNoCapital) {
		t.Fatalf("fourth position error = %v, want ErrNoCapital", err)
	}
	// The reserve is still there, untouched, which is the whole point.
	s := l.Snapshot(t0)
	if !near(s.Reserve, 80) {
		t.Fatalf("reserve = $%.2f, want $80", s.Reserve)
	}
	if !near(s.Allocated, 300) {
		t.Fatalf("allocated = $%.2f, want $300", s.Allocated)
	}
}

func TestCapitalForNotional(t *testing.T) {
	cases := []struct {
		name     string
		s        Structure
		notional float64
		lev      float64
		pm       bool
		want     float64
	}{
		// Matches the measured notional/capital ratio of 0.50 without PM.
		{"cash-and-carry unlevered", CashAndCarry, 100, 1, false, 200},
		{"cash-and-carry 5x perp leg", CashAndCarry, 100, 5, false, 120},
		{"cash-and-carry portfolio margin", CashAndCarry, 100, 1, true, 111},
		// Both legs are margin; PM does not help across venues.
		{"cross-venue unlevered", CrossVenue, 100, 1, false, 200},
		{"cross-venue 4x", CrossVenue, 100, 4, false, 50},
		{"cross-venue PM ignored", CrossVenue, 100, 4, true, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CapitalForNotional(c.s, c.notional, c.lev, c.pm)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !near(got, c.want) {
				t.Fatalf("got $%.4f, want $%.4f", got, c.want)
			}
		})
	}
}

func TestCapitalForNotionalRejectsNonsense(t *testing.T) {
	if _, err := CapitalForNotional(CashAndCarry, 0, 1, false); err == nil {
		t.Fatal("zero notional accepted")
	}
	if _, err := CapitalForNotional(CashAndCarry, 100, 0.5, false); err == nil {
		t.Fatal("leverage below 1 accepted")
	}
	if _, err := CapitalForNotional(Structure("spot_perp_dex"), 100, 1, false); err == nil {
		t.Fatal("unknown structure accepted")
	}
}

// A retry after an ambiguous response must not open a second claim on the same
// budget. This is the capital-layer half of task 09.
func TestDuplicateHoldRefused(t *testing.T) {
	l := newTestLedger(t, 400, 0, "")
	if err := l.Hold("p1", 100, 50, CashAndCarry, "BTC", t0); err != nil {
		t.Fatalf("first hold: %v", err)
	}
	err := l.Hold("p1", 100, 50, CashAndCarry, "BTC", t0)
	if !errors.Is(err, ErrDuplicateHold) {
		t.Fatalf("duplicate hold error = %v, want ErrDuplicateHold", err)
	}
	if s := l.Snapshot(t0); s.Positions != 1 || !near(s.Allocated, 100) {
		t.Fatalf("after duplicate: %d positions allocating $%.2f, want 1 / $100",
			s.Positions, s.Allocated)
	}
}

// Closing twice must not invent capital.
func TestDoubleReleaseIsHarmless(t *testing.T) {
	l := newTestLedger(t, 400, 0, "")
	if err := l.Hold("p1", 100, 50, CashAndCarry, "BTC", t0); err != nil {
		t.Fatalf("hold: %v", err)
	}
	usd, ok, err := l.Release("p1")
	if err != nil || !ok || !near(usd, 100) {
		t.Fatalf("first release = $%.2f ok=%v err=%v, want $100/true/nil", usd, ok, err)
	}
	usd, ok, err = l.Release("p1")
	if err != nil || ok || usd != 0 {
		t.Fatalf("second release = $%.2f ok=%v err=%v, want $0/false/nil", usd, ok, err)
	}
	if free := l.Free(); !near(free, 400) {
		t.Fatalf("free after double release = $%.2f, want $400 exactly", free)
	}
}

// The orphan failure that froze 22 positions, prevented one layer lower.
func TestReconcileReleasesOrphanedHolds(t *testing.T) {
	l := newTestLedger(t, 400, 0, "")
	for _, id := range []string{"p1", "p2", "p3"} {
		if err := l.Hold(id, 100, 50, CashAndCarry, "BTC", t0); err != nil {
			t.Fatalf("hold %s: %v", id, err)
		}
	}
	// The book says only p2 is still open; p1 and p3 closed while we were down.
	freed, err := l.Reconcile([]string{"p2"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(freed) != 2 || freed[0] != "p1" || freed[1] != "p3" {
		t.Fatalf("freed = %v, want [p1 p3]", freed)
	}
	if free := l.Free(); !near(free, 300) {
		t.Fatalf("free after reconcile = $%.2f, want $300", free)
	}
	// Idempotent: a second pass with the same truth frees nothing more.
	freed, err = l.Reconcile([]string{"p2"})
	if err != nil || len(freed) != 0 {
		t.Fatalf("second reconcile freed %v err=%v, want none", freed, err)
	}
}

func TestStructurePermissions(t *testing.T) {
	l, err := NewLedger(Config{
		Name: "headline", Principal: 400,
		Structures: []Structure{CashAndCarry},
	}, "")
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	if !l.Permits(CashAndCarry) {
		t.Fatal("headline book refuses cash-and-carry")
	}
	if l.Permits(CrossVenue) {
		t.Fatal("headline book permits cross-venue; the two books must not mix")
	}
	if err := l.Hold("p1", 100, 50, CrossVenue, "BTC", t0); err == nil {
		t.Fatal("cross-venue hold accepted by a cash-and-carry-only book")
	}
	// An empty structure list means no restriction.
	open := newTestLedger(t, 400, 0, "")
	if !open.Permits(CrossVenue) || !open.Permits(CashAndCarry) {
		t.Fatal("unrestricted book refused a structure")
	}
}

// Holds must survive a restart, or one crash leaks the whole budget.
func TestHoldsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capital_headline.json")

	l1 := newTestLedger(t, 400, 0, path)
	if err := l1.Hold("p1", 100, 50, CashAndCarry, "BTC", t0); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := l1.Hold("p2", 150, 75, CashAndCarry, "ETH", t0); err != nil {
		t.Fatalf("hold: %v", err)
	}

	l2 := newTestLedger(t, 400, 0, path)
	s := l2.Snapshot(t0)
	if s.Positions != 2 || !near(s.Allocated, 250) || !near(s.Free, 150) {
		t.Fatalf("after restart: %d positions, allocated $%.2f, free $%.2f; want 2 / $250 / $150",
			s.Positions, s.Allocated, s.Free)
	}
	if s.Holds[0].Symbol != "BTC" || s.Holds[1].Symbol != "ETH" {
		t.Fatalf("hold detail lost across restart: %+v", s.Holds)
	}
}

// A corrupt file must not be read as "nothing is allocated". That would let the
// book believe its full principal is free while positions are open against it.
func TestCorruptFileRefusesRatherThanZeroing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capital_headline.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := NewLedger(Config{Name: "test", Principal: 400}, path)
	if err == nil {
		t.Fatal("corrupt ledger file loaded silently")
	}
}

// Changing principal under open holds must be a deliberate act, not a silent
// re-ceiling of a running book.
func TestPrincipalChangeUnderOpenHoldsRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capital_headline.json")

	l := newTestLedger(t, 400, 0, path)
	if err := l.Hold("p1", 100, 50, CashAndCarry, "BTC", t0); err != nil {
		t.Fatalf("hold: %v", err)
	}
	_, err := NewLedger(Config{Name: "test", Principal: 1600}, path)
	if err == nil {
		t.Fatal("principal changed from $400 to $1600 under an open hold without complaint")
	}
}

func TestManagerRoutesTwoBooks(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(ManagerConfig{Books: []Config{
		{Name: "headline", Service: "vega", Principal: 400, ReserveFrac: 0.20,
			Structures: []Structure{CashAndCarry}},
		{Name: "research", Service: "vega-union", Principal: 1600, ReserveFrac: 0.20,
			Structures: []Structure{CrossVenue}},
	}}, dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	h, err := m.Book("headline")
	if err != nil {
		t.Fatalf("Book(headline): %v", err)
	}
	r, err := m.Book("research")
	if err != nil {
		t.Fatalf("Book(research): %v", err)
	}
	if !near(h.Free(), 320) || !near(r.Free(), 1280) {
		t.Fatalf("free = headline $%.2f research $%.2f, want $320 / $1280", h.Free(), r.Free())
	}

	// Exhausting one book must not touch the other.
	for i := 0; i < 3; i++ {
		if err := h.Hold(fmt.Sprintf("h%d", i), 100, 50, CashAndCarry, "BTC", t0); err != nil {
			t.Fatalf("headline hold %d: %v", i, err)
		}
	}
	if err := h.Hold("h3", 100, 50, CashAndCarry, "BTC", t0); !errors.Is(err, ErrNoCapital) {
		t.Fatalf("headline overrun = %v, want ErrNoCapital", err)
	}
	if !near(r.Free(), 1280) {
		t.Fatalf("research free moved to $%.2f when headline filled; books are not isolated",
			r.Free())
	}

	if _, err := m.Book("typo"); err == nil {
		t.Fatal("unknown book name returned without error")
	}

	snaps := m.Snapshots(t0)
	if len(snaps) != 2 || snaps[0].Name != "headline" || snaps[1].Name != "research" {
		t.Fatalf("snapshots out of configured order: %+v", snaps)
	}
	if snaps[0].Service != "vega" || snaps[1].Service != "vega-union" {
		t.Fatalf("service attribution lost: %q / %q", snaps[0].Service, snaps[1].Service)
	}
	if !near(snaps[0].Utilisation, 300.0/320.0) {
		t.Fatalf("utilisation = %.4f, want %.4f", snaps[0].Utilisation, 300.0/320.0)
	}
}

func TestBadConfigRefused(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no name", Config{Principal: 400}},
		{"zero principal", Config{Name: "b", Principal: 0}},
		{"negative principal", Config{Name: "b", Principal: -400}},
		{"reserve at 1", Config{Name: "b", Principal: 400, ReserveFrac: 1}},
		{"reserve above 1", Config{Name: "b", Principal: 400, ReserveFrac: 1.5}},
		{"negative reserve", Config{Name: "b", Principal: 400, ReserveFrac: -0.1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewLedger(c.cfg, ""); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := NewManager(ManagerConfig{}, ""); err == nil {
		t.Fatal("empty book list accepted")
	}
	dup := ManagerConfig{Books: []Config{
		{Name: "a", Principal: 400}, {Name: "a", Principal: 400},
	}}
	if _, err := NewManager(dup, ""); err == nil {
		t.Fatal("duplicate book names accepted")
	}
}

// Concurrent holds must never let the book exceed its principal, even by one
// cent, and exactly the affordable number must succeed.
func TestConcurrentHoldsRespectCeiling(t *testing.T) {
	l := newTestLedger(t, 400, 0, "")

	const attempts = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.Hold(fmt.Sprintf("p%d", i), 100, 50, CashAndCarry, "BTC", t0); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if ok != 4 {
		t.Fatalf("%d of %d concurrent holds succeeded, want exactly 4", ok, attempts)
	}
	s := l.Snapshot(t0)
	if s.Allocated > 400 {
		t.Fatalf("allocated $%.2f exceeds principal $400", s.Allocated)
	}
	if !near(s.Allocated, 400) {
		t.Fatalf("allocated $%.2f, want $400 fully committed", s.Allocated)
	}
}

func TestCanFundMatchesHold(t *testing.T) {
	l := newTestLedger(t, 400, 0, "")
	if !l.CanFund(400) {
		t.Fatal("CanFund($400) false on an empty $400 book")
	}
	if l.CanFund(400.01) {
		t.Fatal("CanFund($400.01) true on a $400 book")
	}
	if err := l.Hold("p1", 400, 200, CashAndCarry, "BTC", t0); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if l.CanFund(0.01) {
		t.Fatal("CanFund true on a fully committed book")
	}
}

func TestWriteSnapshots(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(ManagerConfig{Books: []Config{
		{Name: "headline", Service: "vega", Principal: 400, ReserveFrac: 0.20},
	}}, dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	out := filepath.Join(dir, "capital_snapshot.json")
	if err := m.WriteSnapshots(out, t0); err != nil {
		t.Fatalf("WriteSnapshots: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading snapshot: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("snapshot file is empty")
	}
}
