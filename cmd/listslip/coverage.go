package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// COVERAGE-DRIVEN SAMPLING
//
// The sampler used to rank candidates by funding spread and take the top N.
// Measured over seven days that produced 1,298 samples for bitget|bybit and
// ZERO for all eleven venue pairs involving hyperliquid or lighter -- including
// lighter, which charges no fees at all. The book refuses a pair it has no cost
// data for, so the cheapest venues on the board were unreachable, and the
// reason never appeared as a failure: it appeared as 927 daily COST_UNMEASURED
// refusals that looked like a strict gate rather than a starved one.
//
// Ranking by spread also biased the cost distribution itself. High funding
// concentrates in illiquid alts with wide books, so sampling the highest
// spreads measured the expensive tail and reported it as the cost of trading.

// pairKey names a venue pair independent of leg order, matching how
// crossvenue.Costs keys its samples.
func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// loadCoverage counts DISTINCT BOOK READS per venue pair, not records.
//
// Those stopped being the same number the moment a read began emitting one
// record per size. Counting records would have reported four samples for what
// is a single observation of a single book at a single instant -- four numbers
// that move together and carry no more information than one. A pair would then
// clear a 40-sample floor on ten real reads, and the p95 behind every entry
// decision would rest on a quarter of the evidence it claimed.
//
// A read is identified by timestamp and coin, which is exactly what one pass
// over one pair produces.
//
// A malformed line is skipped rather than fatal: this file is append-only and
// a torn final line after a crash must not stop the next run from measuring.
// Undercounting costs an extra sample; refusing to start costs a day.
func loadCoverage(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listslip: reading coverage from %s: %w", path, err)
	}
	defer f.Close()

	seen := map[string]map[string]bool{} // pair -> set of "ts|coin"
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m struct {
			TS     string `json:"ts"`
			Coin   string `json:"coin"`
			VenueA string `json:"venue_a"`
			VenueB string `json:"venue_b"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.VenueA == "" || m.VenueB == "" {
			continue
		}
		k := pairKey(m.VenueA, m.VenueB)
		if seen[k] == nil {
			seen[k] = map[string]bool{}
		}
		seen[k][m.TS+"|"+m.Coin] = true
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("listslip: scanning %s: %w", path, err)
	}

	cov := make(map[string]int, len(seen))
	for k, reads := range seen {
		cov[k] = len(reads)
	}
	return cov, nil
}

// parseSizes turns "50,100,400,1000" into notionals, sorted ascending.
//
// Sorted so the console output reads as a curve rather than as whatever order
// somebody typed the flag in.
func parseSizes(s string) ([]float64, error) {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil, fmt.Errorf("listslip: bad size %q: %w", part, err)
		}
		if v <= 0 {
			return nil, fmt.Errorf("listslip: size must be positive, got %v", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("listslip: no sizes given")
	}
	sort.Float64s(out)
	return out, nil
}

// reportCoverage prints what is short before any measuring happens, so a run
// that fixes nothing is visible as such.
func reportCoverage(cov map[string]int, target int) {
	keys := make([]string, 0, len(cov))
	for k := range cov {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return cov[keys[i]] < cov[keys[j]] })

	var short int
	for _, k := range keys {
		if cov[k] < target {
			short++
		}
	}
	fmt.Printf("coverage: %d venue pairs seen, %d below the %d-sample floor\n",
		len(cov), short, target)
	for _, k := range keys {
		if cov[k] < target {
			fmt.Printf("  SHORT  %-24s %d/%d\n", k, cov[k], target)
		}
	}
	if len(cov) > 0 && short == 0 {
		fmt.Println("  every pair seen so far is covered; pairs never seen do not appear here")
	}
	fmt.Println()
}
