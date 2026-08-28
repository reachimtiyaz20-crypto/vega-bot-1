package exchange

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/binance"
	"github.com/imtiyaz/vega-bot/pkg/economics"
)

// BinanceFeeProvenance records where Binance's fee numbers came from.
//
// Read this before trusting a viability figure. These values are the ones in
// the PROJECT VEGA brief's post-mortem table (spot taker 10 bps, USDT-M
// futures taker 5 bps, VIP 0, no BNB discount). They were NOT read off
// Binance's live fee page during this build, because that page is
// JavaScript-rendered and could not be fetched from the VPS.
//
// VIP 0 with no discount is already the worst tier, so any error is in the
// safe direction. Confirm against the account's own fee tier before capital.
var BinanceFeeProvenance = FeeSource{
	URL:        "https://www.binance.com/en/fee/schedule",
	VerifiedOn: "2026-08-05",
	Note: "READ OFF THE LIVE LOGGED-IN FEE PAGE on 2026-08-05 by the account " +
		"holder: Regular User tier, 30-day spot volume $0, 30-day futures " +
		"volume $0. Spot maker/taker 0.100%/0.100%. USDT-M futures maker/taker " +
		"0.020%/0.050%. Taker on all four legs gives 2*(10+5) = 30 bps. " +
		"NOTE: the BNB Fee Discount toggle is ON for spot (would give 0.075% " +
		"taker = 7.5 bps) but the account holds ZERO BNB, so the discount does " +
		"not currently apply and 10 bps is what would actually be charged. " +
		"Buying a small BNB balance would cut the round trip from 30 to 25 bps " +
		"and shorten break-even by ~17%. Re-check if the account ever reaches " +
		"VIP 1 ($1m 30-day volume) or acquires BNB.",
}

// BinanceVenue is the venue definition used by BinanceSource.
//
// fallbackSlippageBps is used ONLY for symbols whose book could not be read.
// Every symbol with a readable book gets its own measured half-spreads, which
// on 2026-08-05 ranged from 0.008 bps (BTCUSDT) to 5.93 bps (BNCUSDT) on the
// perp leg alone. No single constant covers that range.
func BinanceVenue(fallbackSlippageBps float64) Venue {
	return Venue{
		Name: "binance",
		Fees: economics.FeeSchedule{
			SpotTakerBps:      10,
			FuturesTakerBps:   5,
			SlippageBpsPerLeg: fallbackSlippageBps,
		},
		FeesVerified:                true,
		Source:                      BinanceFeeProvenance,
		SpotAvailable:               true,
		DefaultFundingIntervalHours: 8,
		Notes:                       "USDT-M perps. 2026-08-05: 806 USDT perps, 479 USDT spot pairs, 371 hedgeable.",
	}
}

// referenceTTL is how long slow-moving reference data is cached. Funding
// intervals and spot listings change on the order of weeks; refetching them
// every poll is pure waste against the rate limit budget.
const referenceTTL = time.Hour

// BinanceSource adapts the read-only Binance client to the RateSource
// interface. It holds no credentials and has no path to placing an order.
type BinanceSource struct {
	client *binance.Client
	venue  Venue

	mu          sync.Mutex
	fundingInfo map[string]binance.FundingInfo
	spotSymbols map[string]bool
	refAt       time.Time
}

// NewBinance builds the Binance rate source.
func NewBinance(fallbackSlippageBps float64) *BinanceSource {
	return &BinanceSource{
		client: binance.New(),
		venue:  BinanceVenue(fallbackSlippageBps),
	}
}

// Venue implements RateSource.
func (b *BinanceSource) Venue() Venue { return b.venue }

