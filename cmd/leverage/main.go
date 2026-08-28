// Command leverage replays recorded positions against real price history and
// reports how many would have been liquidated at each leverage.
//
// IT TOUCHES NOTHING. Reads position files, fetches public price and risk
// data, prints, exits. The running books cannot be affected.
//
// THREE KINDS OF SUBJECT
//
//	cross       the cross-venue positions the book actually opened. Two perps,
//	            two venues, no netting. Real entries and real funding.
//	cand        every pair the cross-venue gate ever passed, treated as a
//	            hypothetical opened then and held -hold hours. Assumes the
//	            spread held, which FLATTERS it -- a liquidation count from this
//	            source is a floor.
//	cash        the cash-and-carry book: long SPOT, short PERP, one venue. The
//	            spot is owned outright, so it cannot be liquidated and carries
//	            no maintenance margin. Only the perp can die.
//
// # WHY TWO PRICE SERIES
//
// Pricing both legs off one series makes them cancel exactly, so a netted
// hedge can never lose equity and "never liquidated" is arithmetic rather than
// a finding. Spot and mark are fetched separately so the basis between them is
// measured. That basis is the only thing that can kill a hedged position.
//
// WHY EVERY LEG USES BYBIT'S MARGIN RULES
//
// Binance's maintenance tiers require an authenticated request. Rather than
// guess, every leg is modelled on Bybit's published rules -- the same or
// stricter on these symbols, so the error runs toward caution.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/margin"
	"github.com/imtiyaz/vega-bot/pkg/replay"
)

type kind int

const (
	kindCross kind = iota
	kindCand
	kindCash
)

func (k kind) String() string {
	switch k {
	case kindCash:
		return "CASH-AND-CARRY (spot + perp, one venue)"
	case kindCand:
		return "CROSS-VENUE CANDIDATES (hypothetical)"
	}
	return "CROSS-VENUE POSITIONS (real)"
}

