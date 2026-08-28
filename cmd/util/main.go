// Command util measures how much of the cross-venue book's capacity was
// actually working, and why the rest was not.
//
// WHY THIS MATTERS MORE THAN THE RATE
//
//	annual return = (rate while deployed) x (utilisation) x (leverage)
//
// The rate is measured from real positions. The leverage is a choice with a
// replay behind it. Utilisation has been ESTIMATED BY EYE at "about 30%", and
// it multiplies everything -- so if it is really 12%, no amount of leverage
// reaches the target and the plan is wrong.
//
// # IDLE CAPITAL IS NOT ONE THING
//
// Capital sits unused for reasons that call for opposite fixes:
//
//	NO CANDIDATE      the market offered nothing. More filters cannot help;
//	                  only cheaper access or more venues would.
//	ALL REFUSED       candidates existed and the gate turned them down. If the
//	                  gate is miscalibrated this is recoverable, and it is the
//	                  case the cost-aware floor is meant to fix.
//	ALREADY HELD      the book found something but was already in that coin.
//	                  A concentration limit, not a market limit.
//	AT CAPACITY       genuinely full. The good problem.
//
// Reporting one "utilisation %" without that split hides which change is worth
// making.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type pass struct {
	TsMs         int64   `json:"ts_ms"`
	Coin         string  `json:"coin"`
	LongVenue    string  `json:"long_venue"`
	ShortVenue   string  `json:"short_venue"`
	SpreadBpsHr  float64 `json:"spread_bps_hr"`
	RoundTripBps float64 `json:"round_trip_bps"`
	BeHours      float64 `json:"be_hours"`
	Viable       bool    `json:"viable"`
	Gate         string  `json:"gate"`
}

