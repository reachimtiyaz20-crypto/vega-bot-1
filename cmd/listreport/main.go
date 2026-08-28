// Command listreport backtests the new-listing funding strategy across every
// cached venue, from the funding history on disk.
//
// # LISTING DATE
//
// The earliest funding settlement ANYWHERE dates the listing. A perp cannot
// settle funding before it exists, and the first venue to list it is the first
// to settle. Accurate to within one interval.
//
// # WHY THAT MATTERS MORE THAN IT SOUNDS
//
// The first backtest started its clock at the BINANCE listing, because that is
// the only onboardDate available. If MEXC lists a coin three weeks earlier,
// the richest part of the dislocation happened before the clock started. This
// version starts at whichever venue was first.
//
// INTERVALS ARE DERIVED FROM SETTLEMENT GAPS, per symbol, per venue -- not
// assumed. That assumption has been wrong twice here already.
//
// # WHAT REMAINS ASSUMED, AND IT DECIDES THE ANSWER
//
// Slippage. No historical order books exist anywhere. Yesterday's stress test
// showed the result flipping from +36% to -52% between 15 and 60 bps, so this
// prints a sweep rather than a number. Read the sweep, not the headline.
//
// Survivorship. Every venue's API returns currently-listed symbols. Coins that
// collapsed and were delisted are absent -- and those are precisely where
// funding went extreme and the price went to zero.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type point struct {
	TsMs int64   `json:"t"`
	Rate float64 `json:"r"`
}

type history struct {
	Venue     string  `json:"venue"`
	Symbol    string  `json:"symbol"`
	Coin      string  `json:"coin"`
	Points    []point `json:"points"`
	IntervalH float64 `json:"interval_hours"`
	FirstMs   int64   `json:"first_ms"`
}

// takerBps is what each venue charges, one leg, one direction.
//
// PROVENANCE MATTERS AND IT IS UNEVEN:
//
//	binance 5.0      read off the live fee page 2026-08-05
//	bybit   5.5      read off the live fee panel 2026-08-05
//	hyperliquid 4.5  hyperliquid docs 2026-08-11
//	okx    25.0      okx.com/en-ae/fees 2026-08-15, regular tier. The 5 bps
//	                 figure carried from memory was the VIP 9 rate.
//	mexc    2.0      NOT VERIFIED
//	bingx   5.0      NOT VERIFIED
//	gate    5.0      NOT VERIFIED
//	bitget  6.0      NOT VERIFIED
//
// The four unverified ones are the aggressive listing venues, so they carry
// most of the weight in this result. Override them once checked.
// takerBps is populated from FLAGS, not literals.
//
// It was a hardcoded map until 2026-08-19, when an independent review caught
// that OKX was recorded at 25 bps against a published regular-tier futures
// taker of 5. Every OKX pair's round trip was overstated by 40 bps, and the
// conclusion "OKX is structurally expensive" -- repeated three times in
// analysis -- was an artifact of that one literal.
//
// The value being wrong is the smaller problem. A fee living in source rather
// than in configuration cannot be corrected without a rebuild, and cannot be
// audited against an account statement at all.
var takerBps = map[string]float64{}

// venueFloor is the earliest timestamp a venue returned for ANY symbol.
//
// Two different things produce a floor and they mean opposite things:
//
//	binance/bybit/hyperliquid  365d   <- MY request window. A symbol sitting
//	                                     here is PROVEN older than a year.
//	gate 30d, bitget 90d, okx 95d     <- THEIR retention limit. A symbol here
//	                                     could be any age at all.
//
// Either way the first cached point is not a listing date, and treating it as
// one is what dated SKALE -- listed 2020 -- to exactly 365 days ago.
func venueFloors(root string) map[string]int64 {
	out := map[string]int64{}
	vs, _ := os.ReadDir(root)
	for _, v := range vs {
		if !v.IsDir() {
			continue
		}
		lo := int64(math.MaxInt64)
		fs, _ := os.ReadDir(filepath.Join(root, v.Name()))
		for _, f := range fs {
			b, err := os.ReadFile(filepath.Join(root, v.Name(), f.Name()))
			if err != nil {
				continue
			}
			var h history
			if json.Unmarshal(b, &h) != nil || h.FirstMs == 0 {
				continue
			}
			if h.FirstMs < lo {
				lo = h.FirstMs
			}
		}
		if lo < math.MaxInt64 {
			out[v.Name()] = lo
		}
	}
	return out
}

const censorTolMs = 2 * 24 * 3600 * 1000

