// Command listslip measures what a cross-venue entry would ACTUALLY cost.
//
// # WHY THIS EXISTS
//
// The new-listing backtest breaks even at ~121 bps of round-trip cost and
// showed +45%/yr at 40 bps. Nothing in the historical record can tell us where
// the real number falls, because no venue stores historical order books. So it
// is a parameter, and it is the parameter that decides the whole answer.
//
// This measures it forward on live books. No orders are placed. For every pair
// whose funding spread clears the threshold, both venues' books are read and
// swept at the intended size, and the cost that WOULD have been paid is
// recorded. Two weeks of this replaces the assumption with a distribution.
//
// # WHY IT DOES NOT WAIT FOR NEW LISTINGS
//
// It measures every pair above the threshold, listing or not. High spreads are
// dominated by new listings anyway, and waiting for one wastes days. The coin's
// age is recorded where known; the fill cost is useful either way.
//
// KNOWN GAP: there are no clients for MEXC, Gate or BingX, so bitget/mexc --
// which supplied several of the backtest's biggest winners -- cannot be
// measured here yet.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/fees"
	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

type rate struct {
	symbol   string
	bpsHr    float64
	interval float64
}

type measurement struct {
	TS          string  `json:"ts"`
	Coin        string  `json:"coin"`
	VenueA      string  `json:"venue_a"`
	VenueB      string  `json:"venue_b"`
	SymbolA     string  `json:"symbol_a"`
	SymbolB     string  `json:"symbol_b"`
	SpreadBpsHr float64 `json:"spread_bps_hr"`
	IntervalA   float64 `json:"interval_a_h"`
	IntervalB   float64 `json:"interval_b_h"`
	SlipABps    float64 `json:"slip_a_bps"`
	SlipBBps    float64 `json:"slip_b_bps"`
	SlipTotal   float64 `json:"slip_total_bps"`
	FeesBps     float64 `json:"fees_bps"`
	CostBps     float64 `json:"cost_total_bps"`
	HoursRepay  float64 `json:"hours_to_repay"`
	DepthAUSD   float64 `json:"depth_a_usd"`
	DepthBUSD   float64 `json:"depth_b_usd"`
	NotionalUSD float64 `json:"notional_usd"`
	FeesKnown   bool    `json:"fees_known"`
}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	notional := flag.Float64("notional", 400, "USD per leg -- the size actually swept")
	minSpread := flag.Float64("min-spread", 10, "only measure pairs above this, bps/hr")
	maxPairs := flag.Int("max-pairs", 12, "book reads per run (venue rate limits)")
	sizes := flag.String("sizes", "50,100,400,1000",
		"notionals swept per book read; the READ is the rate-limited part, the sweep is arithmetic")
	coverageTarget := flag.Int("coverage-target", 40,
		"sample floor per venue pair, matching crossvenue minCostSamples; "+
			"pairs below it are measured regardless of spread")
	depthBand := flag.Float64("depth-band", 15, "bps from mid counted as available depth")
	feesVerified := flag.Bool("fees-verified", false, "assert the fee pages were read; WITHOUT THIS NOTHING OPENS")
	feesFile := flag.String("fees", "config/fees.json", "taker-fee registry")
	flag.Parse()

	sizeList, err := parseSizes(*sizes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// -notional is superseded by -sizes, which sweeps several. The flag is
	// kept so the existing unit file keeps parsing; removing it would break
	// a running service for no gain.
	_ = notional

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	reg, ferr := fees.Load(*feesFile)
	if ferr != nil {
		fmt.Fprintln(os.Stderr, ferr)
		os.Exit(1)
	}

	if !*feesVerified {
		fmt.Println()
		fmt.Println("!!! FEES UNVERIFIED. Every entry will refuse.")
		fmt.Println("!!! The edge here is a few bps per hour. A wrong fee does not shrink")
		fmt.Println("!!! the profit, it reverses the sign. Read the three fee pages, then")
		fmt.Println("!!! pass -fees-verified.")
		os.Exit(1)
	}

	readers := orderbook.Readers()

	for name, r := range readers {
		if err := r.LoadSymbols(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "load %s: %v\n", name, err)
		}
	}

	rates := map[string]map[string]rate{}
	add := func(coin, venue, sym string, bpsHr, ivl float64) {
		if ivl <= 0 || coin == "" {
			return // an unnormalisable rate is not a rate
		}
		if rates[coin] == nil {
			rates[coin] = map[string]rate{}
		}
		rates[coin][venue] = rate{sym, bpsHr, ivl}
	}

	if v, ok := readers["lighter"].(*orderbook.LighterPerp); ok {
		// LIGHTER QUOTES PER 8 HOURS AND SETTLES HOURLY.
		//
		// Both conversions are already done inside the reader and neither should
		// be redone here: LighterFunding.BpsPerHour is the quoted per-8h rate
		// converted, and FundingIntervalHours reports the SETTLEMENT cadence
		// rather than the quoting period. Reading either one raw is an 8x error,
		// and that exact error produced two stop-losses on 2026-08-12.
		//
		// Lighter's symbols are bare coin names, so symbol and coin are the same
		// string -- unlike every venue below, which carries a USDT suffix.
		n := 0
		for sym, f := range v.Fundings() {
			iv := v.FundingIntervalHours(sym)
			if !iv.Ok {
				// An unknown settlement cadence is not a rate. Guessing one is
				// how the interval trap works.
				continue
			}
			add(sym, "lighter", sym, f.BpsPerHour, iv.Hours)
			n++
		}
		fmt.Fprintf(os.Stderr, "lighter: %d usable rates (zero taker fees, $10 minimum)\n", n)
	}
	if v, ok := readers["binance"].(*orderbook.BinancePerp); ok {
		if fs, err := v.Fundings(ctx); err == nil {
			for sym, f := range fs {
				if strings.HasSuffix(sym, "USDT") {
					add(strings.TrimSuffix(sym, "USDT"), "binance", sym, f.Rate*10000/f.IntervalHours, f.IntervalHours)
				}
			}
		}
	}
	if v, ok := readers["bybit"].(*orderbook.BybitPerp); ok {
		if fs, err := v.Fundings(ctx); err == nil {
			for sym, f := range fs {
				if strings.HasSuffix(sym, "USDT") {
					add(strings.TrimSuffix(sym, "USDT"), "bybit", sym, f.Rate*10000/f.IntervalHours, f.IntervalHours)
				}
			}
		}
	}
	if v, ok := readers["okx"].(*orderbook.OKXPerp); ok {
		for instID, f := range v.Fundings() {
			add(strings.SplitN(instID, "-", 2)[0], "okx", instID, f.BpsPerHour(), f.IntervalHours)
		}
	}
	if v, ok := readers["bitget"].(*orderbook.BitgetPerp); ok {
		for sym, f := range v.Fundings() {
			add(strings.TrimSuffix(sym, "USDT"), "bitget", sym, f.BpsPerHour(), f.IntervalHours)
		}
	}
	if v, ok := readers["mexc"].(*orderbook.MEXCPerp); ok {
		// MEXC publishes collectCycle and nextSettleTime per symbol, and the
		// bulk ticker carries neither. So the interval has to be hydrated
		// before any rate is meaningful. Capped at 250 per run: 1,116 symbols
		// at 120ms would be two minutes, and the cache warms up over a few
		// passes instead of stalling one.
		//
		// Fundings() returns ONLY hydrated symbols, so an un-hydrated MEXC
		// rate is silently absent rather than silently wrong.
		n := v.EnsureFundingMeta(ctx, v.Symbols(), 0, 250)
		fs := v.Fundings()
		fmt.Fprintf(os.Stderr, "mexc: %d symbols, %d hydrated this run, %d usable rates\n",
			v.SymbolCount(), n, len(fs))
		for sym, f := range fs {
			add(strings.TrimSuffix(sym, "_USDT"), "mexc", sym, f.BpsPerHour(), f.IntervalHours)
		}
	}

	type cand struct {
		coin, a, b string
		ra, rb     rate
		spread     float64
	}
	// What is already measured decides what is worth measuring now.
	covPath := filepath.Join(*dataDir, "listslip", "measurements.jsonl")
	coverage, cerr := loadCoverage(covPath)
	if cerr != nil {
		fmt.Fprintln(os.Stderr, cerr)
		os.Exit(1)
	}
	reportCoverage(coverage, *coverageTarget)

	var cands []cand
	for coin, vm := range rates {
		names := make([]string, 0, len(vm))
		for v := range vm {
			if _, canRead := readers[v]; canRead {
				names = append(names, v)
			}
		}
		sort.Strings(names)
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				a, b := names[i], names[j]
				sp := vm[a].bpsHr - vm[b].bpsHr
				if sp < 0 {
					a, b, sp = b, a, -sp
				}
				// The spread floor applies only to pairs that are already
				// covered. What a fill costs is a property of the order book,
				// not of the funding rate, so a pair we have never measured
				// teaches us as much at 0.5 bps/hr as at 50 -- and eleven pairs
				// sat at zero samples for seven days because the floor kept
				// them out while bitget|bybit was measured 1,298 times.
				if sp >= *minSpread || coverage[pairKey(a, b)] < *coverageTarget {
					cands = append(cands, cand{coin, a, b, vm[a], vm[b], sp})
				}
			}
		}
	}
	// Least-covered venue pair first; spread only breaks ties. Ranking by
	// spread alone is what let one pair take 40% of every sample while the
	// book refused 927 candidates a day for want of data on the others.
	sort.Slice(cands, func(i, j int) bool {
		ci := coverage[pairKey(cands[i].a, cands[i].b)]
		cj := coverage[pairKey(cands[j].a, cands[j].b)]
		if ci != cj {
			return ci < cj
		}
		return cands[i].spread > cands[j].spread
	})

	// One read per venue pair per run. Without this the least-covered pair
	// wins every slot with a different coin each time, which fills one pair
	// and starves the rest -- the original bug wearing a new hat.
	{
		seen := map[string]bool{}
		spread := cands[:0]
		for _, c := range cands {
			k := pairKey(c.a, c.b)
			if seen[k] {
				continue
			}
			seen[k] = true
			spread = append(spread, c)
		}
		cands = spread
	}
	if len(cands) > *maxPairs {
		cands = cands[:*maxPairs]
	}

	outDir := filepath.Join(*dataDir, "listslip")
	os.MkdirAll(outDir, 0o755)
	f, err := os.OpenFile(filepath.Join(outDir, "measurements.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	fmt.Printf("%s  %d coins, %d venue pairs selected, sweeping each at $%s/leg\n\n",
		time.Now().UTC().Format(time.RFC3339), len(rates), len(cands), *sizes)
	fmt.Printf("%-12s %-9s %-9s %8s %8s %8s %8s %9s %10s\n",
		"COIN", "LONG", "SHORT", "bps/hr", "SLIP A", "SLIP B", "FEES", "COST", "REPAY h")
	fmt.Println(strings.Repeat("-", 92))

	for _, c := range cands {
		bkA, errA := readers[c.a].Book(ctx, c.ra.symbol)
		time.Sleep(150 * time.Millisecond)
		bkB, errB := readers[c.b].Book(ctx, c.rb.symbol)
		if errA != nil || errB != nil {
			fmt.Printf("%-12s %-9s %-9s   book unreadable\n", c.coin, c.a, c.b)
			continue
		}
		dA, _, _ := bkA.DepthWithinBps(*depthBand)
		dB, _, _ := bkB.DepthWithinBps(*depthBand)

		tA, knownA := getTakerFor(c.a, c.ra.symbol, reg, readers)
		tB, knownB := getTakerFor(c.b, c.rb.symbol, reg, readers)
		fees := 2 * (tA + tB)

		// ONE BOOK READ, MANY SIZES. Fetching the book is the expensive part
		// and the only part a rate limit sees; sweeping it again at another
		// notional is arithmetic on bytes already in memory. Four sizes cost
		// nothing extra and produce the capacity curve as a by-product.
		//
		// This matters more than it sounds: every sample taken before today
		// was at $400/leg, and the books now trade $50. A cost table measured
		// at one size cannot price a position at another.
		for _, size := range sizeList {
			slipA, okA := bkA.RoundTripSlippageBps(size)
			slipB, okB := bkB.RoundTripSlippageBps(size)
			if !okA || !okB {
				// Too thin AT THIS SIZE, which is itself a measurement: it says
				// where the book stops being able to fill. A table holding only
				// fillable prices hides exactly that.
				fmt.Printf("%-12s %-9s %-9s %8.3f   TOO THIN AT $%.0f\n",
					c.coin, c.a, c.b, c.spread, size)
				continue
			}
			cost := slipA + slipB + fees
			repay := 0.0
			if c.spread > 0 {
				repay = cost / c.spread
			}
			m := measurement{
				TS: time.Now().UTC().Format(time.RFC3339), Coin: c.coin,
				VenueA: c.a, VenueB: c.b, SymbolA: c.ra.symbol, SymbolB: c.rb.symbol,
				SpreadBpsHr: c.spread, IntervalA: c.ra.interval, IntervalB: c.rb.interval,
				SlipABps: slipA, SlipBBps: slipB, SlipTotal: slipA + slipB,
				FeesBps: fees, CostBps: cost, HoursRepay: repay,
				DepthAUSD: dA, DepthBUSD: dB, NotionalUSD: size,
				FeesKnown: knownA && knownB,
			}
			enc.Encode(m)
			flag := ""
			if !m.FeesKnown {
				flag = "  (fees unverified)"
			}
			fmt.Printf("%-12s %-9s %-9s %8.3f %8.2f %8.2f %8.1f %9.1f %10.1f  $%-6.0f%s\n",
				c.coin, c.a, c.b, c.spread, slipA, slipB, fees, cost, repay, size, flag)
		}
	}

	fmt.Print(`
COST is the FULL round trip: sweeping both books in and out, plus four taker
fees. Compare it against 121 bps -- the level at which the new-listing backtest
breaks even. REPAY h is how long the current spread needs to survive to cover
it, against a measured spread half-life of about 12 hours.
`)
}

func getTakerFor(venue, sym string, reg *fees.Registry, readers map[string]orderbook.PerpReader) (float64, bool) {
	if venue == "mexc" {
		type symbolTaker interface {
			TakerBps(symbol string) (float64, bool)
		}
		if v, ok := readers["mexc"].(symbolTaker); ok {
			if bps, ok := v.TakerBps(sym); ok {
				return bps, true
			}
		}
		return 0, false
	}
	return reg.Taker(venue)
}
