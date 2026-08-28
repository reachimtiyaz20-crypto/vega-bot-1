// Command cross runs the cross-venue perp-perp paper book.
//
// SEPARATE FROM cmd/monitor, ON PURPOSE
//
// The cash-and-carry monitor has been accruing since 7 August and holds live
// paper positions. Adding a second strategy to that process would put four
// days of running measurement behind a fresh code path. This runs alongside it,
// writes to its own file, and cannot disturb it.
//
// WHAT A PASS DOES
//
//  1. One Hyperliquid call returns Binance, Bybit and Hyperliquid funding.
//  2. Pairs whose spread cannot repay a round trip are dropped BEFORE any
//     book is read -- book reads are the expensive part.
//  3. Surviving coins have all needed books read and swept at the intended
//     notional. Slippage is MEASURED, never proxied.
//  4. The book opens, accrues and closes against those measurements.
//  5. Every candidate is journaled with its gate decision, so a quiet day can
//     be explained rather than guessed at.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/capital"
	"github.com/imtiyaz/vega-bot/pkg/crossvenue"
	"github.com/imtiyaz/vega-bot/pkg/fees"
	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

func main() {
	dataDir := flag.String("data", "data", "data directory")
	poll := flag.Duration("poll", 10*time.Minute, "how often to run a pass")
	once := flag.Bool("once", false, "run a single pass and exit")

	notional := flag.Float64("notional", 400, "USD per leg; capital deployed is roughly 2x")
	maxConcurrent := flag.Int("max-positions", 5, "concurrent positions")

	minSpread := flag.Float64("min-spread", 1.0, "minimum spread, bps per hour")
	maxBreakEven := flag.Float64("max-breakeven", 24, "refuse pairs needing longer than this to repay, hours")
	maxEntryCost := flag.Float64("max-entry-cost", 40, "refuse entries costing more than this, bps")
	minVol := flag.Float64("min-vol", 10_000_000, "hyperliquid 24h volume floor, USD")
	minPosition := flag.Float64("min-position", 50,
		"smallest position worth running, USD -- a PREFERENCE. Each venue's own "+
			"minimum is applied separately and the larger binds")
	maxBasis := flag.Float64("max-basis", 0, "refuse if venues disagree on price by more than this many bps (0 = OFF)")
	depthBand := flag.Float64("depth-band", 15, "bps from mid that counts as available depth")
	maxCoins := flag.Int("max-coins", 25, "cap on coins measured per pass (venue rate limits)")

	minHold := flag.Float64("min-hold", 4, "minimum hold, hours")
	maxHold := flag.Float64("max-hold", 720, "maximum hold, hours")
	stopLoss := flag.Float64("stop-loss", -60, "close at or below this net, bps")

	hlTaker := flag.Float64("hl-taker", 0, "hyperliquid taker fee, bps")
	binTaker := flag.Float64("bin-taker", 0, "binance futures taker fee, bps")
	bybitTaker := flag.Float64("bybit-taker", 0, "bybit perp taker fee, bps")
	okxTaker := flag.Float64("okx-taker", 0, "okx perp taker fee, bps (UAE regular tier)")
	bitgetTaker := flag.Float64("bitget-taker", 0,
		"bitget futures taker fee, bps -- read it off YOUR account; 0 leaves every bitget pair refused")
	mexcTaker := flag.Float64("mexc-taker", 0,
		"mexc futures taker fee, bps -- read it off YOUR account; 0 leaves every mexc pair refused")
	costsFile := flag.String("costs", "data/listslip/measurements.jsonl",
		"measured fill costs; the entry gate needs a per-pair p95 and refuses pairs without enough samples")
	priorityMinSettled := flag.Float64("priority-min-settled", 1.0,
		"settled spread that earns a coin a LOOK, bps/hr -- attention only; the net-edge gate decides trades")
	minSameSign := flag.Float64("min-same-sign", 0.75,
		"fraction of recent settlements that must share a sign")
	minSettledN := flag.Int("min-settled-intervals", 6,
		"settlements required before a settled spread is trusted")
	universeEvery := flag.Duration("universe-every", 90*time.Minute,
		"how often to rescan every venue pair's SETTLED history; they only change at settlements, so faster buys nothing")
	priorityMax := flag.Int("priority-max", 40,
		"most coins the universe scan may push past the predicted-spread floor")
	deadAfter := flag.Float64("dead-after", 24,
		"close a position that cannot repay what it owes within this many hours at its current spread; 0 disables")
	reversedBleed := flag.Float64("reversed-bleed", 72,
		"close a repaid position whose reversed spread would give back its exit cost within this many hours; 0 disables")
	// ZERO, and the only one here read from an API rather than a fee page.
	// All 210 Lighter markets report taker_fee "0.0000". Kept as a flag so a
	// change can be applied without a rebuild -- and so it is never invisible.
	lighterTaker := flag.Float64("lighter-taker", 0.0, "lighter perp taker fee, bps (API reports 0)")
	feesVerified := flag.Bool("fees-verified", false, "assert the fee pages were read; WITHOUT THIS NOTHING OPENS")

	topN := flag.Int("top", 12, "candidate rows to print")
	feesFile := flag.String("fees", "config/fees.json",
		"taker-fee registry; the single source of truth, with per-venue flags treated as assertions against it")
	capitalConfig := flag.String("capital-config", "config/capital.json",
		"capital budget config; if absent the book runs with NO ceiling")
	capitalBook := flag.String("capital-book", "research",
		"which book in that config this process draws from; empty disables the ceiling")
	flag.Parse()

	cfg := crossvenue.DefaultConfig(*notional)
	cfg.MaxConcurrent = *maxConcurrent
	cfg.MinSpreadBpsHr = *minSpread
	cfg.MaxBreakEvenHours = *maxBreakEven
	cfg.MaxEntryCostBps = *maxEntryCost
	cfg.MinVolUSD = *minVol
	cfg.MinNotionalUSD = *minPosition
	cfg.MaxEntryBasisBps = *maxBasis
	cfg.MinHoldHours = *minHold
	cfg.MaxHoldHours = *maxHold
	cfg.StopLossBps = *stopLoss
	cfg.FeesVerified = *feesVerified
	// FEES COME FROM THE REGISTRY, NOT FROM FLAGS.
	//
	// They lived in per-service flags until 2026-08-20. When OKX was corrected
	// from 25 bps to 5, two services were updated and one was not, so the two
	// cross-venue books priced an identical OKX round trip 40 bps apart and
	// nothing either produced was comparable with the other. An external review
	// found it; the system had no way to notice.
	reg, ferr := fees.Load(*feesFile)
	if ferr != nil {
		fmt.Fprintln(os.Stderr, ferr)
		os.Exit(1)
	}
	cfg.TakerBps = map[string]float64{}
	for v, bps := range reg.TakerBps {
		cfg.TakerBps[v] = bps
	}

	// Per-venue flags survive as a REDUNDANT ASSERTION. A unit file that
	// disagrees with the registry stops the process at startup and names both
	// numbers, rather than quietly pricing a venue differently from every other
	// process on the box.
	for venue, supplied := range map[string]float64{
		"hyperliquid": *hlTaker, "binance": *binTaker, "bybit": *bybitTaker,
		"okx": *okxTaker, "lighter": *lighterTaker,
		"bitget": *bitgetTaker, "mexc": *mexcTaker,
	} {
		if err := reg.Assert(venue, supplied); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Printf("fees from %s (updated %s): %d venues, mexc priced per symbol\n",
		*feesFile, reg.Updated, len(cfg.TakerBps))

	bcfg := crossvenue.DefaultBuilderConfig(*notional)
	bcfg.MinSpreadBpsHr = *minSpread
	bcfg.MinVolUSD = *minVol
	bcfg.DepthBandBps = *depthBand
	bcfg.MaxCoinsToMeasure = *maxCoins

	book, err := crossvenue.NewBook(*dataDir, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL opening book: %v\n", err)
		os.Exit(1)
	}

	// Attach the capital ledger. A nil ledger means no ceiling, which is
	// what this book did before the ledger existed -- so an unconfigured box
	// keeps running rather than failing to start. What it must never do is
	// run unbounded while believing it is bounded, which is why the absent
	// case says so out loud on every start.
	if *capitalBook == "" {
		fmt.Printf("capital: DISABLED by -capital-book=\"\" -- book is UNBOUNDED\n")
	} else if lg, lerr := capital.BookFor(*capitalConfig, *dataDir, *capitalBook); lerr != nil {
		fmt.Fprintf(os.Stderr, "FATAL loading capital book %q: %v\n", *capitalBook, lerr)
		os.Exit(1)
	} else if lg == nil {
		fmt.Printf("capital: %s absent -- book is UNBOUNDED\n", *capitalConfig)
	} else {
		book.Capital = lg
		s := lg.Snapshot(time.Now().UTC())
		fmt.Printf("capital book %q (%s): principal $%.2f, reserve $%.2f, free $%.2f, %d holds carried over\n",
			s.Name, s.Service, s.Principal, s.Reserve, s.Free, s.Positions)
	}
	builder := crossvenue.NewBuilder(bcfg)

	// MEXC prices its taker fee per symbol, so the venue-level map cannot
	// express it. Everything else keeps using Config.TakerBps.
	book.TakerLookup = func(venue, coin string) (float64, bool) {
		if venue != "mexc" {
			return 0, false
		}
		mx, ok := builder.Readers["mexc"].(*orderbook.MEXCPerp)
		if !ok {
			return 0, false
		}
		sym, ok := mx.ResolveCoin(coin)
		if !ok {
			return 0, false
		}
		return mx.TakerBps(sym)
	}
	// Measured costs, not a threshold. A pair with fewer than minCostSamples
	// observations is refused rather than estimated.
	costs, cerr := crossvenue.LoadCosts(*costsFile, *notional)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", cerr)
		os.Exit(1)
	}
	book.Costs = costs
	{
		pairs := costs.Pairs()
		usable := 0
		for _, n := range pairs {
			if n >= 40 {
				usable++
			}
		}
		fmt.Printf("fill costs from %s: %d venue pairs, %d with enough samples to trade\n",
			*costsFile, len(pairs), usable)
	}
	book.MinSettledSameSign = *minSameSign
	book.MinSettledIntervals = *minSettledN
	cachePath := ""
	if *dataDir != "" {
		cachePath = filepath.Join(*dataDir, "settled", "cache.json")
	}
	builder.Settled = crossvenue.NewSettledCache(45*time.Minute, cachePath)
	builder.PriorityMinRecent = *priorityMinSettled
	builder.PriorityMinSameSign = *minSameSign
	builder.PriorityMinIntervals = *minSettledN
	builder.PriorityMax = *priorityMax
	book.DeadAfterHours = *deadAfter
	book.ReversedBleedHours = *reversedBleed

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("VEGA cross-venue  notional $%.0f/leg (capital $%.0f)  poll %s\n",
		*notional, *notional*2, *poll)
	if !*feesVerified {
		fmt.Println()
		fmt.Println("!!! FEES UNVERIFIED. Every entry will refuse.")
		fmt.Println("!!! The edge here is a few bps per hour. A wrong fee does not shrink")
		fmt.Println("!!! the profit, it reverses the sign. Read the three fee pages, then")
		fmt.Println("!!! pass -fees-verified.")
	}
	if *maxBasis <= 0 {
		fmt.Println()
		fmt.Println("NOTE: price-basis gate is OFF. Basis is measured and logged on every")
		fmt.Println("      candidate but not acted on. KAITO showed 136.6 bps between")
		fmt.Println("      Binance and Bybit on 2026-08-12 -- about 5.4 hours of its own")
		fmt.Println("      funding spread. Set -max-basis once the log shows a distribution.")
	}
	fmt.Println()

	fmt.Print("loading venue instrument lists... ")
	if err := builder.LoadSymbols(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\nFATAL: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("ok")
	// The universe scan must start AFTER symbols are loaded -- its first pass
	// reads Symbols() from every reader, and an empty universe scans nothing.
	//
	// Its own context, not the pass context: this outlives any single poll and
	// should keep running while the process does.
	builder.Universe = crossvenue.NewUniverseScanner(builder.Readers, builder.Settled, *dataDir+"/universe/scan.jsonl")
	builder.Universe.Every = *universeEvery
	go builder.Universe.Run(context.Background())
	fmt.Printf("universe scan every %s, up to %d priority coins at >= %.2f bps/hr settled\n",
		*universeEvery, *priorityMax, *priorityMinSettled)

	run := func() {
		if err := pass(ctx, book, builder, *dataDir, *topN); err != nil {
			fmt.Fprintf(os.Stderr, "pass failed: %v\n", err)
		}
	}

	run()
	if *once {
		return
	}

	// Symbol lists are reloaded daily: listings and delistings both matter,
	// and a stale list silently refuses coins that started trading yesterday.
	reload := time.NewTicker(24 * time.Hour)
	defer reload.Stop()
	tick := time.NewTicker(*poll)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nshutting down; book is saved")
			return
		case <-reload.C:
			if err := builder.LoadSymbols(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "symbol reload failed (keeping previous list): %v\n", err)
			}
		case <-tick.C:
			run()
		}
	}
}