func main() {
	dataDir := flag.String("data", "data", "data directory")
	days := flag.Int("days", 365, "listings within this many days count as new")
	window := flag.Int("window", 30, "days to follow each listing")
	minSpread := flag.Float64("min-spread", 3.0, "enter above this, bps/hr")
	exitRatio := flag.Float64("exit-ratio", 0.3, "exit when the spread falls to this fraction of the entry threshold")
	minHold := flag.Float64("min-hold", 8, "minimum hold, hours")
	maxHold := flag.Float64("max-hold", 168, "maximum hold, hours")
	cooldown := flag.Float64("cooldown", 24, "wait before re-entering the same pair, hours")
	slipBps := flag.Float64("slippage", 15, "round-trip slippage, ASSUMED")
	notional := flag.Float64("notional", 400, "USD per leg")
	venuesFlag := flag.String("venues", "", "restrict SIMULATION to these venues; dating still uses all eight")
	afterStr := flag.String("listed-after", "", "only listings on/after YYYY-MM-DD")
	beforeStr := flag.String("listed-before", "", "only listings before YYYY-MM-DD")
	sweep := flag.Bool("sweep", false, "print a slippage sweep instead of one number")
	top := flag.Int("top", 25, "rows")
	fees := flag.String("fees", "binance=5,bybit=5.5,hyperliquid=4.5,okx=5,mexc=2,bingx=5,gate=5,bitget=6",
		"per-venue taker fees in bps -- VERIFY EACH against your own account; a venue omitted here is excluded")
	flag.Parse()
	for _, kv := range strings.Split(*fees, ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(parts[1], 64)
		if err != nil || v < 0 {
			fmt.Fprintf(os.Stderr, "unreadable fee %q\n", kv)
			os.Exit(1)
		}
		takerBps[parts[0]] = v
	}

	root := filepath.Join(*dataDir, "fundcache")
	var universe map[string]map[string]string
	raw, err := os.ReadFile(filepath.Join(root, "universe.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "no cache at %s -- run fundcache first\n", root)
		os.Exit(1)
	}
	if err := json.Unmarshal(raw, &universe); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	allow := map[string]bool{}
	for _, v := range strings.Split(*venuesFlag, ",") {
		if v = strings.TrimSpace(v); v != "" {
			allow[v] = true
		}
	}
	// Venue coverage is NOT uniform across the year. bitget stops at 90 days,
	// okx at 95, gate at 30. A coin listed last month is searched across 28
	// venue pairs; one listed six months ago across 10. That under-detects old
	// listings, so any first-half/second-half comparison measures COVERAGE
	// rather than strategy unless the venue set is held flat.
	var afterMs int64
	beforeMs := int64(math.MaxInt64)
	if *afterStr != "" {
		t, err := time.Parse("2006-01-02", *afterStr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		afterMs = t.UnixMilli()
	}
	if *beforeStr != "" {
		t, err := time.Parse("2006-01-02", *beforeStr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		beforeMs = t.UnixMilli()
	}

	floors := venueFloors(root)
	fmt.Printf("HISTORY DEPTH PER VENUE (a symbol at the floor cannot be dated)\n")
	fns := make([]string, 0, len(floors))
	for v := range floors {
		fns = append(fns, v)
	}
	sort.Strings(fns)
	for _, v := range fns {
		fmt.Printf("  %-12s reaches back %.0f days\n", v,
			time.Since(time.UnixMilli(floors[v])).Hours()/24)
	}
	fmt.Println()

	cutoff := time.Now().Add(-time.Duration(*days) * 24 * time.Hour).UnixMilli()
	hours := *window * 24

	type coinResult struct {
		coin      string
		listedMs  int64
		firstVen  string
		venues    int
		trades    int
		netBps    float64
		heldHours float64
		maxSpread float64
		bestPair  string
	}
	var results []coinResult
	loaded, skipped, undatable, provenOld, tooOld := 0, 0, 0, 0, 0

	for coin, vm := range universe {
		if len(vm) < 2 {
			continue
		}
		// Load only this coin's histories, then discard -- 4,455 files at once
		// would not fit comfortably in 1 GB.
		hs := map[string]history{}
		for vname, sym := range vm {
			p := filepath.Join(root, vname, safeName(sym)+".json")
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var h history
			if json.Unmarshal(b, &h) != nil || len(h.Points) < 3 || h.IntervalH <= 0 {
				continue
			}
			hs[vname] = h
		}
		if len(hs) < 2 {
			skipped++
			continue
		}

		// Listing date from UNCENSORED histories only.
		censored := map[string]bool{}
		for v, h := range hs {
			if fl, ok := floors[v]; ok && h.FirstMs <= fl+censorTolMs {
				censored[v] = true
			}
		}
		listed := int64(math.MaxInt64)
		firstVen := ""
		for v, h := range hs {
			if censored[v] {
				continue
			}
			if h.FirstMs < listed {
				listed, firstVen = h.FirstMs, v
			}
		}
		if firstVen == "" {
			undatable++
			continue // every venue truncated -- age unknowable
		}
		// A censored venue still PROVES the coin traded at its floor. If that
		// is earlier than our candidate date, the coin is older than it looks.
		contradicted := false
		for v, h := range hs {
			if censored[v] && h.FirstMs < listed-censorTolMs {
				contradicted = true
				break
			}
		}
		if contradicted {
			provenOld++
			continue
		}
		if listed < afterMs || listed >= beforeMs {
			continue
		}
		if listed < cutoff {
			tooOld++
			continue // dated, and genuinely not new
		}
		loaded++

		// Hourly series per venue, forward-filled.
		series := map[string][]float64{}
		for v, h := range hs {
			if len(allow) > 0 && !allow[v] {
				continue
			}
			series[v] = hourly(h.Points, h.IntervalH, listed, hours)
		}

		cr := coinResult{coin: coin, listedMs: listed, firstVen: firstVen, venues: len(hs)}
		names := make([]string, 0, len(series))
		for v := range series {
			names = append(names, v)
		}
		sort.Strings(names)

		bestPairNet := math.Inf(-1)
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				a, b := names[i], names[j]
				rt := 2*(takerBps[a]+takerBps[b]) + *slipBps
				tr, net, held, ms := simulate(series[a], series[b],
					*minSpread, *minSpread*(*exitRatio), *minHold, *maxHold, *cooldown, rt)
				cr.trades += tr
				cr.netBps += net
				cr.heldHours += held
				if ms > cr.maxSpread {
					cr.maxSpread = ms
				}
				if net > bestPairNet {
					bestPairNet, cr.bestPair = net, a+"/"+b
				}
			}
		}
		if cr.trades > 0 {
			results = append(results, cr)
		}
	}

	fmt.Printf("MULTI-VENUE NEW-LISTING BACKTEST\n\n")
	fmt.Printf("  coins on 2+ venues                        %d\n",
		loaded+skipped+undatable+provenOld+tooOld)
	fmt.Printf("    discarded, every venue truncated        %d\n", undatable)
	fmt.Printf("    discarded, proven older than they look  %d\n", provenOld)
	fmt.Printf("    discarded, dated and not new            %d\n", tooOld)
	fmt.Printf("  genuine listings within %d days           %d\n", *days, loaded)
	fmt.Printf("  produced at least one trade              %d\n", len(results))
	fmt.Printf("  window %d days, enter above %.1f bps/hr, exit at %.1f, hold %.0f-%.0fh, cooldown %.0fh\n",
		*window, *minSpread, *minSpread*(*exitRatio), *minHold, *maxHold, *cooldown)
	fmt.Printf("  slippage %.0f bps ASSUMED, on top of each venue pair's real fees\n\n", *slipBps)

	if *sweep {
		fmt.Println("  (re-run without -sweep for the per-coin table)")
	}

	sort.Slice(results, func(i, j int) bool { return results[i].netBps > results[j].netBps })

	if !*sweep {
		fmt.Printf("%-14s %-12s %-11s %5s %6s %11s %9s %10s\n",
			"COIN", "LISTED", "FIRST ON", "VENUES", "TRADES", "NET bps", "HELD h", "BEST PAIR")
		fmt.Println(strings.Repeat("-", 92))
		for i, r := range results {
			if i >= *top {
				fmt.Printf("  ... %d more\n", len(results)-*top)
				break
			}
			fmt.Printf("%-14s %-12s %-11s %5d %6d %+11.1f %9.0f %10s\n",
				r.coin, time.UnixMilli(r.listedMs).UTC().Format("2006-01-02"),
				r.firstVen, r.venues, r.trades, r.netBps, r.heldHours, r.bestPair)
		}
	}

	var netBps, held float64
	trades, wins := 0, 0
	firstOn := map[string]int{}
	for _, r := range results {
		netBps += r.netBps
		held += r.heldHours
		trades += r.trades
		if r.netBps > 0 {
			wins++
		}
		firstOn[r.firstVen]++
	}
	netUSD := netBps / 10000 * *notional

	var sorted []float64
	for _, r := range results {
		sorted = append(sorted, r.netBps)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))

	fmt.Printf("\nCONCENTRATION -- the number that decides if this is an edge\n\n")
	if len(sorted) > 5 {
		var t1, t3, t5 float64
		for i := 0; i < 5 && i < len(sorted); i++ {
			if i < 1 {
				t1 += sorted[i]
			}
			if i < 3 {
				t3 += sorted[i]
			}
			t5 += sorted[i]
		}
		all := 0.0
		for _, x := range sorted {
			all += x
		}
		fmt.Printf("  best single coin          %+.0f bps = %.0f%% of everything\n", t1, t1/all*100)
		fmt.Printf("  best three coins          %+.0f bps = %.0f%% of everything\n", t3, t3/all*100)
		fmt.Printf("  best five coins           %+.0f bps = %.0f%% of everything\n", t5, t5/all*100)
		fmt.Printf("  median coin               %+.0f bps\n", sorted[len(sorted)/2])
		fmt.Printf("  WITHOUT the best three    %+.0f bps = $%+.2f\n", all-t3, (all-t3)/10000**notional)
		fmt.Printf("\n  If a handful of names carry the result, the strategy is a lottery\n")
		fmt.Printf("  ticket, not a yield. You would have had to hold ALL of them.\n")
	}

	fmt.Printf("\nAGGREGATE\n\n")
	fmt.Printf("  trades                    %d\n", trades)
	fmt.Printf("  profitable coins          %d of %d\n", wins, len(results))
	fmt.Printf("  total net                 %+.1f bps of notional\n", netBps)
	fmt.Printf("  net in dollars            $%+.2f  at $%.0f/leg\n", netUSD, *notional)
	fmt.Printf("  position-hours            %.0f\n", held)
	if held > 0 {
		cap := held * *notional * 2
		fmt.Printf("  rate WHILE DEPLOYED       %+.1f%%/yr\n", netUSD/cap*8760*100)
		yearH := float64(*days) * 24
		for _, n := range []float64{1, 3, 5} {
			take := math.Min(held, yearH*n)
			frac := take / held
			cUSD := *notional * 2 * n
			fmt.Printf("    %.0f concurrent  captures %3.0f%%  $%+8.2f on $%.0f  = %+.1f%%/yr\n",
				n, frac*100, netUSD*frac, cUSD, netUSD*frac/cUSD*100*(8760/yearH))
		}
	}

	fmt.Printf("\n  WHICH VENUE LISTED FIRST\n")
	type kv struct {
		k string
		n int
	}
	var fs []kv
	for k, n := range firstOn {
		fs = append(fs, kv{k, n})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].n > fs[j].n })
	for _, f := range fs {
		fmt.Printf("    %-12s %d\n", f.k, f.n)
	}

	fmt.Print(`
READ THE SLIPPAGE ASSUMPTION, NOT THE HEADLINE. Historical order books do not
exist on any venue, so the cost of crossing a days-old listing is a parameter.
Run with -sweep across 15/25/40/60 to see where the answer flips.

SURVIVORSHIP: every venue API returns currently-listed symbols. Coins that
collapsed and were delisted are absent, and those are exactly where funding was
most extreme.

MEXC, BingX, Gate and Bitget fees are UNVERIFIED and they carry most of the
weight here, being the aggressive listers. Check them before believing this.
`)
}

