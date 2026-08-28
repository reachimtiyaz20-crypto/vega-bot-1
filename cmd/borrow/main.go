// Command borrow records borrow rates and reports what they mean for the book.
//
// IT TOUCHES NOTHING. Fetches public rates, journals them, reads positions
// read-only, prints, exits.
//
// THE POINT IS THE THIRD SECTION. Rates on their own are trivia. What matters
// is (funding − borrow) per position, because that difference is what leverage
// multiplies. A symbol earning 2.4% against a 2.8% borrow LOSES money at any
// leverage, and until now nothing in VEGA could say so.
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
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/borrow"
)

func main() {
	dataDir := flag.String("data", "data", "data directory")
	ccyList := flag.String("currencies", "USDT,USDC,BTC,ETH", "currencies to read")
	record := flag.Bool("record", false, "append this snapshot to the journal")
	levList := flag.String("levs", "1,3,5,10", "leverages to model")
	quiet := flag.Bool("q", false, "record only, no report")
	flag.Parse()

	ccys := split(*ccyList)
	levs := parseFloats(*levList)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	snap := borrow.Collect(ctx, ccys)

	if *record {
		if err := journal(*dataDir, snap); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING journaling: %v\n", err)
		}
	}
	if *quiet {
		return
	}

	// --- 1. live rates ---
	fmt.Printf("BORROW RATES  %s UTC\n\n", snap.At.Format("2006-01-02 15:04:05"))
	fmt.Printf("%-8s %-8s %14s %12s %14s %12s\n",
		"VENUE", "CCY", "PUBLISHED", "PERIOD", "ANNUAL", "MAX BORROW")
	fmt.Println(strings.Repeat("-", 74))

	sort.Slice(snap.Rates, func(i, j int) bool {
		if snap.Rates[i].Currency != snap.Rates[j].Currency {
			return snap.Rates[i].Currency < snap.Rates[j].Currency
		}
		return snap.Rates[i].AnnualPct < snap.Rates[j].AnnualPct
	})
	for _, r := range snap.Rates {
		mark := ""
		if !r.Borrowable {
			mark = "  NOT BORROWABLE"
		}
		fmt.Printf("%-8s %-8s %14.10f %12s %13.3f%% %12.0f%s\n",
			r.Venue, r.Currency, r.RawRate, r.PeriodS, r.AnnualPct, r.MaxBorrowUSD, mark)
	}
	for _, e := range snap.Errs {
		fmt.Printf("\n  !! %s\n", e)
	}

	best, haveBest := snap.Cheapest("USDT")
	if haveBest {
		fmt.Printf("\ncheapest USDT: %s at %.3f%% a year\n", best.Venue, best.AnnualPct)
	}

	// --- 2. history ---
	if hist, err := readJournal(*dataDir); err == nil && len(hist) > 1 {
		fmt.Println("\nUSDT BORROW HISTORY")
		fmt.Printf("%-8s %8s %10s %10s %10s %10s\n", "VENUE", "READINGS", "MIN", "MEDIAN", "MAX", "LATEST")
		fmt.Println(strings.Repeat("-", 60))
		for _, venue := range []string{"okx", "bybit"} {
			var vals []float64
			var latest float64
			for _, s := range hist {
				for _, r := range s.Rates {
					if r.Venue == venue && r.Currency == "USDT" && r.Ok {
						vals = append(vals, r.AnnualPct)
						latest = r.AnnualPct
					}
				}
			}
			if len(vals) == 0 {
				continue
			}
			sorted := append([]float64(nil), vals...)
			sort.Float64s(sorted)
			fmt.Printf("%-8s %8d %9.3f%% %9.3f%% %9.3f%% %9.3f%%\n",
				venue, len(vals), sorted[0], sorted[len(sorted)/2],
				sorted[len(sorted)-1], latest)
		}
		span := hist[len(hist)-1].At.Sub(hist[0].At)
		fmt.Printf("\n  %d snapshots over %.1f hours\n", len(hist), span.Hours())
	} else {
		fmt.Println("\nUSDT BORROW HISTORY\n  not enough readings yet -- run with -record hourly")
	}

	// --- 3. WHAT IT MEANS FOR THE BOOK ---
	if !haveBest {
		return
	}
	positions, err := loadPositions(*dataDir)
	if err != nil || len(positions) == 0 {
		return
	}

	fmt.Printf("\nLEVERAGE ON THE OPEN BOOK   borrowing USDT at %.3f%% (%s)\n", best.AnnualPct, best.Venue)
	fmt.Print("return on capital, annualised, if the current funding rate holds\n\n")

	hdr := fmt.Sprintf("%-10s %-8s %10s %10s %10s", "SYMBOL", "VENUE", "FUND/YR", "COST/YR", "NET f")
	for _, l := range levs {
		hdr += fmt.Sprintf(" %8.0fx", l)
	}
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)))

	now := time.Now().UTC()
	for _, p := range positions {
		held := now.Sub(p.OpenedAt).Hours() / 24
		if held <= 0 || p.Notional <= 0 {
			continue
		}
		exit := p.ExitCost
		if exit == 0 {
			exit = p.EntryCost
		}
		// Funding actually collected, extrapolated to a year.
		fundYr := p.Funding / held * 365 / 100
		// Round trip amortised over the planned hold.
		hold := p.PlannedHold
		if hold <= 0 {
			hold = 30
		}
		costYr := (p.EntryCost + exit) * (365 / hold) / 100
		f := fundYr - costYr

		row := fmt.Sprintf("%-10s %-8s %9.2f%% %9.2f%% %9.2f%%",
			p.Symbol, p.Venue, fundYr, costYr, f)
		for _, L := range levs {
			// return = f·L − b·(L−1)
			ret := f*L - best.AnnualPct*(L-1)
			flag := " "
			if ret < f {
				// Leverage is making it worse: borrow exceeds the edge.
				flag = "!"
			}
			row += fmt.Sprintf(" %7.1f%%%s", ret, flag)
		}
		fmt.Println(row)
	}

	crossReport(*dataDir, levs)

	fmt.Printf(`
NET f is funding minus amortised trading costs, on NOTIONAL. Leverage multiplies
(f − borrow), not f. A row marked ! earns LESS at that leverage than unlevered,
because the borrow costs more than the edge is worth -- more leverage on it is
strictly worse, not merely riskier.

Break-even leverage exists where f = %.3f%%. Below that, do not borrow.
`, best.AnnualPct)
}

