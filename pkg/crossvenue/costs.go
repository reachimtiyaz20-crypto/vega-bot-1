package crossvenue

// MEASURED FILL COSTS, KEYED BY VENUE PAIR.
//
// The parsing and percentile work here came from an external coding agent
// (Jules) on 2026-08-20 and is kept largely as delivered. The gate that uses it
// was not completed and is implemented separately.
//
// WHY THERE IS NO FALLBACK
//
// A pair with too few measurements returns n=0 and the caller REFUSES it. The
// obvious convenience -- substitute a global figure when a pair is sparse -- is
// deliberately absent. A substituted number, even a pessimistic one, is still
// invented data standing in for data we could have collected, and this codebase
// has been damaged repeatedly by exactly that habit. The correct response to a
// missing measurement is to measure it.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// exitStressSlipMultiple scales the SLIPPAGE half of a round trip to reflect
// what exits actually cost, rather than what the book showed at entry.
//
// cmd/listslip sweeps both books at a single moment, so its round-trip
// slippage prices the exit against the same calm book as the entry. It assumes
// you leave into the conditions you arrived in. You do not: you leave when the
// position has gone wrong, and a book that has gone wrong is thin.
//
// Measured across 35 closed cross-venue positions, exit slippage against entry
// slippage on the same position:
//
//	median   1.35x
//	orderly  1.19x  (n=22)
//	forced   2.22x  (n=13, stop-outs)
//	worst    8.44x  (KAITO, 1.48 -> 12.53)
//
// A round trip is half entry slippage and half exit slippage, so scaling only
// the exit half by the 1.35 median scales the total by (1 + 1.35) / 2 = 1.175.
//
// Fees are deliberately excluded. A taker fee does not widen when a book thins.
//
// The distribution is bimodal, not smooth. If the stop-out rate rises above the
// 37% observed here, this single number understates the cost and should be
// re-derived rather than nudged.
const exitStressSlipMultiple = 1.175

// Costs holds round-trip cost samples per unordered venue pair.
type Costs struct {
	samples  map[string][]float64 // as measured, both halves at the calm book
	stressed map[string][]float64 // exit half scaled to realised conditions
}

// costPairKey is order-independent: a round trip costs the same whichever leg
// is called long.
func costPairKey(a, b string) string {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// LoadCosts reads the JSONL emitted by cmd/listslip.
//
// cost_total_bps is ALREADY the complete round trip: both legs, swept in and
// out, plus four taker fees. One number, not an entry and an exit to be summed.
// notionalUSD is the size the book actually trades. Only samples measured at
// that size are kept, because the cost of a fill is a function of how much of
// the book it eats -- measured 2026-08-24, one pair cost 21.5 bps at $50 and
// 94.2 bps at $1000, and averaging those describes no trade anyone can make.
//
// Pass 0 to keep every sample regardless of size. That is for tests and
// diagnostics; a running book must state its size.
func LoadCosts(path string, notionalUSD float64) (*Costs, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("loading costs: %w", err)
	}
	defer f.Close()

	c := &Costs{samples: make(map[string][]float64), stressed: make(map[string][]float64)}
	d := json.NewDecoder(f)
	for d.More() {
		var row struct {
			VenueA       string  `json:"venue_a"`
			VenueB       string  `json:"venue_b"`
			CostTotalBps float64 `json:"cost_total_bps"`
			SlipTotalBps float64 `json:"slip_total_bps"`
			FeesBps      float64 `json:"fees_bps"`
			NotionalUSD  float64 `json:"notional_usd"`
		}
		if err := d.Decode(&row); err != nil {
			return nil, fmt.Errorf("parsing costs: %w", err)
		}
		if row.VenueA == "" || row.VenueB == "" || row.CostTotalBps <= 0 {
			continue // an unusable row is skipped, never defaulted
		}
		// Wrong size is wrong data. A 1% band absorbs float formatting without
		// admitting a neighbouring rung.
		if notionalUSD > 0 {
			if row.NotionalUSD <= 0 {
				continue // pre-2026-08-24 rows carry no size; they cannot be placed
			}
			if math.Abs(row.NotionalUSD-notionalUSD) > 0.01*notionalUSD {
				continue
			}
		}
		k := costPairKey(row.VenueA, row.VenueB)
		c.samples[k] = append(c.samples[k], row.CostTotalBps)

		// The stressed figure needs slippage and fees separately, because only
		// slippage widens. A row without that split cannot be stressed, so it is
		// skipped rather than approximated -- same rule as everything else here.
		if row.SlipTotalBps > 0 {
			c.stressed[k] = append(c.stressed[k],
				row.FeesBps+row.SlipTotalBps*exitStressSlipMultiple)
		}
	}
	for _, v := range c.samples {
		sort.Float64s(v)
	}
	for _, v := range c.stressed {
		sort.Float64s(v)
	}
	return c, nil
}

// P95CostBps returns the 95th-percentile round trip for a venue pair and the
// number of samples behind it.
//
// The count is returned rather than hidden because refusing a thin sample is
// the caller's job. A p95 from 30 observations is the second-highest of them
// pretending to be a percentile.
func (c *Costs) P95CostBps(venueA, venueB string) (float64, int) {
	return p95of(c.samples, c, venueA, venueB)
}

// P95StressedCostBps is P95CostBps with the exit half of the slippage scaled to
// what exits have actually cost. This is the figure an entry gate should use:
// the measured one prices a departure into conditions that will not be there.
func (c *Costs) P95StressedCostBps(venueA, venueB string) (float64, int) {
	return p95of(c.stressed, c, venueA, venueB)
}

func p95of(m map[string][]float64, c *Costs, venueA, venueB string) (float64, int) {
	if c == nil {
		return 0, 0
	}
	s := m[costPairKey(venueA, venueB)]
	n := len(s)
	if n == 0 {
		return 0, 0
	}
	idx := int(float64(n) * 0.95)
	if idx >= n {
		idx = n - 1
	}
	return s[idx], n
}

// Pairs reports sample counts per venue pair, for the pass summary. If most
// pairs sit below the minimum, that is a finding about the sampler's coverage
// rather than about the market.
func (c *Costs) Pairs() map[string]int {
	if c == nil {
		return nil
	}
	out := make(map[string]int, len(c.samples))
	for k, v := range c.samples {
		out[k] = len(v)
	}
	return out
}
