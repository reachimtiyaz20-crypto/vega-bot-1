package crossvenue

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSettledCachePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dataPath := filepath.Join(tmpDir, "settled.json")

	now := time.Now().UTC()

	// 1. Round-trip at least 3 distinct entries across 2+ venues
	t.Run("RoundTrip", func(t *testing.T) {
		c1 := NewSettledCache(24*time.Hour, dataPath)

		c1.mu.Lock()
		c1.data["binance|BTC"] = SettledRate{
			Venue: "binance", Symbol: "BTC",
			BpsPerHour: 0.035, RecentBpsPerHour: 0.040,
			Intervals: 12, RecentIntervals: 3,
			SameSignFrac: 0.85, LastSettleMs: now.UnixMilli(),
			FetchedAt: now,
		}
		c1.data["okx|ETH"] = SettledRate{
			Venue: "okx", Symbol: "ETH",
			BpsPerHour: 0.080, RecentBpsPerHour: 0.075,
			Intervals: 10, RecentIntervals: 2,
			SameSignFrac: 1.0, LastSettleMs: now.UnixMilli() - 10000,
			FetchedAt: now.Add(-time.Hour),
		}
		c1.data["mexc|UNITREE"] = SettledRate{
			Venue: "mexc", Symbol: "UNITREE",
			BpsPerHour: 15.1, RecentBpsPerHour: 12.5,
			Intervals: 6, RecentIntervals: 3,
			SameSignFrac: 0.6, LastSettleMs: now.UnixMilli() - 5000,
			FetchedAt: now.Add(-2 * time.Hour),
		}
		c1.mu.Unlock()

		if err := c1.save(); err != nil {
			t.Fatalf("save failed: %v", err)
		}

		c2 := NewSettledCache(24*time.Hour, dataPath)
		if c2.Size() != 3 {
			t.Fatalf("expected 3 entries, got %d", c2.Size())
		}

		for _, key := range []struct{ v, s string }{
			{"binance", "BTC"}, {"okx", "ETH"}, {"mexc", "UNITREE"},
		} {
			r1, _ := c1.Get(key.v, key.s)
			r2, ok2 := c2.Get(key.v, key.s)
			if !ok2 {
				t.Fatalf("missing %s|%s", key.v, key.s)
			}
			if math.Abs(r1.BpsPerHour-r2.BpsPerHour) > 1e-9 {
				t.Errorf("%s|%s BpsPerHour mismatch: %v != %v", key.v, key.s, r1.BpsPerHour, r2.BpsPerHour)
			}
			if math.Abs(r1.RecentBpsPerHour-r2.RecentBpsPerHour) > 1e-9 {
				t.Errorf("%s|%s RecentBpsPerHour mismatch: %v != %v", key.v, key.s, r1.RecentBpsPerHour, r2.RecentBpsPerHour)
			}
			if math.Abs(r1.SameSignFrac-r2.SameSignFrac) > 1e-9 {
				t.Errorf("%s|%s SameSignFrac mismatch: %v != %v", key.v, key.s, r1.SameSignFrac, r2.SameSignFrac)
			}
			if r1.Intervals != r2.Intervals || r1.RecentIntervals != r2.RecentIntervals {
				t.Errorf("%s|%s Intervals mismatch", key.v, key.s)
			}
			if r1.LastSettleMs != r2.LastSettleMs {
				t.Errorf("%s|%s LastSettleMs mismatch", key.v, key.s)
			}
			if r1.FetchedAt.UnixMilli() != r2.FetchedAt.UnixMilli() {
				t.Errorf("%s|%s FetchedAt mismatch: %v != %v", key.v, key.s, r1.FetchedAt, r2.FetchedAt)
			}
		}

		// Check for no .tmp file
		tmpPath := dataPath + ".tmp"
		if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
			t.Errorf("tmp file %s remains after save", tmpPath)
		}
	})

	// 2. Load where older than TTL is dropped while fresh survives
	t.Run("TTL Drop", func(t *testing.T) {
		c1 := NewSettledCache(time.Hour, dataPath) // very short TTL

		c1.mu.Lock()
		// Will survive
		c1.data["binance|BTC"] = SettledRate{
			Venue: "binance", Symbol: "BTC",
			BpsPerHour: 0.035, FetchedAt: time.Now(),
			Intervals: 1, // Need Known() == true
		}
		// Will be dropped
		c1.data["okx|ETH"] = SettledRate{
			Venue: "okx", Symbol: "ETH",
			BpsPerHour: 0.080, FetchedAt: time.Now().Add(-2 * time.Hour),
			Intervals: 1,
		}
		c1.mu.Unlock()
		if err := c1.save(); err != nil {
			t.Fatalf("save failed: %v", err)
		}

		c2 := NewSettledCache(time.Hour, dataPath)
		if c2.Size() != 1 {
			t.Fatalf("expected 1 surviving entry, got %d", c2.Size())
		}
		if _, ok := c2.Get("binance", "BTC"); !ok {
			t.Errorf("fresh entry missing")
		}
		// Should NOT be there, but also not returned by Get due to TTL check if we forced it.
		// Checking internal data just to be sure it wasn't even loaded.
		c2.mu.RLock()
		_, ok := c2.data["okx|ETH"]
		c2.mu.RUnlock()
		if ok {
			t.Errorf("expired entry was loaded")
		}
	})

	// 3. Corrupt file yields empty cache, no panic
	t.Run("Corrupt File", func(t *testing.T) {
		if err := os.WriteFile(dataPath, []byte("{corrupt json"), 0644); err != nil {
			t.Fatal(err)
		}

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked on corrupt file: %v", r)
			}
		}()

		c := NewSettledCache(time.Hour, dataPath)
		if c.Size() != 0 {
			t.Errorf("expected 0 size for corrupt file, got %d", c.Size())
		}
	})

	// 4. Missing file is treated as first run
	t.Run("Missing File", func(t *testing.T) {
		missingPath := filepath.Join(tmpDir, "missing.json")

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked on missing file: %v", r)
			}
		}()

		c := NewSettledCache(time.Hour, missingPath)
		if c.Size() != 0 {
			t.Errorf("expected 0 size for missing file, got %d", c.Size())
		}
	})
}