type position struct {
	Coin       string    `json:"coin"`
	LongVenue  string    `json:"long_venue"`
	ShortVenue string    `json:"short_venue"`
	OpenedAt   time.Time `json:"opened_at"`
	ClosedAt   time.Time `json:"closed_at"`
	CapitalUSD float64   `json:"capital_usd"`
	Notional   float64   `json:"notional_usd"`
	LongLeg    float64   `json:"long_leg_bps"`
	ShortLeg   float64   `json:"short_leg_bps"`
	EntryCost  float64   `json:"entry_cost_bps"`
	ExitCost   float64   `json:"exit_cost_bps"`
}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	maxConcurrent := flag.Int("max-positions", 5, "book capacity, positions")
	notional := flag.Float64("notional", 400, "USD per leg")
	maxBE := flag.Float64("max-breakeven", 24, "break-even cap used by the gate, hours")
	flag.Parse()

	// --- positions ---
	raw, err := os.ReadFile(filepath.Join(*dataDir, "cross_positions.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read positions: %v\n", err)
		os.Exit(1)
	}
	var pf struct {
		Open   map[string]*position `json:"open"`
		Closed []*position          `json:"closed"`
	}
	if err := json.Unmarshal(raw, &pf); err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}

	var all []*position
	for _, p := range pf.Open {
		all = append(all, p)
	}
	all = append(all, pf.Closed...)
	if len(all) == 0 {
		fmt.Println("no positions yet")
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].OpenedAt.Before(all[j].OpenedAt) })

	now := time.Now().UTC()
	start := all[0].OpenedAt
	bookHours := now.Sub(start).Hours()
	capacityUSD := float64(*maxConcurrent) * *notional * 2

	var capHours, netUSD float64
	for _, p := range all {
		end := p.ClosedAt
		if end.IsZero() || end.Year() < 2 {
			end = now
		}
		h := end.Sub(p.OpenedAt).Hours()
		if h <= 0 {
			continue
		}
		capHours += p.CapitalUSD * h
		exit := p.ExitCost
		if exit == 0 {
			exit = p.EntryCost
		}
		netUSD += (p.LongLeg + p.ShortLeg - p.EntryCost - exit) / 10000 * p.Notional
	}
	capacityHours := capacityUSD * bookHours
	util := capHours / capacityHours * 100

	fmt.Printf("CROSS-VENUE UTILISATION\n\n")
	fmt.Printf("  book running        %.1f hours (%.2f days) since %s\n",
		bookHours, bookHours/24, start.Format("2006-01-02 15:04"))
	fmt.Printf("  capacity            %d positions x $%.0f/leg = $%.0f\n",
		*maxConcurrent, *notional, capacityUSD)
	fmt.Printf("  positions opened    %d\n", len(all))
	fmt.Printf("  capital-hours used  %.0f of %.0f possible\n", capHours, capacityHours)
	fmt.Printf("  UTILISATION         %.1f%%\n\n", util)

	// Return, decomposed the way the plan uses it.
	if capHours > 0 {
		deployedYears := capHours / (365 * 24)
		rateWhileDeployed := netUSD / capHours * (365 * 24) * 100
		overall := rateWhileDeployed * util / 100
		fmt.Printf("  net so far          $%+.2f\n", netUSD)
		fmt.Printf("  rate WHILE DEPLOYED %+.1f%%/yr   (on the capital that was actually working)\n",
			rateWhileDeployed)
		fmt.Printf("  x utilisation       %.1f%%\n", util)
		fmt.Printf("  = BOOK RETURN       %+.1f%%/yr at 1x\n", overall)
		fmt.Printf("    at 3x leverage    %+.1f%%/yr\n\n", overall*3)
		_ = deployedYears
	}

	// --- why the rest was idle ---
	f, err := os.Open(filepath.Join(*dataDir, "crossvenue", "passes.jsonl"))
	if err != nil {
		fmt.Println("no pass log; cannot attribute idle time")
		return
	}
	defer f.Close()

	byPass := map[int64][]pass{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var p pass
		if json.Unmarshal(sc.Bytes(), &p) == nil && p.Coin != "" {
			byPass[p.TsMs] = append(byPass[p.TsMs], p)
		}
	}

	var stamps []int64
	for ts := range byPass {
		stamps = append(stamps, ts)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i] < stamps[j] })

	var withViable, allRefused, onlyAlreadyHeld int
	gateWhenNoneViable := map[string]int{}
	// Counterfactual: how many passes would have offered something if the
	// floor were derived from each pair's OWN cost instead of a global 1.0.
	var wouldQualify int

	for _, ts := range stamps {
		rows := byPass[ts]
		viable := 0
		heldOnly := true
		anyCostAware := false
		for _, r := range rows {
			if r.Viable {
				viable++
			}
			if r.Gate != "" && r.Gate != "ALREADY_HELD" {
				heldOnly = false
			}
			// A pair is worth taking if it repays inside the cap, whatever its
			// spread -- which is what a cost-aware floor would ask.
			if r.RoundTripBps > 0 && r.SpreadBpsHr > 0 &&
				r.RoundTripBps/r.SpreadBpsHr <= *maxBE {
				anyCostAware = true
			}
		}
		if viable > 0 {
			withViable++
		} else {
			allRefused++
			if heldOnly {
				onlyAlreadyHeld++
			}
			for _, r := range rows {
				if r.Gate != "" {
					gateWhenNoneViable[r.Gate]++
				}
			}
		}
		if anyCostAware {
			wouldQualify++
		}
	}

	total := len(stamps)
	fmt.Printf("WHY CAPITAL WAS IDLE   %d passes logged\n\n", total)
	if total == 0 {
		return
	}
	pct := func(n int) float64 { return float64(n) / float64(total) * 100 }

	fmt.Printf("  passes offering something viable   %4d  (%.1f%%)\n", withViable, pct(withViable))
	fmt.Printf("  passes with nothing viable         %4d  (%.1f%%)\n", allRefused, pct(allRefused))
	fmt.Printf("    of those, refused ONLY for       %4d  (%.1f%%)  ALREADY_HELD -- a concentration\n",
		onlyAlreadyHeld, pct(onlyAlreadyHeld))
	fmt.Printf("                                                    limit, not a market limit\n\n")

	if len(gateWhenNoneViable) > 0 {
		type kv struct {
			k string
			n int
		}
		var gs []kv
		for k, n := range gateWhenNoneViable {
			gs = append(gs, kv{k, n})
		}
		sort.Slice(gs, func(i, j int) bool { return gs[i].n > gs[j].n })
		fmt.Println("  what blocked them, by count:")
		for _, g := range gs {
			fmt.Printf("    %-32s %d\n", g.k, g.n)
		}
		fmt.Println()
	}

	fmt.Printf("  passes where SOME pair repaid inside %.0fh   %4d  (%.1f%%)\n",
		*maxBE, wouldQualify, pct(wouldQualify))
	fmt.Printf("  passes that actually offered something       %4d  (%.1f%%)\n",
		withViable, pct(withViable))
	if withViable > 0 && wouldQualify > withViable {
		fmt.Printf("  -> a COST-AWARE floor would have offered %.1fx more\n",
			float64(wouldQualify)/float64(withViable))
	}

	fmt.Print(`
CAVEAT: passes.jsonl only records pairs that already cleared the GLOBAL spread
floor. Pairs below it never appear, so the cost-aware counterfactual above is a
FLOOR on the improvement, not the whole of it -- the cheap Lighter pairs that
sit between 0.5 and 1.0 bps/hr are invisible here entirely.
`)
	_ = strings.TrimSpace
}
