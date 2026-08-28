// Command newlist tests whether newly listed perps pay more funding, and
// whether VEGA has been refusing them.
//
// # THE HYPOTHESIS
//
// A trader at a Binance event described running funding arbitrage on newly
// listed alts across venues, at ~37% a year for four years. The mechanism is
// plausible: a new perp lists, retail piles in long, there is no spot market
// yet so nobody can arbitrage it the usual way, and funding sits pinned near
// the venue cap for days before normalising.
//
// If that is right, the opportunity is a LISTING CALENDAR, not a rate screen.
//
// AND VEGA MAY BE EXCLUDING IT BY CONSTRUCTION
//
//	MinVolUSD  $10,000,000   -- a new listing starts far below this
//	depth floors, notional minimums
//
// The cash-and-carry scan refuses ~631 symbols per poll for thin perp volume.
// If new listings are what pays, they are sitting in that pile, refused for
// being NEW rather than for being bad.
//
// # WHAT THIS MEASURES
//
// A symbol whose first appearance in the journal is well after the journal
// starts is a new listing. For those, versus everything else:
//
//  1. annualised funding, and how it decays after listing
//  2. which gate refused them
//
// READ-ONLY. Opens the journal, prints, exits. Nothing running is affected.
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type obs struct {
	Type       string  `json:"type"`
	TsMs       int64   `json:"ts_ms"`
	Venue      string  `json:"venue"`
	Symbol     string  `json:"symbol"`
	Annualized float64 `json:"annualized_pct"`
	FundingPct float64 `json:"funding_rate_pct"`
	IntervalH  float64 `json:"interval_hours"`
	PerpVol    float64 `json:"perp_vol_24h_usd"`
	SpotVol    float64 `json:"spot_vol_24h_usd"`
	SpotAvail  bool    `json:"spot_available"`
	Gate30d    string  `json:"gate_30d"`
	NetBps30d  float64 `json:"net_bps_30d"`
}