// passRecord is one candidate's full reasoning, journaled.
//
// Written whether the candidate passed or not. The refusals are the more
// valuable half: they are the only way to answer "why did nothing open today"
// three weeks from now.
type passRecord struct {
	TsMs int64 `json:"ts_ms"`

	Coin       string `json:"coin"`
	LongVenue  string `json:"long_venue"`
	ShortVenue string `json:"short_venue"`

	SpreadBpsHr float64 `json:"spread_bps_hr"`
	LongBpsHr   float64 `json:"long_bps_hr"`
	ShortBpsHr  float64 `json:"short_bps_hr"`

	EntryCostBps float64 `json:"entry_cost_bps"`
	RoundTripBps float64 `json:"round_trip_bps"`
	BeHours      float64 `json:"be_hours"`

	NotionalUSD float64 `json:"notional_usd"`
	LimitedBy   string  `json:"limited_by,omitempty"`

	LongIntervalHours     float64 `json:"long_interval_hours"`
	ShortIntervalHours    float64 `json:"short_interval_hours"`
	LongIntervalExplicit  bool    `json:"long_interval_explicit"`
	ShortIntervalExplicit bool    `json:"short_interval_explicit"`

	BasisBps      float64 `json:"basis_bps"`
	BasisMeasured bool    `json:"basis_measured"`

	LongDepthUSD  float64 `json:"long_depth_usd"`
	ShortDepthUSD float64 `json:"short_depth_usd"`

	VolUSD float64 `json:"vol_usd"`

	Gate   string `json:"gate"`
	Reason string `json:"reason,omitempty"`
	Viable bool   `json:"viable"`
}

