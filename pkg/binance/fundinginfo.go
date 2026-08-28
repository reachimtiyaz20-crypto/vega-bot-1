package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// rawFundingInfo mirrors /fapi/v1/fundingInfo, verified live from the
// Frankfurt VPS on 2026-08-05:
//
//	{"symbol":"GTCUSDT","adjustedFundingRateCap":"0.02000000",
//	 "adjustedFundingRateFloor":"-0.02000000","fundingIntervalHours":8,
//	 "disclaimer":false,"updateTime":1758377721362}
//
// Note fundingIntervalHours is a NUMBER here while every rate is a string.
// Note also that the endpoint does NOT list only non-standard symbols --
// GTCUSDT is listed with the standard 8h interval. It lists symbols with any
// adjusted parameter, so absence from this list means "standard 8h, standard
// cap", not "unknown".
type rawFundingInfo struct {
	Symbol                   string  `json:"symbol"`
	AdjustedFundingRateCap   string  `json:"adjustedFundingRateCap"`
	AdjustedFundingRateFloor string  `json:"adjustedFundingRateFloor"`
	FundingIntervalHours     float64 `json:"fundingIntervalHours"`
	Disclaimer               bool    `json:"disclaimer"`
	UpdateTime               int64   `json:"updateTime"`
}

// FundingInfo is a symbol's funding schedule and rate bounds.
type FundingInfo struct {
	Symbol string

	// IntervalHours is 8 by default; LPTUSDT and TRBUSDT settle every 4.
	// A 4-hour symbol pays 6 times a day, which HALVES break-even time --
	// treating it as 8h silently under-reports its viability.
	IntervalHours float64

	// CapPct and FloorPct bound the per-interval rate, as percentages.
	// Binance's default adjusted cap is 2% per interval. A parsed rate
	// outside these bounds is a data error, not an opportunity.
	CapPct   float64
	FloorPct float64
}

// IntervalsPerDay converts the funding interval into settlements per day,
// which is what economics.Opportunity.IntervalsPerDayOverride wants.
func (f FundingInfo) IntervalsPerDay() float64 {
	if f.IntervalHours <= 0 {
		return 3
	}
	return 24 / f.IntervalHours
}

// FundingInfo fetches per-symbol funding schedules and rate caps.
//
// Returns a map keyed by symbol. Symbols absent from the map use Binance
// defaults: 8-hour intervals, 2% cap. Callers must treat a missing key as
// "default", never as "skip".
func (c *Client) FundingInfo(ctx context.Context) (map[string]FundingInfo, error) {
	body, err := c.get(ctx, "/fapi/v1/fundingInfo")
	if err != nil {
		return nil, err
	}

	var raw []rawFundingInfo
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("binance: decoding fundingInfo: %w (first 200 bytes: %.200s)", err, body)
	}

	out := make(map[string]FundingInfo, len(raw))
	for _, r := range raw {
		fi := FundingInfo{
			Symbol:        r.Symbol,
			IntervalHours: r.FundingIntervalHours,
		}
		if v, err := strconv.ParseFloat(r.AdjustedFundingRateCap, 64); err == nil {
			fi.CapPct = v * 100
		}
		if v, err := strconv.ParseFloat(r.AdjustedFundingRateFloor, 64); err == nil {
			fi.FloorPct = v * 100
		}
		out[r.Symbol] = fi
	}
	return out, nil
}

// DefaultFundingInfo is what to assume for a symbol absent from FundingInfo.
func DefaultFundingInfo(symbol string) FundingInfo {
	return FundingInfo{
		Symbol:        symbol,
		IntervalHours: 8,
		CapPct:        2.0,
		FloorPct:      -2.0,
	}
}
