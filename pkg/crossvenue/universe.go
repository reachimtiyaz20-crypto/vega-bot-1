package crossvenue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

// UNIVERSE-WIDE SETTLED SCAN.
//
// # THE BLIND SPOT THIS CLOSES
//
// The book shortlists on PREDICTED spread and only then checks settled history.
// Predicted spread was shown on 2026-08-19 to be estimation noise: 7.30 bps/hr
// claimed against 0.45 actually paid, missing low in 8 of 8 closed positions.
//
// So the shortlist is close to a random sample of ten pairs out of 3,400, and
// the gate then correctly refuses them. On 2026-08-20 that produced 371
// refusals and ZERO entries in six hours -- while a manual universe scan found
// three pairs genuinely clearing 3 bps/hr on recent settled data.
//
// There is no reason to think those three were among the ten it looked at.
// The book cannot find what it does not examine, and its examination criterion
// was the discredited one.
//
// # WHAT THIS DOES
//
// Walks every venue pair's SETTLED history on a slow ticker and keeps a ranked
// table. Coins near the top are handed to the builder as PriorityCoins, which
// bypass the predicted-spread floor exactly as open positions already do.
//
// It adds no gate and changes no judgement. It only changes what the book is
// SHOWN, which is the actual defect.
type UniversePair struct {
	Coin           string
	VenueA, VenueB string
	SymbolA        string
	SymbolB        string
	RecentBpsHr    float64 // last few settlements -- is it still happening
	MeanBpsHr      float64 // full window -- was it ever real
	SameSign       float64 // persistence, 0..1
	Intervals      int
	At             time.Time
}

type UniverseSave struct {
	LastScan time.Time      `json:"last_scan"`
	Pairs    []UniversePair `json:"pairs"`
}

type UniverseScanner struct {
	Settled *SettledCache
	Readers map[string]orderbook.PerpReader

	DataPath string // path to persist the scan, defaults to data/universe/scan.jsonl

	// Every is how often a full rescan starts. Settled history only changes at
	// settlements, which are hours apart, so scanning faster buys nothing and
	// costs thousands of requests.
	Every time.Duration
	// Budget is the per-venue time allowance, and Pace the delay between
	// requests. Bounded by TIME, never by request count: when a venue
	// throttles, a count-based cap becomes an hour-long stall.
	Budget time.Duration
	Pace   time.Duration

	mu       sync.RWMutex
	pairs    []UniversePair
	lastScan time.Time
	scanning bool
	fetched  int
	planned  int
}

func NewUniverseScanner(readers map[string]orderbook.PerpReader, settled *SettledCache, dataPath string) *UniverseScanner {
	u := &UniverseScanner{
		Settled:  settled,
		Readers:  readers,
		DataPath: dataPath,
		Every:    90 * time.Minute,
		Budget:   4 * time.Minute,
		Pace:     140 * time.Millisecond,
	}
	u.load()
	return u
}

// load restores a table written by a previous process. A missing file is normal
// on first run. A file that exists and cannot be parsed is not: an empty table
// and a corrupt one look identical from the outside, so say which this is
// rather than starting a six-hour rescan in silence.
func (u *UniverseScanner) load() {
	if u.DataPath == "" {
		return
	}
	b, err := os.ReadFile(u.DataPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "universe: cannot read %s: %v\n", u.DataPath, err)
		}
		return
	}
	var save UniverseSave
	if err := json.Unmarshal(b, &save); err != nil {
		fmt.Fprintf(os.Stderr, "universe: %s is corrupt, ignoring it: %v\n", u.DataPath, err)
		return
	}
	u.mu.Lock()
	u.pairs, u.lastScan = save.Pairs, save.LastScan
	u.mu.Unlock()
	fmt.Fprintf(os.Stderr, "universe: loaded %d pairs from %s, scanned %s ago\n",
		len(save.Pairs), u.DataPath, time.Since(save.LastScan).Truncate(time.Minute))
}

// save writes the table atomically and durably: temp file, fsync, rename. The
// rename alone survives a process crash; without the fsync it does not survive
// the machine losing power.
func (u *UniverseScanner) save(pairs []UniversePair, now time.Time) error {
	if u.DataPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(u.DataPath), 0o755); err != nil {
		return err
	}
	tmp := u.DataPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(UniverseSave{LastScan: now, Pairs: pairs}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, u.DataPath)
}

// coinOf strips a venue's symbol convention back to the bare asset.
func coinOf(venue, symbol string) string {
	switch venue {
	case "okx":
		return strings.SplitN(symbol, "-", 2)[0]
	case "mexc":
		return strings.TrimSuffix(symbol, "_USDT")
	case "binance", "bybit", "bitget":
		if !strings.HasSuffix(symbol, "USDT") {
			return ""
		}
		return strings.TrimSuffix(symbol, "USDT")
	}
	return symbol // hyperliquid and lighter address the coin directly
}

// Run scans immediately, then on the ticker, until the context ends.
func (u *UniverseScanner) Run(ctx context.Context) {
	u.ScanOnce(ctx)
	t := time.NewTicker(u.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.ScanOnce(ctx)
		}
	}
}