func pass(ctx context.Context, book *crossvenue.Book, builder *crossvenue.Builder,
	dataDir string, topN int) error {

	now := time.Now().UTC()

	// Tell the builder what is already held, so those coins survive the entry
	// spread floor and keep being observed while they decay.
	held := map[string]bool{}
	openPos, _ := book.Snapshot()
	for _, p := range openPos {
		held[p.Coin] = true
	}
	builder.HeldCoins = held

	cands, st, err := builder.Build(ctx)
	if err != nil {
		return err
	}

	// Assess BEFORE Update so the journal captures the reasoning that led to
	// this pass's decisions, not the state after them.
	assessments := make([]crossvenue.Assessment, 0, len(cands))
	for _, c := range cands {
		assessments = append(assessments, book.Assess(c, now))
	}

	res, err := book.Update(now, cands)
	if err != nil {
		return err
	}

	if err := journal(dataDir, now, assessments); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING journaling: %v\n", err)
	}
	// Pairs followed DOWN after they stopped qualifying. Separate file so the
	// candidate log stays exactly what it always was.
	if err := journalDecay(dataDir, now, st.Decaying); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING journaling decay: %v\n", err)
	}

	// --- report ---
	fmt.Printf("\n===== %s UTC  (built in %s) =====\n", now.Format("2006-01-02 15:04:05"), st.Elapsed.Round(time.Millisecond))
	fmt.Printf("%d coins, %d pairs formed -> %d shortlisted -> %d measured; "+
		"%d books read (%d failed, %d unlisted); %d candidates\n",
		st.Coins, st.PairsFormed, st.CoinsShortlist, st.CoinsMeasured,
		st.BooksRead, st.BookFailures, st.SymbolRefusals, st.Candidates)
	fmt.Printf("intervals: %d unresolved (dropped), %d from a venue DEFAULT rather than published\n",
		st.UnresolvedInterval, st.DefaultedInterval)
	// Stage one is where 909 of 910 pairs die, and it was never reported.
	// Seventeen gates downstream pass 66% of what reaches them; ONE threshold
	// here removes almost everything, and it was invisible.
	fmt.Printf("stage 1 drops: %d below the %.2f bps/hr spread floor, %d below the volume floor, %d delisted\n",
		st.DroppedSpread, book.Config().MinSpreadBpsHr, st.DroppedVolume, st.DroppedDelisted)
	if len(st.Decaying) > 0 {
		fmt.Printf("following %d pairs DOWN after they stopped qualifying (decay.jsonl)\n",
			len(st.Decaying))
	}
	for _, w := range st.Warnings {
		fmt.Printf("  WARNING %s\n", w)
	}

	open, closed := book.Snapshot()

	if len(res.Opened) > 0 {
		fmt.Println("\nOPENED")
		for _, o := range res.Opened {
			fmt.Printf("  + %s\n", o)
		}
	}
	if len(res.Closed) > 0 {
		fmt.Println("\nCLOSED")
		for _, c := range res.Closed {
			fmt.Printf("  - %s\n", c)
		}
	}

	if len(open) > 0 {
		fmt.Printf("\nOPEN POSITIONS (%d)  worst first\n", len(open))
		var net, allIn float64
		for _, p := range open {
			net += p.NetUSD()
			allIn += p.AllInNetUSD()
			drift := "basis unmeasured"
			if d, ok := p.BasisDriftBps(); ok {
				drift = fmt.Sprintf("basis %+.1f -> %+.1f bps (%+.1f)", p.EntryBasisBps, p.LastBasisBps, d)
			}
			fmt.Printf("  %s\n      %s\n      %s\n      %s\n",
				p.Describe(now), drift, p.ExitWatch.String(), p.DescribePlan())
		}
		fmt.Printf("  unrealised: funding $%+.2f, all-in incl. basis $%+.2f\n", net, allIn)
	} else {
		fmt.Println("\nNo open positions.")
	}

	if len(closed) > 0 {
		var realised, realisedAllIn float64
		for _, p := range closed {
			realised += p.NetUSD()
			realisedAllIn += p.AllInNetUSD()
		}
		// Basis is banked once a position closes, so the headline is the all-in
		// figure. Reporting only the funding half booked ONG as +$7.02 on
		// 2026-08-21 when it had lost $8.05.
		fmt.Printf("closed: %d, realised $%+.2f  (funding $%+.2f, basis $%+.2f)\n",
			len(closed), realisedAllIn, realised, realisedAllIn-realised)
	}

	// Candidates, best break-even first. Viable rows first.
	sort.Slice(assessments, func(i, j int) bool {
		if assessments[i].Viable != assessments[j].Viable {
			return assessments[i].Viable
		}
		if assessments[i].Viable {
			return assessments[i].BreakEvenHrs < assessments[j].BreakEvenHrs
		}
		return assessments[i].Candidate.SettledRecentSpreadBpsHr > assessments[j].Candidate.SettledRecentSpreadBpsHr
	})

	fmt.Println("\nCANDIDATES")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  COIN\tLONG\tSHORT\tIVL\tBPS/HR\tPRED\tCOST\tBE\tSIZE\tBASIS\tVERDICT")
	for i, a := range assessments {
		if i >= topN {
			break
		}
		c := a.Candidate
		basis := "  n/a"
		if c.BasisMeasured {
			basis = fmt.Sprintf("%+.1f", c.BasisBps)
		}
		verdict := a.Gate
		if a.Viable {
			verdict = "OK"
		}
		// Mark any leg whose interval was assumed rather than published.
		ivl := fmt.Sprintf("%.0f/%.0fh", c.LongIntervalHours, c.ShortIntervalHours)
		if !c.LongIntervalExplicit || !c.ShortIntervalExplicit {
			ivl += "*"
		}
		be, size := "-", "-"
		if a.RoundTripBps > 0 {
			be = fmt.Sprintf("%.1fh", a.BreakEvenHrs)
			size = fmt.Sprintf("$%.0f", a.NotionalUSD)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%+.3f\t%+.3f\t%.1f\t%s\t%s\t%s\t%s\n",
			c.Coin, c.LongVenue, c.ShortVenue, ivl, c.SettledRecentSpreadBpsHr, c.SpreadBpsHr(),
			a.RoundTripBps, be, size, basis, verdict)
	}
	_ = w.Flush()

	if len(res.Refusals) > 0 {
		fmt.Println("\nREFUSALS")
		type kv struct {
			k string
			v int
		}
		var rs []kv
		for k, v := range res.Refusals {
			rs = append(rs, kv{k, v})
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].v > rs[j].v })
		for _, r := range rs {
			fmt.Printf("  %-32s %d\n", r.k, r.v)
		}
	}
	fmt.Println()
	return nil
}

