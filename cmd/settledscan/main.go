// Command settledscan measures the SETTLED funding spread across the entire
// cross-venue universe, without any predicted-rate pre-filter.
//
// # WHY
//
// The live book shortlists on PREDICTED spread and only then checks settled
// history. Predicted spread was shown on 2026-08-19 to be estimation noise --
// 7.30 bps/hr claimed against 0.45 actually settled, missing low in 8 of 8
// cases. So the shortlist is close to a random sample of 19 pairs out of 5,000,
// and a coin with a stable, real settled spread would be examined only by luck.
//
// The book cannot find what it does not look at, and its looking criterion is
// the thing that was discredited.
//
// This looks at everything and asks the question the project turns on: does a
// tradeable settled spread exist anywhere, and how often.
//
// Read-only. No orders, no positions, no state touched.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/crossvenue"
	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

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
	return symbol // hyperliquid, lighter address the coin directly
}

type pairResult struct {
	Coin      string  `json:"coin"`
	VenueA    string  `json:"venue_a"`
	VenueB    string  `json:"venue_b"`
	SpreadBps float64 `json:"settled_spread_bps_hr"`
	RecentBps float64 `json:"recent_spread_bps_hr"`
	SameSign  float64 `json:"same_sign"`
	Intervals int     `json:"intervals"`
	RateA     float64 `json:"rate_a"`
	RateB     float64 `json:"rate_b"`
}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	maxCoins := flag.Int("max-coins", 0, "cap coins scanned (0 = all)")
	pace := flag.Duration("pace", 130*time.Millisecond, "delay between requests per venue")
	budget := flag.Duration("budget", 25*time.Minute, "total time budget per venue")
	minIntervals := flag.Int("min-intervals", 6, "settlements required before a pair counts")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	readers := orderbook.Readers()
	fmt.Println("loading instrument lists...")
	for name, r := range readers {
		if err := r.LoadSymbols(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", name, err)
		}
	}

	// coin -> venue -> symbol
	universe := map[string]map[string]string{}
	for name, r := range readers {
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

	var coins []string
	for c, vm := range universe {
		if len(vm) >= 2 {
			coins = append(coins, c)
		}
	}
	sort.Strings(coins)
	if *maxCoins > 0 && len(coins) > *maxCoins {
		coins = coins[:*maxCoins]
	}
	fmt.Printf("%d coins total, %d on two or more venues\n\n", len(universe), len(coins))

	// Group the work by venue so each venue's pacing is its own.
	perVenue := map[string][]string{}
	ivl := map[string]float64{}
	for _, c := range coins {
		for v, sym := range universe[c] {
			perVenue[v] = append(perVenue[v], sym)
			if fi := readers[v].FundingIntervalHours(sym); fi.Ok && fi.Hours > 0 {
				ivl[v+"|"+sym] = fi.Hours
			}
		}
	}

	// MEXC publishes collectCycle per symbol but not in its bulk feed, so the
	// interval is unknown until hydrated. Without this every MEXC rate is
	// refused -- which is correct, and useless.
	if mx, isMexc := readers["mexc"].(*orderbook.MEXCPerp); isMexc {
		// EnsureFundingMeta carries a 25-second internal budget, so one call
		// hydrates ~73 symbols and the remaining 541 are refused for an
		// unknown interval. Correct, and useless. Loop until it stops making
		// progress.
		n := 0
		for pass := 0; pass < 12; pass++ {
			got := mx.EnsureFundingMeta(ctx, perVenue["mexc"], 90*time.Millisecond, len(perVenue["mexc"]))
			n += got
			if got == 0 {
				break
			}
		}
		fmt.Printf("  mexc: hydrated %d of %d intervals\n", n, len(perVenue["mexc"]))
		for _, sym := range perVenue["mexc"] {
			if fi := mx.FundingIntervalHours(sym); fi.Ok && fi.Hours > 0 {
				ivl["mexc|"+sym] = fi.Hours
			}
		}
	}

	cache := crossvenue.NewSettledCache(4 * time.Hour, "")
	for v, syms := range perVenue {
		start := time.Now()
		f, bad := cache.Ensure(ctx, v, syms,
			func(sym string) float64 { return ivl[v+"|"+sym] }, *budget, *pace)
		fmt.Printf("  %-12s %5d symbols -> %5d fetched, %4d failed  (%s)\n",
			v, len(syms), f, bad, time.Since(start).Round(time.Second))
	}

	var results []pairResult
	for _, c := range coins {
		var vs []string
		for v := range universe[c] {
			vs = append(vs, v)
		}
		sort.Strings(vs)
		for i := 0; i < len(vs); i++ {
			for j := i + 1; j < len(vs); j++ {
				a, bb := vs[i], vs[j]
				ra, ok1 := cache.Get(a, universe[c][a])
				rb, ok2 := cache.Get(bb, universe[c][bb])
				if !ok1 || !ok2 || ra.Intervals < *minIntervals || rb.Intervals < *minIntervals {
					continue
				}
				ss := math.Min(ra.SameSignFrac, rb.SameSignFrac)
				n := ra.Intervals
				if rb.Intervals < n {
					n = rb.Intervals
				}
				results = append(results, pairResult{
					Coin: c, VenueA: a, VenueB: bb,
					SpreadBps: math.Abs(ra.BpsPerHour - rb.BpsPerHour),
					RecentBps: math.Abs(ra.RecentBpsPerHour - rb.RecentBpsPerHour),
					SameSign:  ss, Intervals: n,
					RateA: ra.BpsPerHour, RateB: rb.BpsPerHour,
				})
			}
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].RecentBps > results[j].RecentBps })

	out := filepath.Join(*dataDir, "settledscan")
	os.MkdirAll(out, 0o755)
	path := filepath.Join(out, fmt.Sprintf("scan-%s.jsonl", time.Now().UTC().Format("2006-01-02-1504")))
	if f, err := os.Create(path); err == nil {
		enc := json.NewEncoder(f)
		for _, r := range results {
			enc.Encode(r)
		}
		f.Close()
	}

	fmt.Printf("\n%d venue pairs priced from settled history\n\n", len(results))
	if len(results) == 0 {
		fmt.Println("nothing to report")
		return
	}

	// Percentiles are computed on their OWN sorted copy. The results slice is
	// ordered by RecentBps, and indexing one quantity by another's rank
	// produced a "distribution" where p90 sat below p75.
	fmt.Println("DISTRIBUTION, bps/hr")
	pctl := func(label string, pick func(pairResult) float64) {
		v := make([]float64, len(results))
		for i, r := range results {
			v[i] = pick(r)
		}
		sort.Float64s(v)
		fmt.Printf("  %-10s", label)
		for _, q := range []float64{0.50, 0.75, 0.90, 0.95, 0.99} {
			fmt.Printf("  p%-3.0f %8.4f", q*100, v[int(float64(len(v)-1)*q)])
		}
		fmt.Println()
	}
	pctl("12-settle", func(r pairResult) float64 { return r.SpreadBps })
	pctl("recent", func(r pairResult) float64 { return r.RecentBps })
	fmt.Println()
	// FULL WINDOW vs RECENT WINDOW.
	//
	// The gap between these columns IS the staleness problem. A pair clearing
	// on the twelve-settlement mean but not on the last three is an episode
	// that has already ended -- exactly what BICO turned out to be.
	fmt.Println("HOW MANY CLEAR A THRESHOLD")
	fmt.Printf("  %-11s %9s %9s %11s %12s\n",
		"threshold", "12-SETTLE", "RECENT", "persistent", "ALREADY OVER")
	for _, t := range []float64{0.5, 1, 2, 3, 5, 10} {
		full, rec, pers, over := 0, 0, 0, 0
		for _, r := range results {
			f, c := r.SpreadBps >= t, r.RecentBps >= t
			if f {
				full++
			}
			if c {
				rec++
				if r.SameSign >= 0.75 {
					pers++
				}
			}
			if f && !c {
				over++
			}
		}
		fmt.Printf("  >= %-8.1f %9d %9d %11d %12d\n", t, full, rec, pers, over)
	}

	fmt.Println()
	fmt.Println("WIDEST 20")
	fmt.Printf("  %-12s %-9s %-9s %9s %9s %9s %9s %7s\n",
		"COIN", "A", "B", "12-SETTLE", "RECENT", "RATE A", "RATE B", "SAME")
	for i, r := range results {
		if i >= 20 {
			break
		}
		fmt.Printf("  %-12s %-9s %-9s %9.4f %9.4f %9.4f %9.4f %6.0f%%\n",
			r.Coin, r.VenueA, r.VenueB, r.SpreadBps, r.RecentBps, r.RateA, r.RateB, r.SameSign*100)
	}

	fmt.Printf("\nwritten to %s\n", path)
	fmt.Print(`
READ THE THRESHOLD TABLE, NOT THE WIDEST LIST. A handful of wide pairs proves
nothing -- they may be illiquid, unfillable, or about to revert. The question is
how many pairs clear a level that repays a ~40 bps round trip in a sane time,
AND hold their sign across settlements.

At 3 bps/hr a 40 bps round trip repays in about 13 hours. Below roughly 1 bps/hr
nothing is tradeable at any hold this book would accept.
`)
}
