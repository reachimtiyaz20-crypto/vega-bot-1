package orderbook

import (
	"context"
	"fmt"
)

// BULK FUNDING FROM BINANCE AND BYBIT DIRECTLY
//
// Until now both venues' rates reached this project only through Hyperliquid's
// predictedFundings, which relays them for the 232 coins HYPERLIQUID lists.
// Binance lists 527 and Bybit 713. So roughly 60% of each venue was invisible,
// and every bitget/bybit pair the listing backtest found -- SSPC, FWDI, KORU --
// could never have been seen live, no matter how wide the spread got.
//
// One call each. The interval comes from the venue's own instrument metadata
// via FundingIntervalHours, never assumed, and a symbol without one is dropped.
type CEXFunding struct {
	Symbol        string
	Rate          float64 // per its own interval, as published
	IntervalHours float64
	NextFundingMs int64
}

// Fundings reads every Binance perp's upcoming rate in one request.
func (b *BinancePerp) Fundings(ctx context.Context) (map[string]CEXFunding, error) {
	var rows []struct {
		Symbol          string `json:"symbol"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
	}
	if err := getJSON(ctx, b.client(), b.base()+"/fapi/v1/premiumIndex", &rows); err != nil {
		return nil, fmt.Errorf("binance premiumIndex: %w", err)
	}
	out := make(map[string]CEXFunding, len(rows))
	for _, r := range rows {
		fi := b.FundingIntervalHours(r.Symbol)
		if !fi.Ok || fi.Hours <= 0 {
			continue
		}
		rate, ok := ParseNum(r.LastFundingRate)
		if !ok || r.NextFundingTime <= 0 {
			continue
		}
		out[r.Symbol] = CEXFunding{
			Symbol: r.Symbol, Rate: rate,
			IntervalHours: fi.Hours, NextFundingMs: r.NextFundingTime,
		}
	}
	return out, nil
}

// Fundings reads every Bybit linear perp's upcoming rate in one request.
func (b *BybitPerp) Fundings(ctx context.Context) (map[string]CEXFunding, error) {
	var env struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			List []struct {
				Symbol          string `json:"symbol"`
				FundingRate     string `json:"fundingRate"`
				NextFundingTime string `json:"nextFundingTime"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := getJSON(ctx, b.client(), b.base()+"/v5/market/tickers?category=linear", &env); err != nil {
		return nil, fmt.Errorf("bybit tickers: %w", err)
	}
	if env.RetCode != 0 {
		return nil, fmt.Errorf("bybit tickers: retCode %d: %s", env.RetCode, env.RetMsg)
	}
	out := make(map[string]CEXFunding, len(env.Result.List))
	for _, r := range env.Result.List {
		fi := b.FundingIntervalHours(r.Symbol)
		if !fi.Ok || fi.Hours <= 0 {
			continue
		}
		rate, ok1 := ParseNum(r.FundingRate)
		nf, ok2 := ParseNum(r.NextFundingTime)
		if !ok1 || !ok2 || nf <= 0 {
			continue
		}
		out[r.Symbol] = CEXFunding{
			Symbol: r.Symbol, Rate: rate,
			IntervalHours: fi.Hours, NextFundingMs: int64(nf),
		}
	}
	return out, nil
}