type decayRecord struct {
	TsMs                int64   `json:"ts_ms"`
	Coin                string  `json:"coin"`
	LongVenue           string  `json:"long_venue"`
	ShortVenue          string  `json:"short_venue"`
	SpreadBpsHr         float64 `json:"spread_bps_hr"`
	HoursSinceQualified float64 `json:"hours_since_qualified"`
	BelowFloor          bool    `json:"below_floor"`
}

// journalDecay records pairs that have fallen below the entry floor.
//
// These are NOT candidates and were never assessed. They exist so the decay
// curve has its second half -- the part where a dislocation dies, which
// passes.jsonl has never been able to see.
func journalDecay(dataDir string, now time.Time, obs []crossvenue.DecayObs) error {
	if len(obs) == 0 {
		return nil
	}
	dir := filepath.Join(dataDir, "crossvenue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "decay.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	ms := now.UnixMilli()
	for _, o := range obs {
		if err := enc.Encode(decayRecord{
			TsMs: ms, Coin: o.Coin,
			LongVenue: o.LongVenue, ShortVenue: o.ShortVenue,
			SpreadBpsHr: o.SpreadBpsHr, HoursSinceQualified: o.HoursSinceQualified,
			BelowFloor: true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// journal appends one line per candidate to crossvenue/passes.jsonl.
func journal(dataDir string, now time.Time, as []crossvenue.Assessment) error {
	dir := filepath.Join(dataDir, "crossvenue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "passes.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	ms := now.UnixMilli()
	for _, a := range as {
		c := a.Candidate
		if err := enc.Encode(passRecord{
			TsMs:                  ms,
			Coin:                  c.Coin,
			LongVenue:             c.LongVenue,
			ShortVenue:            c.ShortVenue,
			SpreadBpsHr:           c.SpreadBpsHr(),
			LongBpsHr:             c.LongBpsHr,
			ShortBpsHr:            c.ShortBpsHr,
			EntryCostBps:          a.EntryCostBps,
			RoundTripBps:          a.RoundTripBps,
			BeHours:               a.BreakEvenHrs,
			NotionalUSD:           a.NotionalUSD,
			LimitedBy:             a.LimitedBy,
			LongIntervalHours:     c.LongIntervalHours,
			ShortIntervalHours:    c.ShortIntervalHours,
			LongIntervalExplicit:  c.LongIntervalExplicit,
			ShortIntervalExplicit: c.ShortIntervalExplicit,
			BasisBps:              c.BasisBps,
			BasisMeasured:         c.BasisMeasured,
			LongDepthUSD:          c.LongDepthUSD,
			ShortDepthUSD:         c.ShortDepthUSD,
			VolUSD:                c.VolUSD,
			Gate:                  a.Gate,
			Reason:                a.Reason,
			Viable:                a.Viable,
		}); err != nil {
			return err
		}
	}
	return nil
}