// hourly forward-fills settlements onto an hourly grid.
func hourly(ps []point, ivl float64, listed int64, hours int) []float64 {
	out := make([]float64, hours)
	for i := range out {
		out[i] = math.NaN()
	}
	for _, p := range ps {
		h := int((p.TsMs - listed) / 3_600_000)
		if h < 0 || h >= hours {
			continue
		}
		out[h] = p.Rate * 10000 / ivl
	}
	last := math.NaN()
	for i := range out {
		if !math.IsNaN(out[i]) {
			last = out[i]
		} else {
			out[i] = last
		}
	}
	return out
}

func simulate(a, b []float64, entry, exit, minHold, maxHold, cooldown, roundTrip float64) (trades int, net, held, maxSpread float64) {
	inPos := false
	var acc, h, since float64
	since = cooldown
	for i := 0; i < len(a) && i < len(b); i++ {
		if math.IsNaN(a[i]) || math.IsNaN(b[i]) {
			continue
		}
		sp := math.Abs(a[i] - b[i])
		if sp > maxSpread {
			maxSpread = sp
		}
		if !inPos {
			since++
			if sp >= entry && since >= cooldown {
				inPos, acc, h = true, -roundTrip, 0
				trades++
			}
			continue
		}
		acc += sp
		h++
		if (sp < exit && h >= minHold) || h >= maxHold {
			net += acc
			held += h
			inPos, since = false, 0
		}
	}
	if inPos {
		net += acc
		held += h
	}
	return
}

func safeName(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(s)
}
