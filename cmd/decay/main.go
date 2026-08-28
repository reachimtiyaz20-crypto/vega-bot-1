// Command decay measures how fast a cross-venue dislocation dies.
//
// # WHY THIS EXISTS
//
// Every gate in the cross-venue book prices a candidate at its ENTRY spread and
// assumes that spread holds for the whole hold. Measured 2026-08-15, it does
// not:
//
//	ACE    9.139 -> 2.254 bps/hr in 13.7 hours   (~75% gone)
//	KAITO  6.666 -> 1.135 bps/hr in 77.9 hours
//
// ACE's plan checks missed low three times running, mean -50.23 bps. A gate
// built on a spread that halves every few hours will pass trades that cannot
// repay, and MaxBreakEvenHours is the wrong shape entirely.
//
// So: measure the decay from every pair ever logged, not from two positions.
//
// # THE CENSORING PROBLEM, STATED UP FRONT
//
// passes.jsonl only records pairs that CLEARED the stage-one spread floor.
// A pair whose spread falls below the floor stops being logged -- it does not
// appear as a small number, it disappears. So the decay measured here is a
// LOWER BOUND on the true decay: the fastest-dying pairs leave the sample
// first, and what remains is biased toward survivors.
//
// The real decay is worse than whatever this prints.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type rec struct {
	TsMs        int64   `json:"ts_ms"`
	Coin        string  `json:"coin"`
	LongVenue   string  `json:"long_venue"`
	ShortVenue  string  `json:"short_venue"`
	SpreadBpsHr float64 `json:"spread_bps_hr"`
	RoundTrip   float64 `json:"round_trip_bps"`
	Viable      bool    `json:"viable"`
	BelowFloor  bool    `json:"below_floor"`
}