// ScanOnce refreshes the whole table. Safe to call concurrently; overlapping
// calls return immediately rather than doubling the request load.
func (u *UniverseScanner) ScanOnce(ctx context.Context) {
	u.mu.Lock()
	if u.scanning {
		u.mu.Unlock()
		return
	}
	u.scanning = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.scanning = false
		u.mu.Unlock()
	}()

	universe := map[string]map[string]string{} // coin -> venue -> symbol
	for name, r := range u.Readers {
		for _, sym := range r.Symbols() {
			c := coinOf(name, sym)
			if c == "" {
				continue
			}
			if universe[c] == nil {
				universe[c] = map[string]string{}
			}
			universe[c][name] = sym
		}
	}

	perVenue := map[string][]string{}
	ivl := map[string]float64{}
	for _, vm := range universe {
		if len(vm) < 2 {
			continue // nothing to pair against
		}
		for v, sym := range vm {
			perVenue[v] = append(perVenue[v], sym)
			if fi := u.Readers[v].FundingIntervalHours(sym); fi.Ok && fi.Hours > 0 {
				ivl[v+"|"+sym] = fi.Hours
			}
		}
	}

	// MEXC publishes collectCycle per symbol but not in its bulk feed, so the
	// interval is unknown until hydrated -- and an unknown interval is refused,
	// which would silently exclude the venue entirely.
	if mx, ok := u.Readers["mexc"].(*orderbook.MEXCPerp); ok {
		for pass := 0; pass < 10; pass++ {
			if ctx.Err() != nil {
				break
			}
			if got := mx.EnsureFundingMeta(ctx, perVenue["mexc"], u.Pace, len(perVenue["mexc"])); got == 0 {
				break
			}
		}
		for _, sym := range perVenue["mexc"] {
			if fi := mx.FundingIntervalHours(sym); fi.Ok && fi.Hours > 0 {
				ivl["mexc|"+sym] = fi.Hours
			}
		}
	}

	planned := 0
	for _, syms := range perVenue {
		planned += len(syms)
	}
	u.mu.Lock()
	u.planned = planned
	u.fetched = 0
	u.mu.Unlock()

	for v, syms := range perVenue {
		if ctx.Err() != nil {
			return
		}
		f, _ := u.Settled.Ensure(ctx, v, syms,
			func(sym string) float64 { return ivl[v+"|"+sym] }, u.Budget, u.Pace)
		u.mu.Lock()
		u.fetched += f
		u.mu.Unlock()
	}

	now := time.Now().UTC()
	var out []UniversePair
	for coin, vm := range universe {
		if len(vm) < 2 {
			continue
		}
		names := make([]string, 0, len(vm))
		for v := range vm {
			names = append(names, v)
		}
		sort.Strings(names)
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				a, b := names[i], names[j]
				ra, ok1 := u.Settled.Get(a, vm[a])
				rb, ok2 := u.Settled.Get(b, vm[b])
				if !ok1 || !ok2 || !ra.Known() || !rb.Known() {
					continue
				}
				n := ra.Intervals
				if rb.Intervals < n {
					n = rb.Intervals
				}
				ss := ra.SameSignFrac
				if rb.SameSignFrac < ss {
					ss = rb.SameSignFrac
				}
				recent := rb.RecentBpsPerHour - ra.RecentBpsPerHour
				mean := rb.BpsPerHour - ra.BpsPerHour
				if recent < 0 {
					recent, mean = -recent, -mean
					a, b = b, a
				}
				out = append(out, UniversePair{
					Coin: coin, VenueA: a, VenueB: b,
					SymbolA: vm[a], SymbolB: vm[b],
					RecentBpsHr: recent, MeanBpsHr: mean,
					SameSign: ss, Intervals: n, At: now,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecentBpsHr > out[j].RecentBpsHr })

	u.mu.Lock()
	u.pairs, u.lastScan = out, now
	u.mu.Unlock()

	if err := u.save(out, now); err != nil {
		fmt.Fprintf(os.Stderr, "universe: save failed: %v\n", err)
	}
}

// Top returns pairs meeting the given bars, widest first.
func (u *UniverseScanner) Top(minRecent, minSameSign float64, minIntervals, max int) []UniversePair {
	u.mu.RLock()
	defer u.mu.RUnlock()

	if !u.lastScan.IsZero() && time.Since(u.lastScan) > 2*u.Every {
		os.Stderr.WriteString("universe: table is STALE (older than 2x interval), ignoring it\n")
		return nil
	}

	var out []UniversePair
	for _, p := range u.pairs {
		if p.RecentBpsHr < minRecent || p.SameSign < minSameSign || p.Intervals < minIntervals {
			continue
		}
		out = append(out, p)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

// Coins is Top reduced to a set the builder can use as a floor bypass.
func (u *UniverseScanner) Coins(minRecent, minSameSign float64, minIntervals, max int) map[string]bool {
	out := map[string]bool{}
	for _, p := range u.Top(minRecent, minSameSign, minIntervals, max) {
		out[p.Coin] = true
	}
	return out
}

// Status reports table size and age, for the pass summary. A stale table is
// worse than none: it would prioritise coins whose episodes ended hours ago.
func (u *UniverseScanner) Status() (pairs int, lastScan time.Time, scanning bool, fetched int, planned int) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return len(u.pairs), u.lastScan, u.scanning, u.fetched, u.planned
}
