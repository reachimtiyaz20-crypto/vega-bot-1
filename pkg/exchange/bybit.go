package exchange

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/economics"
)

const bybitBase = "https://api.bybit.com"

// BybitFeeProvenance records fees READ FROM THE ACCOUNT on 2026-08-05.
//
// This venue was deliberately shipped UNVERIFIED first: it scanned and
// journalled for hours while every opportunity came back REFUSED, because
// nobody had yet read the fee page. That is the pattern -- collect data
// immediately, permit decisions only after a human checks the number.
//
// The placeholders happened to be right. They were still placeholders until
// somebody looked.
var BybitFeeProvenance = FeeSource{
	URL:        "https://www.bybit.com/en/help-center/article/Trading-Fee-Structure",
	VerifiedOn: "2026-08-05",
	Note: "READ OFF THE LIVE LOGGED-IN FEE PANEL on 2026-08-05 by the account " +
		"holder: Non-VIP tier. Spot taker/maker 0.1000%/0.1000%. Futures " +
		"taker/maker 0.0550%/0.0200%. Taker on all four legs gives " +
		"2*(10+5.5) = 31 bps, one bp MORE than Binance's 30. " +
		"NOTE: MNT fee-discount toggles were OFF (spot 25%, futures 10% if " +
		"enabled and MNT held). Re-check if the account acquires MNT or " +
		"reaches a VIP tier -- Bybit runs INDEPENDENT VIP ladders for spot " +
		"and derivatives, so one can move without the other.",
}

// BybitVenue describes Bybit for the cost model.
func BybitVenue(fallbackSlippageBps float64) Venue {
	return Venue{
		Name: "bybit",
		Fees: economics.FeeSchedule{
			SpotTakerBps:      10.0,
			FuturesTakerBps:   5.5,
			SlippageBpsPerLeg: fallbackSlippageBps,
		},
		FeesVerified:                true,
		Source:                      BybitFeeProvenance,
		SpotAvailable:               true,
		DefaultFundingIntervalHours: 8,
		Notes: "USDT perps. 2026-08-06: 690 USDT perps, 410 USDT spot pairs, 293 hedgeable. " +
			"Two API calls per poll against Binance's six.",
	}
}

// bybitResp is the envelope every v5 endpoint wraps its payload in.
// retCode 0 means success; anything else is an error carried inside an
// HTTP 200, which is why the status code alone cannot be trusted here.
type bybitResp[T any] struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  T      `json:"result"`
}

// bybitLinearTicker is one USDT perpetual, verified live on 2026-08-06:
//
//	{"symbol":"BTCUSDT","indexPrice":"64783.38","markPrice":"64753.50",
//	 "turnover24h":"4051512258.8537","fundingRate":"-0.00000199",
//	 "nextFundingTime":"1785974400000","ask1Size":"19.538",
//	 "bid1Price":"64751.30","ask1Price":"64751.40","bid1Size":"1.963",
//	 "fundingIntervalHour":"8","fundingCap":"0.005"}
//
// Note nextFundingTime is a STRING here where Binance uses a number, and
// fundingIntervalHour is a STRING "8". The separate instruments-info endpoint
// reports the same interval as `fundingInterval: 480` in MINUTES -- reaching
// for that field instead would be wrong by a factor of sixty.
type bybitLinearTicker struct {
	Symbol              string `json:"symbol"`
	IndexPrice          string `json:"indexPrice"`
	MarkPrice           string `json:"markPrice"`
	Turnover24h         string `json:"turnover24h"`
	FundingRate         string `json:"fundingRate"`
	NextFundingTime     string `json:"nextFundingTime"`
	FundingIntervalHour string `json:"fundingIntervalHour"`
	FundingCap          string `json:"fundingCap"`
	Bid1Price           string `json:"bid1Price"`
	Bid1Size            string `json:"bid1Size"`
	Ask1Price           string `json:"ask1Price"`
	Ask1Size            string `json:"ask1Size"`
}

// bybitSpotTicker is one spot pair. No index price on this endpoint.
//
//	{"symbol":"BTCUSDT","bid1Price":"64782.6","bid1Size":"0.785908",
//	 "ask1Price":"64782.7","ask1Size":"0.578711",
//	 "turnover24h":"491941366.89169932","volume24h":"7640.126011"}
type bybitSpotTicker struct {
	Symbol      string `json:"symbol"`
	Bid1Price   string `json:"bid1Price"`
	Bid1Size    string `json:"bid1Size"`
	Ask1Price   string `json:"ask1Price"`
	Ask1Size    string `json:"ask1Size"`
	Turnover24h string `json:"turnover24h"`
}

type bybitList[T any] struct {
	Category string `json:"category"`
	List     []T    `json:"list"`
}

// BybitSource adapts Bybit's read-only public API to RateSource.
// No credentials, no signing, no order path.
type BybitSource struct {
	http  *http.Client
	venue Venue
}

