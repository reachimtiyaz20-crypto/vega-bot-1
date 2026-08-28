package crossvenue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A table that round-trips only when empty proves nothing: the encoder, the
// decoder and every field tag are bypassed, and a save that wrote no bytes
// would pass identically. These carry real pairs and check the values back.
func TestUniverseRoundTripCarriesPairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "universe", "scan.jsonl")
	now := time.Now().UTC().Truncate(time.Second)

	want := []UniversePair{
		{Coin: "UNITREE", VenueA: "mexc", VenueB: "bybit", SymbolA: "UNITREE_USDT", SymbolB: "UNITREEUSDT",
			RecentBpsHr: 15.112, MeanBpsHr: 9.4, SameSign: 0.90, Intervals: 21, At: now},
		{Coin: "SIREN", VenueA: "binance", VenueB: "bybit", SymbolA: "SIRENUSDT", SymbolB: "SIRENUSDT",
			RecentBpsHr: 0.109, MeanBpsHr: 0.31, SameSign: 0.50, Intervals: 12, At: now},
	}

	u := &UniverseScanner{DataPath: path, Every: 90 * time.Minute}
	if err := u.save(want, now); err != nil {
		t.Fatalf("save: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("save wrote nothing: err=%v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after rename")
	}

	v := &UniverseScanner{DataPath: path, Every: 90 * time.Minute}
	v.load()
	got := v.Top(0, 0, 0, 10)
	if len(got) != 2 {
		t.Fatalf("loaded %d pairs, want 2", len(got))
	}
	by := map[string]UniversePair{}
	for _, p := range got {
		by[p.Coin] = p
	}
	for _, w := range want {
		g, ok := by[w.Coin]
		if !ok {
			t.Fatalf("%s missing after reload", w.Coin)
		}
		if g.VenueA != w.VenueA || g.VenueB != w.VenueB || g.SymbolA != w.SymbolA {
			t.Errorf("%s venues/symbols changed: %+v", w.Coin, g)
		}
		if g.RecentBpsHr != w.RecentBpsHr || g.MeanBpsHr != w.MeanBpsHr {
			t.Errorf("%s rates changed: recent %v want %v, mean %v want %v",
				w.Coin, g.RecentBpsHr, w.RecentBpsHr, g.MeanBpsHr, w.MeanBpsHr)
		}
		if g.SameSign != w.SameSign || g.Intervals != w.Intervals {
			t.Errorf("%s persistence changed: %v/%d want %v/%d",
				w.Coin, g.SameSign, g.Intervals, w.SameSign, w.Intervals)
		}
	}
}

// A table older than 2x the scan interval must be refused, not served. Serving
// it would let the book prioritise coins on evidence that has since gone quiet.
func TestUniverseStaleTableRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan.jsonl")
	old := time.Now().UTC().Add(-4 * time.Hour)

	u := &UniverseScanner{DataPath: path, Every: 90 * time.Minute}
	if err := u.save([]UniversePair{{Coin: "UNITREE", RecentBpsHr: 15.1, SameSign: 1, Intervals: 20, At: old}}, old); err != nil {
		t.Fatalf("save: %v", err)
	}

	v := &UniverseScanner{DataPath: path, Every: 90 * time.Minute}
	v.load()
	if got := v.Top(0, 0, 0, 10); got != nil {
		t.Fatalf("stale table served %d pairs, want none", len(got))
	}
	if got := v.Coins(0, 0, 0, 10); len(got) != 0 {
		t.Fatalf("stale table served %d coins via Coins(), want none", len(got))
	}
}

// Corrupt input must degrade to an empty table, never panic the process.
func TestUniverseCorruptFileIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan.jsonl")
	if err := os.WriteFile(path, []byte("{not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	u := &UniverseScanner{DataPath: path, Every: 90 * time.Minute}
	u.load()
	if got := u.Top(0, 0, 0, 10); len(got) != 0 {
		t.Fatalf("corrupt file yielded %d pairs", len(got))
	}
}

// A missing file is the normal first run and must be silent and harmless.
func TestUniverseMissingFileIsFirstRun(t *testing.T) {
	u := &UniverseScanner{DataPath: filepath.Join(t.TempDir(), "nope.jsonl"), Every: 90 * time.Minute}
	u.load()
	if got := u.Top(0, 0, 0, 10); len(got) != 0 {
		t.Fatalf("missing file yielded %d pairs", len(got))
	}
}