type item struct {
	sub  replay.Subject
	kind kind
}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	source := flag.String("source", "all", "all | cross | candidates | cash")
	holdHours := flag.Float64("hold", 24, "hours to hold a hypothetical candidate")
	levList := flag.String("levs", "1,2,3,5,10", "leverages to test")
	maxSubjects := flag.Int("max", 400, "cap on hypothetical subjects")
	verbose := flag.Bool("v", false, "print every replay")
	flag.Parse()

	levs := parseLevs(*levList)
	if len(levs) == 0 {
		fmt.Fprintln(os.Stderr, "no valid leverages")
		os.Exit(1)
	}
	want := func(k string) bool { return *source == "all" || *source == k }

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	var items []item

	if want("cross") {
		s, err := loadCross(*dataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING cross positions: %v\n", err)
		}
		for _, x := range s {
			items = append(items, item{x, kindCross})
		}
		fmt.Printf("cross-venue positions: %d\n", len(s))
	}
	if want("candidates") {
		s, err := loadCandidates(*dataDir, *holdHours, *maxSubjects)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING candidates: %v\n", err)
		}
		for _, x := range s {
			items = append(items, item{x, kindCand})
		}
		fmt.Printf("cross-venue candidates: %d (held %.0fh)\n", len(s), *holdHours)
	}
	if want("cash") {
		s, err := loadCashCarry(*dataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING cash-and-carry: %v\n", err)
		}
		for _, x := range s {
			items = append(items, item{x, kindCash})
		}
		fmt.Printf("cash-and-carry positions: %d\n", len(s))
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to replay")
		os.Exit(1)
	}

	// --- risk schedules ---
	symbols := map[string]bool{}
	needSpot := map[string]bool{}
	for _, it := range items {
		symbols[it.sub.Long.Symbol] = true
		symbols[it.sub.Short.Symbol] = true
		if it.sub.LongIsSpot {
			needSpot[it.sub.Long.Symbol] = true
		}
	}

	fmt.Printf("\nbybit risk limits for %d symbols... ", len(symbols))
	risk := margin.NewBybitRisk()
	schedules := map[string]margin.Schedule{}
	var refused []string
	for sym := range symbols {
		sch, err := risk.Fetch(ctx, sym)
		if err != nil {
			// The reason matters. BTCUSDT fetched cleanly in isolation and was
			// refused inside a batch on 2026-08-14, which is a rate limit, not
			// a missing symbol -- and swallowing the error made those look
			// identical.
			refused = append(refused, fmt.Sprintf("%s: %v", sym, err))
			continue
		}
		for _, v := range []string{"bybit", "binance", "hyperliquid"} {
			s2 := sch
			s2.Venue = v
			schedules[v+"|"+sym] = s2
		}
		time.Sleep(70 * time.Millisecond)
	}
	fmt.Printf("%d ok, %d refused\n", len(symbols)-len(refused), len(refused))
	for _, r := range refused {
		fmt.Printf("  REFUSED %s\n", r)
	}

	// --- price history ---
	kl := replay.New()
	markBy := map[string]replay.Series{}
	spotBy := map[string]replay.Series{}

	fmt.Println("\nfetching 1m price history")
	for sym := range symbols {
		from, to := window(items, sym)
		if from.IsZero() {
			continue
		}
		if ser, err := kl.MarkKlines(ctx, sym, from.Add(-2*time.Minute), to.Add(2*time.Minute)); err != nil {
			fmt.Printf("  WARNING mark %s: %v\n", sym, err)
		} else {
			markBy[sym] = ser
			f, t, _ := ser.Span()
			fmt.Printf("  %-12s mark %5d candles  %s -> %s\n", sym, len(ser.Candles),
				f.Format("01-02 15:04"), t.Format("01-02 15:04"))
		}
		if needSpot[sym] {
			if ser, err := kl.SpotKlines(ctx, sym, from.Add(-2*time.Minute), to.Add(2*time.Minute)); err != nil {
				fmt.Printf("  WARNING spot %s: %v\n", sym, err)
			} else {
				spotBy[sym] = ser
				f, t, _ := ser.Span()
				fmt.Printf("  %-12s spot %5d candles  %s -> %s\n", sym, len(ser.Candles),
					f.Format("01-02 15:04"), t.Format("01-02 15:04"))
			}
		}
	}

	// --- price each leg from ITS OWN series ---
	// The gap between them is the basis, and the basis is the whole point.
	var priced []item
	dropped := 0
	for _, it := range items {
		lSer, lOk := seriesFor(it.sub, true, markBy, spotBy)
		sSer, sOk := seriesFor(it.sub, false, markBy, spotBy)
		if !lOk || !sOk {
			dropped++
			continue
		}
		lp, ok1 := markAt(lSer, it.sub.OpenedAt)
		sp, ok2 := markAt(sSer, it.sub.OpenedAt)
		if !ok1 || !ok2 {
			dropped++
			continue
		}
		it.sub.Long.EntryPrice = lp
		it.sub.Short.EntryPrice = sp
		priced = append(priced, it)
	}
	fmt.Printf("\npriced %d of %d subjects (%d dropped for missing history)\n\n",
		len(priced), len(items), dropped)

	// --- replay ---
	type key struct {
		k   kind
		m   replay.Mode
		lev float64
	}
	results := map[key][]replay.Result{}
	skipped := map[string]int{}

	for _, it := range priced {
		lSer, _ := seriesFor(it.sub, true, markBy, spotBy)
		sSer, _ := seriesFor(it.sub, false, markBy, spotBy)

		// Cash-and-carry is one venue by definition, so only portfolio applies.
		modes := []replay.Mode{replay.ModeIsolated, replay.ModePortfolio}
		if it.kind == kindCash {
			modes = []replay.Mode{replay.ModePortfolio}
		}

		for _, m := range modes {
			for _, lev := range levs {
				r := replay.ReplayPair(it.sub, lSer, sSer, schedules, lev, m)
				results[key{it.kind, m, lev}] = append(results[key{it.kind, m, lev}], r)
				if !r.Ok {
					skipped[r.Err]++
				} else if *verbose {
					fmt.Println("  " + r.Describe())
				}
			}
		}
	}

	// --- report ---
	for _, k := range []kind{kindCash, kindCross, kindCand} {
		modes := []replay.Mode{replay.ModeIsolated, replay.ModePortfolio}
		if k == kindCash {
			modes = []replay.Mode{replay.ModePortfolio}
		}
		for _, m := range modes {
			var rows []replay.Summary
			for _, lev := range levs {
				rs := results[key{k, m, lev}]
				if len(rs) == 0 {
					continue
				}
				rows = append(rows, replay.Summarise(rs, lev, m))
			}
			if len(rows) == 0 {
				continue
			}
			fmt.Printf("\n===== %s\n      %s\n", k, m)
			fmt.Printf("%-6s %8s %10s %16s %12s %11s %12s\n",
				"LEV", "TESTED", "SURVIVED", "LIQUIDATED", "NET $", "RETURN %", "WORST MARGIN")
			fmt.Println(strings.Repeat("-", 82))
			for _, s := range rows {
				liq := 0.0
				if s.Total > 0 {
					liq = float64(s.Liquidated) / float64(s.Total) * 100
				}
				worst := fmt.Sprintf("%.1f%%", s.WorstRatio*100)
				if s.AnyLiquidated {
					worst += " of survivors"
				}
				fmt.Printf("%-6.0f %8d %10d %10d (%3.0f%%) %12.2f %10.2f%% %14s\n",
					s.Leverage, s.Total, s.Survived, s.Liquidated, liq,
					s.NetUSD, s.ReturnPct, worst)
			}
		}
	}

	if len(skipped) > 0 {
		fmt.Println("\nSKIPPED")
		for reason, n := range skipped {
			fmt.Printf("  %5d  %s\n", n, reason)
		}
	}

	fmt.Println(`
RETURN % is per position over its own holding period -- 24h for hypotheticals,
actual duration for real positions. It is NOT annualised and the windows
overlap, so it cannot be compounded.

NET $ counts a survivor at the funding actually recorded and a casualty at the
margin it lost. It does NOT model what happens after a cross-venue liquidation,
when the surviving leg is left naked and directional. Real outcomes are worse.`)
}