type sym struct {
	FirstMs, LastMs int64
	Count           int

	// funding by age since first sighting
	sum12, sum48, sumRest float64
	n12, n48, nRest       int
	maxPerpVol            float64
	gates                 map[string]int
	spotAvail             bool
}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	newAfterH := flag.Float64("new-after", 24,
		"a symbol first seen this many hours into the journal counts as newly listed")
	minObs := flag.Int("min-obs", 5, "minimum sightings for a symbol to count")
	top := flag.Int("top", 25, "rows to print")
	flag.Parse()

	files, _ := filepath.Glob(filepath.Join(*dataDir, "journal", "*.jsonl*"))
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no journal files")
		os.Exit(1)
	}
	sort.Strings(files)

	// --- pass 1: first and last sighting ---
	syms := map[string]*sym{}
	var startMs, endMs int64

	scanAll(files, func(o obs) {
		if o.Type != "obs" || o.Symbol == "" {
			return
		}
		if startMs == 0 || o.TsMs < startMs {
			startMs = o.TsMs
		}
		if o.TsMs > endMs {
			endMs = o.TsMs
		}
		k := o.Venue + "|" + o.Symbol
		s := syms[k]
		if s == nil {
			s = &sym{FirstMs: o.TsMs, LastMs: o.TsMs, gates: map[string]int{}}
			syms[k] = s
		}
		if o.TsMs < s.FirstMs {
			s.FirstMs = o.TsMs
		}
		if o.TsMs > s.LastMs {
			s.LastMs = o.TsMs
		}
		s.Count++
	})

	if startMs == 0 {
		fmt.Fprintln(os.Stderr, "no observations")
		os.Exit(1)
	}
	spanH := float64(endMs-startMs) / 3_600_000
	cutoff := startMs + int64(*newAfterH*3_600_000)

	// --- pass 2: funding by age, gates ---
	scanAll(files, func(o obs) {
		if o.Type != "obs" || o.Symbol == "" {
			return
		}
		s := syms[o.Venue+"|"+o.Symbol]
		if s == nil {
			return
		}
		ageH := float64(o.TsMs-s.FirstMs) / 3_600_000
		switch {
		case ageH <= 12:
			s.sum12 += o.Annualized
			s.n12++
		case ageH <= 48:
			s.sum48 += o.Annualized
			s.n48++
		default:
			s.sumRest += o.Annualized
			s.nRest++
		}
		if o.PerpVol > s.maxPerpVol {
			s.maxPerpVol = o.PerpVol
		}
		if o.Gate30d != "" {
			s.gates[o.Gate30d]++
		}
		if o.SpotAvail {
			s.spotAvail = true
		}
	})

	type row struct {
		key            string
		venue, symbol  string
		firstMs        int64
		f12, f48, fAll float64
		vol            float64
		gate           string
		spot           bool
		obs            int
	}
	var news, established []row

	for k, s := range syms {
		if s.Count < *minObs {
			continue
		}
		parts := strings.SplitN(k, "|", 2)
		avg := func(sum float64, n int) float64 {
			if n == 0 {
				return 0
			}
			return sum / float64(n)
		}
		allSum := s.sum12 + s.sum48 + s.sumRest
		allN := s.n12 + s.n48 + s.nRest

		domGate := ""
		best := 0
		for g, n := range s.gates {
			if n > best {
				domGate, best = g, n
			}
		}
		r := row{
			key: k, venue: parts[0], symbol: parts[1], firstMs: s.FirstMs,
			f12: avg(s.sum12, s.n12), f48: avg(s.sum48, s.n48),
			fAll: avg(allSum, allN), vol: s.maxPerpVol,
			gate: domGate, spot: s.spotAvail, obs: s.Count,
		}
		if s.FirstMs > cutoff {
			news = append(news, r)
		} else {
			established = append(established, r)
		}
	}

	fmt.Printf("NEW LISTING FUNDING TEST\n\n")
	fmt.Printf("  journal spans %.1f hours (%.1f days), %s -> %s\n",
		spanH, spanH/24,
		time.UnixMilli(startMs).UTC().Format("2006-01-02 15:04"),
		time.UnixMilli(endMs).UTC().Format("2006-01-02 15:04"))
	fmt.Printf("  symbols seen        %d\n", len(syms))
	fmt.Printf("  established         %d  (present in the first %.0fh)\n", len(established), *newAfterH)
	fmt.Printf("  NEWLY LISTED        %d  (first seen after that)\n\n", len(news))

	if len(news) == 0 {
		fmt.Printf(`No new listings in this window.

That is not evidence against the strategy -- %.1f days is a short window and
listings are episodic. To test it properly the book has to WATCH FORWARD: record
the symbol set each day and flag additions. Worth doing if the idea is to be
pursued.
`, spanH/24)
		return
	}

	sort.Slice(news, func(i, j int) bool { return news[i].f12 > news[j].f12 })

	fmt.Printf("%-10s %-9s %-16s %10s %10s %10s %14s %-14s\n",
		"SYMBOL", "VENUE", "FIRST SEEN", "0-12h %/yr", "12-48h", "48h+", "PERP VOL $", "GATE")
	fmt.Println(strings.Repeat("-", 100))
	for i, r := range news {
		if i >= *top {
			fmt.Printf("  ... %d more\n", len(news)-*top)
			break
		}
		fmt.Printf("%-10s %-9s %-16s %10.2f %10.2f %10.2f %14.0f %-14s\n",
			r.symbol, r.venue, time.UnixMilli(r.firstMs).UTC().Format("01-02 15:04"),
			r.f12, r.f48, r.fAll, r.vol, r.gate)
	}

	// --- the comparison ---
	med := func(rs []row, pick func(row) float64) float64 {
		var v []float64
		for _, r := range rs {
			v = append(v, pick(r))
		}
		if len(v) == 0 {
			return 0
		}
		sort.Float64s(v)
		return v[len(v)/2]
	}

	fmt.Printf("\nMEDIAN ANNUALISED FUNDING, on notional\n\n")
	fmt.Printf("  newly listed, first 12h   %8.2f %%/yr\n", med(news, func(r row) float64 { return r.f12 }))
	fmt.Printf("  newly listed, 12-48h      %8.2f %%/yr\n", med(news, func(r row) float64 { return r.f48 }))
	fmt.Printf("  newly listed, overall     %8.2f %%/yr\n", med(news, func(r row) float64 { return r.fAll }))
	fmt.Printf("  established               %8.2f %%/yr\n", med(established, func(r row) float64 { return r.fAll }))

	nf := med(news, func(r row) float64 { return r.f12 })
	ef := med(established, func(r row) float64 { return r.fAll })
	if ef != 0 {
		fmt.Printf("\n  ratio, new(0-12h) vs established:  %.2fx\n", nf/ef)
	}

	// --- were we refusing them ---
	gateDist := func(rs []row) map[string]int {
		m := map[string]int{}
		for _, r := range rs {
			g := r.gate
			if g == "" {
				g = "(none)"
			}
			m[g]++
		}
		return m
	}
	fmt.Printf("\nWHAT GATE THEY HIT\n\n%-24s %10s %14s\n", "GATE", "NEW", "ESTABLISHED")
	fmt.Println(strings.Repeat("-", 52))
	ng, eg := gateDist(news), gateDist(established)
	keys := map[string]bool{}
	for k := range ng {
		keys[k] = true
	}
	for k := range eg {
		keys[k] = true
	}
	var ks []string
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ng[ks[i]] > ng[ks[j]] })
	for _, k := range ks {
		fmt.Printf("%-24s %9d%% %13d%%\n", k,
			pct(ng[k], len(news)), pct(eg[k], len(established)))
	}

	fmt.Print(`
READ IT THIS WAY. If newly listed symbols show materially higher funding in
their first 12 hours AND their dominant gate is a volume or depth refusal, then
VEGA is declining them for being NEW rather than for being bad -- and that is a
specific, fixable exclusion rather than a market that does not pay.

If their funding looks like everything else, the listing angle is not where the
edge is, whatever anyone at a conference said.
`)
}

func pct(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}

func scanAll(files []string, fn func(obs)) {
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var r io.Reader = f
		if strings.HasSuffix(path, ".gz") {
			gz, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				continue
			}
			r = gz
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 128*1024), 4<<20)
		for sc.Scan() {
			var o obs
			if json.Unmarshal(sc.Bytes(), &o) == nil {
				fn(o)
			}
		}
		f.Close()
	}
}