// NewBybit builds the Bybit rate source.
func NewBybit(fallbackSlippageBps float64) *BybitSource {
	return &BybitSource{
		http:  newVenueHTTP(),
		venue: BybitVenue(fallbackSlippageBps),
	}
}

// Venue implements RateSource.
func (b *BybitSource) Venue() Venue { return b.venue }

// FundingRates implements RateSource.
//
// Two calls: linear tickers and spot tickers. Bybit returns funding, both
// prices, 24h turnover and top-of-book in a single response per category,
// so there is nothing to cache and nothing to stitch together.
func (b *BybitSource) FundingRates(ctx context.Context) ([]Observation, error) {
	var perps bybitResp[bybitList[bybitLinearTicker]]
	if err := httpJSON(ctx, b.http, bybitBase+"/v5/market/tickers?category=linear", &perps); err != nil {
		return nil, err
	}
	if perps.RetCode != 0 {
		return nil, fmt.Errorf("exchange: bybit linear tickers retCode=%d: %s", perps.RetCode, perps.RetMsg)
	}

	// Spot failure is not fatal: symbols simply come back without a spot leg,
	// which Assess refuses with a stated reason rather than costing at zero.
	spotBySymbol := map[string]bybitSpotTicker{}
	var spots bybitResp[bybitList[bybitSpotTicker]]
	if err := httpJSON(ctx, b.http, bybitBase+"/v5/market/tickers?category=spot", &spots); err == nil && spots.RetCode == 0 {
		for _, s := range spots.Result.List {
			spotBySymbol[s.Symbol] = s
		}
	}

	now := time.Now().UTC()
	out := make([]Observation, 0, len(perps.Result.List))

	for _, t := range perps.Result.List {
		// USDT-settled perpetuals only. Bybit's linear category also carries
		// USDC perps and dated futures, which do not pay funding on this
		// schedule and must not be assessed as if they do.
		if !strings.HasSuffix(t.Symbol, "USDT") || strings.Contains(t.Symbol, "-") {
			continue
		}

		rate, ok := parseF(t.FundingRate)
		if !ok {
			continue
		}
		ratePct := rate * 100

		// Bybit publishes a per-interval cap (0.5% on BTCUSDT). A rate outside
		// it is a feed or parsing error, not an opportunity -- an implausibly
		// large rate is exactly what a factor-of-100 bug looks like.
		if fcap, ok := parseF(t.FundingCap); ok && fcap > 0 {
			capPct := fcap * 100
			if ratePct > capPct || ratePct < -capPct {
				continue
			}
		}

		intervalHours, ok := parseF(t.FundingIntervalHour)
		if !ok || intervalHours <= 0 {
			intervalHours = b.venue.DefaultFundingIntervalHours
		}

		mark, _ := parseF(t.MarkPrice)
		index, _ := parseF(t.IndexPrice)
		perpVol, _ := parseF(t.Turnover24h)

		bid, hasBid := parseF(t.Bid1Price)
		ask, hasAsk := parseF(t.Ask1Price)
		bidQty, _ := parseF(t.Bid1Size)
		askQty, _ := parseF(t.Ask1Size)

		o := Observation{
			Venue:                 b.venue.Name,
			Symbol:                t.Symbol,
			FundingRatePct:        ratePct,
			IntervalHours:         intervalHours,
			MarkPrice:             mark,
			IndexPrice:            index,
			NextFundingTime:       parseMsString(t.NextFundingTime),
			ObservedAt:            now,
			PerpQuoteVolume24hUSD: perpVol,
		}
		if hasBid && hasAsk {
			o.PerpHalfSpreadBps = halfSpreadBps(bid, ask)
			o.PerpTopOfBookUSD = topOfBookUSD(bid, bidQty, ask, askQty)
		}

		s, hasSpot := spotBySymbol[t.Symbol]
		o.SpotSymbolAvailable = hasSpot
		if hasSpot {
			sBid, okB := parseF(s.Bid1Price)
			sAsk, okA := parseF(s.Ask1Price)
			sBidQty, _ := parseF(s.Bid1Size)
			sAskQty, _ := parseF(s.Ask1Size)
			o.SpotQuoteVolume24hUSD, _ = parseF(s.Turnover24h)
			if okB && okA {
				o.SpotHalfSpreadBps = halfSpreadBps(sBid, sAsk)
				o.SpotTopOfBookUSD = topOfBookUSD(sBid, sBidQty, sAsk, sAskQty)
			}
		}

		// Measured only when BOTH books are readable. Pricing two of four legs
		// is the same class of error as pricing none -- it just fails less
		// obviously.
		o.LiquidityMeasured = hasBid && hasAsk && hasSpot && o.SpotHalfSpreadBps >= 0 && o.SpotTopOfBookUSD > 0

		out = append(out, o)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("exchange: bybit returned %d linear tickers but no usable USDT perps", len(perps.Result.List))
	}
	return out, nil
}