// --- loaders ------------------------------------------------------------------

func loadCross(dir string) ([]replay.Subject, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "cross_positions.json"))
	if err != nil {
		return nil, err
	}
	var f struct {
		Open   map[string]*crossPos `json:"open"`
		Closed []*crossPos          `json:"closed"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	var out []replay.Subject
	add := func(p *crossPos) {
		if p == nil || p.Notional <= 0 {
			return
		}
		sym := strings.ToUpper(p.Coin) + "USDT"
		out = append(out, replay.Subject{
			Label:       fmt.Sprintf("%s %s/%s", p.Coin, p.LongVenue, p.ShortVenue),
			Long:        replay.LegSpec{Venue: p.LongVenue, Symbol: sym},
			Short:       replay.LegSpec{Venue: p.ShortVenue, Symbol: sym},
			NotionalUSD: p.Notional,
			OpenedAt:    p.OpenedAt,
			ClosedAt:    p.ClosedAt,
			FundingBps:  p.LongLeg + p.ShortLeg,
			CostBps:     p.EntryCost + p.ExitCost,
		})
	}
	for _, p := range f.Open {
		add(p)
	}
	for _, p := range f.Closed {
		add(p)
	}
	return out, nil
}

type crossPos struct {
	Coin       string    `json:"coin"`
	LongVenue  string    `json:"long_venue"`
	ShortVenue string    `json:"short_venue"`
	OpenedAt   time.Time `json:"opened_at"`
	ClosedAt   time.Time `json:"closed_at"`
	Notional   float64   `json:"notional_usd"`
	LongLeg    float64   `json:"long_leg_bps"`
	ShortLeg   float64   `json:"short_leg_bps"`
	EntryCost  float64   `json:"entry_cost_bps"`
	ExitCost   float64   `json:"exit_cost_bps"`
}

type cashPos struct {
	Venue     string    `json:"venue"`
	Symbol    string    `json:"symbol"`
	OpenedAt  time.Time `json:"opened_at"`
	ClosedAt  time.Time `json:"closed_at"`
	Notional  float64   `json:"notional_usd"`
	Funding   float64   `json:"funding_collected_bps"`
	EntryCost float64   `json:"entry_cost_bps"`
	ExitCost  float64   `json:"exit_cost_bps"`
}

func loadCashCarry(dir string) ([]replay.Subject, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "positions.json"))
	if err != nil {
		return nil, err
	}
	var f struct {
		Open   map[string]*cashPos `json:"open"`
		Closed []*cashPos          `json:"closed"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}

	var out []replay.Subject
	add := func(p *cashPos) {
		if p == nil || p.Notional <= 0 {
			return
		}
		exit := p.ExitCost
		if exit == 0 {
			// Open positions have not paid the exit yet; the book carries it
			// as symmetric with entry and so does this.
			exit = p.EntryCost
		}
		var closed time.Time
		if p.ClosedAt.Year() > 1 {
			closed = p.ClosedAt
		}
		out = append(out, replay.Subject{
			Label:       fmt.Sprintf("%s %s spot+perp", p.Symbol, p.Venue),
			Long:        replay.LegSpec{Venue: p.Venue, Symbol: p.Symbol}, // SPOT
			Short:       replay.LegSpec{Venue: p.Venue, Symbol: p.Symbol}, // PERP
			NotionalUSD: p.Notional,
			OpenedAt:    p.OpenedAt,
			ClosedAt:    closed,
			FundingBps:  p.Funding,
			CostBps:     p.EntryCost + exit,
			LongIsSpot:  true,
		})
	}
	for _, p := range f.Open {
		add(p)
	}
	for _, p := range f.Closed {
		add(p)
	}
	return out, nil
}

