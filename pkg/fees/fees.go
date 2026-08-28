// Package fees is the single source of truth for taker fees.
//
// # WHY IT EXISTS
//
// Fees lived in per-service command-line flags until 2026-08-20. When OKX was
// corrected from 25 bps to 5, two of the three services were updated and one
// was not -- so vega-cross priced an identical OKX round trip 40 bps higher
// than vega-union, and no number from either was comparable with the other.
//
// The values being wrong was the smaller problem. Fees scattered across seven
// unit files is why they could disagree at all.
package fees

import (
	"encoding/json"
	"fmt"
	"os"
)

type Registry struct {
	Updated    string             `json:"updated"`
	Warning    string             `json:"warning"`
	TakerBps   map[string]float64 `json:"taker_bps"`
	Provenance map[string]string  `json:"provenance"`
	PerSymbol  map[string]string  `json:"per_symbol_venues"`
}

func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fees: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("fees: decoding %s: %w", path, err)
	}
	if len(r.TakerBps) == 0 {
		return nil, fmt.Errorf("fees: %s carries no taker fees", path)
	}
	for v, bps := range r.TakerBps {
		if bps < 0 || bps > 100 {
			return nil, fmt.Errorf("fees: %s taker %.2f bps is outside any plausible range", v, bps)
		}
	}
	return &r, nil
}

// Taker returns a venue's fee. ok is false for a venue priced per symbol or
// absent entirely -- both of which must be refused rather than defaulted.
func (r *Registry) Taker(venue string) (float64, bool) {
	v, ok := r.TakerBps[venue]
	return v, ok
}

// Assert checks a supplied value against the registry.
//
// Flags are kept as a REDUNDANT ASSERTION rather than a source. A service file
// that disagrees with the registry is a configuration error, and it should stop
// the process at startup rather than quietly price a venue differently from
// every other process on the box.
func (r *Registry) Assert(venue string, supplied float64) error {
	if supplied == 0 {
		return nil // not asserted
	}
	want, ok := r.TakerBps[venue]
	if !ok {
		return fmt.Errorf("fees: %s is not in the registry but a fee of %.2f was supplied", venue, supplied)
	}
	if diff := supplied - want; diff > 0.001 || diff < -0.001 {
		return fmt.Errorf("fees: %s supplied as %.2f bps but the registry says %.2f -- "+
			"one of them is wrong and guessing which is how OKX stayed at 25 bps for two weeks",
			venue, supplied, want)
	}
	return nil
}
