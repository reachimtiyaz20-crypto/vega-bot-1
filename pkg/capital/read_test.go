package capital

import (
	"os"
	"path/filepath"
	"testing"
)

// The reader and the writer must agree. If they ever diverge, the dashboard
// reports a different free balance from the one the book is enforcing, and
// there is nothing on the page to say which number is real.
func TestReadSnapshotAgreesWithLedger(t *testing.T) {
	dir := t.TempDir()
	path := LedgerPath(dir, "headline")

	l, err := NewLedger(Config{
		Name: "headline", Service: "vega", Principal: 400, ReserveFrac: 0.20,
	}, path)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	if err := l.Hold("binance:BTCUSDT", 100, 50, CashAndCarry, "BTCUSDT", t0); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := l.Hold("bybit:ETHUSDT", 100, 50, CashAndCarry, "ETHUSDT", t0); err != nil {
		t.Fatalf("hold: %v", err)
	}

	want := l.Snapshot(t0)
	got, ok, err := ReadSnapshot(path)
	if err != nil || !ok {
		t.Fatalf("ReadSnapshot: ok=%v err=%v", ok, err)
	}

	if got.Name != want.Name {
		t.Errorf("name %q, want %q", got.Name, want.Name)
	}
	for _, c := range []struct {
		field    string
		got, exp float64
	}{
		{"principal", got.Principal, want.Principal},
		{"reserve", got.Reserve, want.Reserve},
		{"allocated", got.Allocated, want.Allocated},
		{"free", got.Free, want.Free},
		{"utilisation", got.Utilisation, want.Utilisation},
	} {
		if !near(c.got, c.exp) {
			t.Errorf("%s = %.6f, writer says %.6f", c.field, c.got, c.exp)
		}
	}
	if got.Positions != want.Positions {
		t.Errorf("positions %d, want %d", got.Positions, want.Positions)
	}
	if len(got.Holds) != 2 || got.Holds[0].ID != "binance:BTCUSDT" {
		t.Errorf("holds not sorted or incomplete: %+v", got.Holds)
	}
}

// A ledger writes on its first hold, not at startup. No file means no position
// has ever been opened in that book -- a real state, not a fault, and it must
// not render as a book with zero principal.
func TestReadSnapshotMissingFileIsNotAnError(t *testing.T) {
	snap, ok, err := ReadSnapshot(filepath.Join(t.TempDir(), "capital_nope.json"))
	if err != nil {
		t.Fatalf("missing file returned an error: %v", err)
	}
	if ok {
		t.Fatal("missing file reported ok=true")
	}
	if snap.Principal != 0 {
		t.Fatalf("missing file produced a populated snapshot: %+v", snap)
	}
}

func TestReadSnapshotCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capital_headline.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, ok, err := ReadSnapshot(path); err == nil || ok {
		t.Fatalf("corrupt file read as ok=%v err=%v", ok, err)
	}
}

func TestFindLedgers(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"research", "headline"} {
		if err := os.WriteFile(LedgerPath(dir, n), []byte("{}"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	// Decoys that must not be picked up.
	for _, n := range []string{"positions.json", "capital_headline.json.tmp", "fees.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	got, err := FindLedgers(dir)
	if err != nil {
		t.Fatalf("FindLedgers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d ledgers, want 2: %v", len(got), got)
	}
	if filepath.Base(got[0]) != "capital_headline.json" {
		t.Errorf("not sorted by name: %v", got)
	}

	empty, err := FindLedgers("")
	if err != nil || empty != nil {
		t.Errorf("empty dir returned %v, %v; want nil, nil", empty, err)
	}
}