type passRec struct {
	TsMs        int64   `json:"ts_ms"`
	Coin        string  `json:"coin"`
	LongVenue   string  `json:"long_venue"`
	ShortVenue  string  `json:"short_venue"`
	SpreadBpsHr float64 `json:"spread_bps_hr"`
	RoundTrip   float64 `json:"round_trip_bps"`
	Notional    float64 `json:"notional_usd"`
	Viable      bool    `json:"viable"`
}

func loadCandidates(dir string, hold float64, max int) ([]replay.Subject, error) {
	f, err := os.Open(filepath.Join(dir, "crossvenue", "passes.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := map[string]bool{}
	var out []replay.Subject
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		var r passRec
		if json.Unmarshal(sc.Bytes(), &r) != nil || !r.Viable || r.Notional <= 0 {
			continue
		}
		k := fmt.Sprintf("%s|%s|%s|%d", r.Coin, r.LongVenue, r.ShortVenue, r.TsMs/3_600_000)
		if seen[k] {
			continue
		}
		seen[k] = true

		open := time.UnixMilli(r.TsMs).UTC()
		sym := strings.ToUpper(r.Coin) + "USDT"
		out = append(out, replay.Subject{
			Label:       fmt.Sprintf("%s %s/%s %s", r.Coin, r.LongVenue, r.ShortVenue, open.Format("01-02 15:04")),
			Long:        replay.LegSpec{Venue: r.LongVenue, Symbol: sym},
			Short:       replay.LegSpec{Venue: r.ShortVenue, Symbol: sym},
			NotionalUSD: r.Notional,
			OpenedAt:    open,
			ClosedAt:    open.Add(time.Duration(hold * float64(time.Hour))),
			FundingBps:  r.SpreadBpsHr * hold,
			CostBps:     r.RoundTrip,
		})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// --- helpers ------------------------------------------------------------------

func seriesFor(s replay.Subject, isLong bool, mark, spot map[string]replay.Series) (replay.Series, bool) {
	sym := s.Short.Symbol
	if isLong {
		sym = s.Long.Symbol
		if s.LongIsSpot {
			ser, ok := spot[sym]
			return ser, ok
		}
	}
	ser, ok := mark[sym]
	return ser, ok
}

func window(items []item, sym string) (from, to time.Time) {
	now := time.Now().UTC()
	for _, it := range items {
		if it.sub.Long.Symbol != sym && it.sub.Short.Symbol != sym {
			continue
		}
		end := it.sub.ClosedAt
		if end.IsZero() || end.After(now) {
			end = now
		}
		if from.IsZero() || it.sub.OpenedAt.Before(from) {
			from = it.sub.OpenedAt
		}
		if to.IsZero() || end.After(to) {
			to = end
		}
	}
	return from, to
}

func markAt(s replay.Series, t time.Time) (float64, bool) {
	ms := t.UnixMilli()
	for _, c := range s.Candles {
		if c.TsMs >= ms {
			return c.Open, true
		}
	}
	return 0, false
}

func parseLevs(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	sort.Float64s(out)
	return out
}