// reference returns cached funding schedules and the spot symbol set.
//
// A refresh failure keeps the previous values rather than failing the scan.
// The degradation is safe in both directions: a stale spot set might exclude
// a newly-listed pair (missed opportunity, not a loss), and a stale funding
// interval defaults a 4h symbol to 8h, which UNDER-states its viability.
func (b *BinanceSource) reference(ctx context.Context) (map[string]binance.FundingInfo, map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.spotSymbols != nil && time.Since(b.refAt) < referenceTTL {
		return b.fundingInfo, b.spotSymbols
	}

	if fi, err := b.client.FundingInfo(ctx); err == nil {
		b.fundingInfo = fi
	}
	if ss, err := b.client.SpotUSDTSymbols(ctx); err == nil {
		b.spotSymbols = ss
		b.refAt = time.Now()
	}
	return b.fundingInfo, b.spotSymbols
}

// FundingRates implements RateSource.
//
// Six endpoints per poll against a 2400/min weight budget. Two of them
// (funding schedules, spot listings) are cached hourly. The two 24hr ticker
// calls are the heavy ones and are the reason the monitor polls on a slow
// cadence rather than continuously.
func (b *BinanceSource) FundingRates(ctx context.Context) ([]Observation, error) {
	prem, err := b.client.PremiumIndex(ctx)
	if err != nil {
		return nil, err
	}

	sched, spotSet := b.reference(ctx)
	if spotSet == nil {
		return nil, fmt.Errorf("exchange: binance spot symbol list unavailable; refusing to scan without it, because every symbol would look hedgeable")
	}

	// Book and volume failures are not fatal. Symbols simply come back with
	// LiquidityMeasured=false, and Assess refuses them by default rather than
	// costing them at zero.
	perpBooks, _ := b.client.FuturesBookTickers(ctx)
	spotBooks, _ := b.client.SpotBookTickers(ctx)
	perpVol, _ := b.client.Futures24hQuoteVolume(ctx)
	spotVol, _ := b.client.Spot24hQuoteVolume(ctx)

	now := time.Now().UTC()
	out := make([]Observation, 0, len(prem))

	for _, p := range prem {
		if !binance.IsUSDTPerp(p.Symbol) {
			continue
		}

		info, ok := sched[p.Symbol]
		if !ok || info.IntervalHours <= 0 {
			info = binance.DefaultFundingInfo(p.Symbol)
		}

		// A rate outside the venue's own published cap is a parsing or feed
		// error, not an opportunity. An implausibly large rate is exactly
		// what a factor-of-100 bug looks like, and the one thing worse than
		// missing an opportunity is inventing one.
		if info.CapPct > 0 && (p.LastFundingRatePct > info.CapPct || p.LastFundingRatePct < info.FloorPct) {
			continue
		}

		o := Observation{
			Venue:               b.venue.Name,
			Symbol:              p.Symbol,
			FundingRatePct:      p.LastFundingRatePct,
			IntervalHours:       info.IntervalHours,
			MarkPrice:           p.MarkPrice,
			IndexPrice:          p.IndexPrice,
			NextFundingTime:     p.NextFundingTime,
			ObservedAt:          now,
			SpotSymbolAvailable: spotSet[p.Symbol],

			PerpQuoteVolume24hUSD: perpVol[p.Symbol],
			SpotQuoteVolume24hUSD: spotVol[p.Symbol],
		}

		// Liquidity counts as measured only when BOTH books are readable.
		// A perp spread without a spot spread prices two of four legs, and a
		// half-priced round trip is the same class of error as an unpriced
		// one -- it just fails less obviously.
		perp, hasPerp := perpBooks[p.Symbol]
		spot, hasSpot := spotBooks[p.Symbol]
		if hasPerp {
			o.PerpHalfSpreadBps = perp.HalfSpreadBps()
			o.PerpTopOfBookUSD = perp.TopOfBookUSD()
		}
		if hasSpot {
			o.SpotHalfSpreadBps = spot.HalfSpreadBps()
			o.SpotTopOfBookUSD = spot.TopOfBookUSD()
		}
		o.LiquidityMeasured = hasPerp && hasSpot && o.SpotSymbolAvailable

		out = append(out, o)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("exchange: binance returned %d premium index entries but no usable USDT perps", len(prem))
	}
	return out, nil
}