// --- cross-venue --------------------------------------------------------------

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

// crossReport shows what leverage does to perp-perp positions.
//
// THE ARITHMETIC IS DIFFERENT AND THE DIFFERENCE IS THE WHOLE ARGUMENT.
//
// Cash-and-carry must OWN the spot, so scaling it means borrowing, and every
// turn of leverage pays interest:
//
//	return = f·L − b·(L−1)
//
// Cross-venue has no spot leg. Both sides are perps posted on margin, so
// capital is 2·notional/L and leverage scales the return directly:
//
//	return = base · L
//
// No interest, no borrow-rate risk, no dependence on a lending market that
// tightens exactly when funding gets rich. That is why a 47% unlevered
// perp-perp position reaches ~143% at 3x while the best cash-and-carry symbol
// tops out near 40% at 10x.
//
// What it costs instead is liquidation risk: the replay put 33% of positions
// dead at 5x and 69% at 10x, because neither venue can see the other leg.
// intervalFixAt is when per-symbol funding intervals replaced the hardcoded
// 8-hour assumption. Positions opened before it were judged on spreads that
// were up to 75x too rich.
var intervalFixAt = time.Date(2026, 8, 12, 13, 30, 0, 0, time.UTC)

func crossReport(dir string, levs []float64) {
	raw, err := os.ReadFile(filepath.Join(dir, "cross_positions.json"))
	if err != nil {
		return
	}
	var f struct {
		Open   map[string]*crossPos `json:"open"`
		Closed []*crossPos          `json:"closed"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return
	}

	type row struct {
		label    string
		state    string
		heldD    float64
		netBps   float64
		annual   float64
		base     float64
		netUSD   float64
		capital  float64
		openedAt time.Time
	}
	var rows []row
	now := time.Now().UTC()

	add := func(p *crossPos, open bool) {
		if p == nil || p.Notional <= 0 {
			return
		}
		end := now
		state := "OPEN"
		if !open {
			end = p.ClosedAt
			state = "closed"
		}
		held := end.Sub(p.OpenedAt).Hours() / 24
		if held <= 0 {
			return
		}
		exit := p.ExitCost
		if exit == 0 {
			exit = p.EntryCost
		}
		net := p.LongLeg + p.ShortLeg - p.EntryCost - exit
		annual := net / held * 365 / 100
		rows = append(rows, row{
			label:    fmt.Sprintf("%s %s/%s", p.Coin, p.LongVenue, p.ShortVenue),
			state:    state,
			heldD:    held,
			netBps:   net,
			annual:   annual,
			base:     annual / 2,
			netUSD:   net / 10000 * p.Notional,
			capital:  p.Notional * 2,
			openedAt: p.OpenedAt,
		})
	}
	for _, p := range f.Open {
		add(p, true)
	}
	for _, p := range f.Closed {
		add(p, false)
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].base > rows[j].base })

	fmt.Printf("\n\nCROSS-VENUE PERP-PERP   NO BORROWING -- both legs are perps on margin\n")
	fmt.Print("return on capital, annualised from each position's own result\n\n")

	hdr := fmt.Sprintf("%-34s %-7s %7s %10s %10s", "PAIR", "STATE", "HELD", "NET BPS", "ON NOTL")
	for _, l := range levs {
		hdr += fmt.Sprintf(" %8.0fx", l)
	}
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)))

	// ANNUALISING A SHORT HOLD IS NOISE, NOT INFORMATION.
	//
	// ACE at 0.10 days had paid its entry cost and collected nothing, which
	// annualised to -1388%. The two stopped-out KAITOs, held 0.07 and 0.08
	// days, gave -4103% and -5010%. Those describe the arithmetic of dividing
	// by a small number, not the strategy.
	const minAnnualiseDays = 1.0
	const confidentDays = 7.0

	var totalNetUSD, totalCapital float64

	for _, r := range rows {
		totalNetUSD += r.netUSD
		totalCapital += r.capital

		if r.heldD < minAnnualiseDays {
			fmt.Printf("%-34s %-7s %6.2fd %+10.2f %10s   too short to annualise\n",
				r.label, r.state, r.heldD, r.netBps, "--")
			continue
		}
		mark := " "
		if r.heldD < confidentDays {
			mark = "~"
		}
		line := fmt.Sprintf("%-34s %-7s %6.2fd %+10.2f %9.1f%%%s",
			r.label, r.state, r.heldD, r.netBps, r.annual, mark)
		for _, L := range levs {
			line += fmt.Sprintf(" %8.1f%%", r.base*L)
		}
		fmt.Println(line)
	}

	// The honest aggregate, split at the interval fix.
	//
	// intervalFixAt is when per-symbol funding intervals went live. Before it,
	// venueIntervalHours hardcoded Binance and Bybit at 8 hours; KAITOUSDT
	// settles every 4, so one pair read +24.8 bps/hr against a true +0.33 and
	// two positions were opened on a number that was 75x wrong. Both were
	// stopped out.
	//
	// Those two are shown, not hidden -- they are real money the book lost.
	// But they are not evidence about the CURRENT system, because the entry
	// gate now refuses that shape outright (STOPS_OUT_BEFORE_PROFIT). Mixing
	// them into one average describes a bug that no longer exists.
	if totalCapital > 0 {
		fmt.Println(strings.Repeat("-", len(hdr)))
		fmt.Printf("BOOK TOTAL, not annualised:  net $%+.2f on $%.0f of capital  =  %+.3f%%\n",
			totalNetUSD, totalCapital, totalNetUSD/totalCapital*100)

		var preNet, preCap, postNet, postCap float64
		var preN, postN int
		for _, r := range rows {
			if r.openedAt.Before(intervalFixAt) {
				preNet += r.netUSD
				preCap += r.capital
				preN++
			} else {
				postNet += r.netUSD
				postCap += r.capital
				postN++
			}
		}
		if preCap > 0 {
			fmt.Printf("  before the interval fix (%d):  $%+.2f on $%.0f  =  %+.3f%%\n",
				preN, preNet, preCap, preNet/preCap*100)
		}
		if postCap > 0 {
			fmt.Printf("  after  the interval fix (%d):  $%+.2f on $%.0f  =  %+.3f%%\n",
				postN, postNet, postCap, postNet/postCap*100)
		}
	}

	fmt.Print(`
~ marks a hold under a week: the annual figure extrapolates hard from a short
window. Holds under a day are not annualised at all, because dividing a paid
entry cost by 0.07 days produces four-digit percentages that describe division,
not performance.

No borrow column because there is nothing to borrow. Capital is 2 x notional at
1x and 2 x notional / L above it, so leverage scales the return linearly --
unlike cash-and-carry, where interest eats each additional turn.

The cost is liquidation. Replayed against real 1m marks, cross-venue positions
died 0% of the time at 3x, 33% at 5x and 69% at 10x, because neither exchange
can see the other leg. 3x is the measured ceiling.
`)
}

// --- journal ------------------------------------------------------------------

func journal(dir string, s borrow.Snapshot) error {
	d := filepath.Join(dir, "borrow")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, "rates.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(s)
}

func readJournal(dir string) ([]borrow.Snapshot, error) {
	f, err := os.Open(filepath.Join(dir, "borrow", "rates.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []borrow.Snapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var s borrow.Snapshot
		if json.Unmarshal(sc.Bytes(), &s) == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// --- positions ----------------------------------------------------------------

type pos struct {
	Venue       string    `json:"venue"`
	Symbol      string    `json:"symbol"`
	OpenedAt    time.Time `json:"opened_at"`
	Notional    float64   `json:"notional_usd"`
	Funding     float64   `json:"funding_collected_bps"`
	EntryCost   float64   `json:"entry_cost_bps"`
	ExitCost    float64   `json:"exit_cost_bps"`
	PlannedHold float64   `json:"planned_hold_days"`
}

func loadPositions(dir string) ([]pos, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "positions.json"))
	if err != nil {
		return nil, err
	}
	var f struct {
		Open map[string]*pos `json:"open"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	var out []pos
	for _, p := range f.Open {
		if p != nil {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToUpper(p))
		}
	}
	return out
}

func parseFloats(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		var v float64
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%f", &v); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	sort.Float64s(out)
	return out
}