// buckets are the elapsed-hour marks the survival curve is sampled at.
var buckets = []float64{0.5, 1, 2, 4, 8, 12, 24, 48}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	minObs := flag.Int("min-obs", 4, "minimum observations for a pair to count")
	viableOnly := flag.Bool("viable-only", false, "only pairs that passed every gate")
	flag.Parse()

	// BOTH files. passes.jsonl holds pairs while they qualify; decay.jsonl
	// holds the same pairs on the way down. Reading only the first is what
	// produced a non-monotonic survival curve.
	paths := []string{
		filepath.Join(*dataDir, "crossvenue", "passes.jsonl"),
		filepath.Join(*dataDir, "crossvenue", "decay.jsonl"),
	}

	// Group observations by pair.
	series := map[string][]rec{}
	total, below := 0, 0

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue // decay.jsonl will not exist until the book has run
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var r rec
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Coin == "" || r.SpreadBpsHr <= 0 {
				continue
			}
			if *viableOnly && !r.Viable && !r.BelowFloor {
				continue
			}
			total++
			if r.BelowFloor {
				below++
			}
			k := r.Coin + "|" + r.LongVenue + "|" + r.ShortVenue
			series[k] = append(series[k], r)
		}
		f.Close()
	}
	if below == 0 {
		fmt.Println("NOTE decay.jsonl is empty or absent, so this curve is still CENSORED --")
		fmt.Println("     pairs vanish when they drop below the floor instead of being followed down.")
		fmt.Println()
	} else {
		fmt.Printf("including %d observations of pairs BELOW the floor (uncensored)\n\n", below)
	}

	type sample struct {
		hours float64
		ratio float64
		entry float64 // the spread this pair was first seen at
	}
	var samples []sample
	kept := 0
	var lifespans []float64
	var peakSpread []float64

	for _, rs := range series {
		if len(rs) < *minObs {
			continue
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].TsMs < rs[j].TsMs })

		// The reference is the FIRST sighting. A pair is "entered" the moment
		// it appears, which is what the entry gate would actually have done.
		s0 := rs[0].SpreadBpsHr
		t0 := rs[0].TsMs
		if s0 <= 0 {
			continue
		}
		kept++
		peakSpread = append(peakSpread, s0)
		lifespans = append(lifespans, float64(rs[len(rs)-1].TsMs-t0)/3_600_000)

		for _, r := range rs[1:] {
			h := float64(r.TsMs-t0) / 3_600_000
			if h <= 0 {
				continue
			}
			samples = append(samples, sample{hours: h, ratio: r.SpreadBpsHr / s0, entry: s0})
		}
	}

	if kept == 0 {
		fmt.Println("no pair had enough observations; let the book run longer")
		return
	}

	fmt.Printf("SPREAD DECAY  from %d observations, %d pairs with >=%d sightings\n\n",
		total, kept, *minObs)

	// --- survival curve ---
	fmt.Println("SURVIVAL CURVE   what fraction of the entry spread remains")
	fmt.Printf("%-12s %10s %12s %12s %12s\n", "AFTER", "SAMPLES", "MEDIAN", "25th pct", "75th pct")
	fmt.Println(strings.Repeat("-", 62))

	medians := map[float64]float64{}
	for i, b := range buckets {
		lo := 0.0
		if i > 0 {
			lo = buckets[i-1]
		}
		var in []float64
		for _, s := range samples {
			if s.hours > lo && s.hours <= b {
				in = append(in, s.ratio)
			}
		}
		if len(in) < 3 {
			continue
		}
		sort.Float64s(in)
		med := pct(in, 0.5)
		medians[b] = med
		fmt.Printf("%-12s %10d %11.1f%% %11.1f%% %11.1f%%\n",
			fmt.Sprintf("%.1f h", b), len(in), med*100, pct(in, 0.25)*100, pct(in, 0.75)*100)
	}

	// --- half-life ---
	var hl []float64
	for _, s := range samples {
		if s.ratio <= 0 || s.ratio >= 1 || s.hours <= 0 {
			continue
		}
		h := s.hours * math.Log(0.5) / math.Log(s.ratio)
		if h > 0 && h < 2000 {
			hl = append(hl, h)
		}
	}
	if len(hl) >= 3 {
		sort.Float64s(hl)
		fmt.Printf("\nHALF-LIFE   median %.1f hours   (25th %.1f, 75th %.1f, n=%d)\n",
			pct(hl, 0.5), pct(hl, 0.25), pct(hl, 0.75), len(hl))
	}

	sort.Float64s(lifespans)
	sort.Float64s(peakSpread)
	fmt.Printf("PAIR LIFESPAN  median %.1f hours in the log   (a pair vanishes when it drops below the entry floor)\n",
		pct(lifespans, 0.5))
	fmt.Printf("ENTRY SPREAD   median %.3f bps/hr\n", pct(peakSpread, 0.5))

	// --- what it does to the gate ---
	//
	// The gate multiplies spread by hours. If the spread decays, the funding
	// actually collected is the INTEGRAL of a falling curve, not a rectangle.
	// --- decay by ENTRY SPREAD SIZE ---
	//
	// A pooled half-life is dominated by ordinary pairs: the median entry spread
	// in this log is under 1 bps/hr. Applying that number to a violent
	// dislocation is what let ONG through on 2026-08-21 -- entered at 138 bps/hr,
	// the gate expected 138 x 5.86h = 808 bps, and it collected 215 before the
	// spread crossed zero in under three hours.
	fmt.Println("\n\nDECAY BY ENTRY SPREAD SIZE")
	fmt.Println("the gate multiplies entry spread by ONE hold figure. If wide spreads die")
	fmt.Println("faster, that figure is most generous exactly where the stakes are highest.")
	fmt.Println()
	entryBands := []struct {
		lo, hi float64
		name   string
	}{{0, 2, "< 2"}, {2, 5, "2 - 5"}, {5, 20, "5 - 20"}, {20, 1e9, "> 20"}}
	fmt.Printf("%-10s %9s %11s %8s %8s %8s %13s\n",
		"ENTRY", "SAMPLES", "HALF-LIFE", "1h", "4h", "8h", "8h INTEGRAL")
	fmt.Println(strings.Repeat("-", 74))
	for _, eb := range entryBands {
		var sub []sample
		for _, sm := range samples {
			if sm.entry > eb.lo && sm.entry <= eb.hi {
				sub = append(sub, sm)
			}
		}
		if len(sub) < 10 {
			fmt.Printf("%-10s %9d   too few samples to measure\n", eb.name, len(sub))
			continue
		}
		var bhl []float64
		for _, sm := range sub {
			if sm.ratio <= 0 || sm.ratio >= 1 || sm.hours <= 0 {
				continue
			}
			if h := sm.hours * math.Log(0.5) / math.Log(sm.ratio); h > 0 && h < 2000 {
				bhl = append(bhl, h)
			}
		}
		bm := map[float64]float64{}
		for i, b := range buckets {
			lo := 0.0
			if i > 0 {
				lo = buckets[i-1]
			}
			var in []float64
			for _, sm := range sub {
				if sm.hours > lo && sm.hours <= b {
					in = append(in, sm.ratio)
				}
			}
			if len(in) >= 3 {
				sort.Float64s(in)
				bm[b] = pct(in, 0.5)
			}
		}
		hlmed := math.NaN()
		if len(bhl) >= 3 {
			sort.Float64s(bhl)
			hlmed = pct(bhl, 0.5)
		}
		fmt.Printf("%-10s %9d %10.1fh %7.0f%% %7.0f%% %7.0f%% %13.2f\n",
			eb.name, len(sub), hlmed, bm[1]*100, bm[4]*100, bm[8]*100, integrate(bm, 8))
	}

	fmt.Println("\n\nWHAT THIS DOES TO BREAK-EVEN")
	fmt.Print("the gate assumes spread x hours. reality is the area under a falling curve.\n\n")
	fmt.Printf("%-12s %14s %14s %16s\n", "HOLD", "GATE ASSUMES", "ACTUAL", "OVERSTATED BY")
	fmt.Println(strings.Repeat("-", 60))

	for _, h := range []float64{2, 4, 8, 12, 24} {
		naive := h // in units of the entry spread
		actual := integrate(medians, h)
		if actual <= 0 {
			continue
		}
		fmt.Printf("%-12s %13.2f %13.2f %15.2fx\n",
			fmt.Sprintf("%.0f h", h), naive, actual, naive/actual)
	}

	fmt.Print(`
GATE ASSUMES and ACTUAL are in units of "entry spread x hours". A 2x
overstatement means a pair the gate thinks repays in 4 hours really needs 8.

CENSORING: passes.jsonl only records pairs ABOVE the entry spread floor. A pair
that decays below it stops being logged rather than appearing as a small number,
so the fastest-dying pairs leave this sample first. The true decay is worse than
what is printed here.
`)
}

// integrate approximates the area under the survival curve out to h hours,
// in units of the entry spread.
func integrate(medians map[float64]float64, h float64) float64 {
	var area, prevT, prevV float64
	prevV = 1.0 // at t=0 the spread is, by definition, itself
	var ks []float64
	for k := range medians {
		ks = append(ks, k)
	}
	sort.Float64s(ks)

	for _, t := range ks {
		if t > h {
			break
		}
		v := medians[t]
		area += (prevV + v) / 2 * (t - prevT)
		prevT, prevV = t, v
	}
	if prevT < h && prevV > 0 {
		area += prevV * (h - prevT)
	}
	return area
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}
