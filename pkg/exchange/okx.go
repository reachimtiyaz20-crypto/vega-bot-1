package exchange

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/economics"
)

const okxBase = "https://www.okx.com"

// OKXFeeProvenance is DELIBERATELY UNVERIFIED. See BybitFeeProvenance for why
// this pattern exists: the venue scans and journals from day one, but cannot
// show a green light until a human has read the fee page.
//
// TO VERIFY: read OKX's spot taker and USDT perpetual taker for your own tier,
// put them in OKXVenue, set VerifiedOn, and set FeesVerified: true.
var OKXFeeProvenance = FeeSource{
	URL:        "https://www.okx.com/fees",
	VerifiedOn: "",
	Note: "UNVERIFIED PLACEHOLDER. Spot taker 10 bps and perp taker 5 bps are " +
		"commonly-cited Lv1 figures, NOT read off the fee page by anyone. OKX " +
		"tiers depend on OKB holdings as well as volume, so the published table " +
		"may not be the rate this account pays. Verify before trusting any " +
		"viability figure from this venue.",
}

// OKXVenue describes OKX for the cost model.
func OKXVenue(fallbackSlippageBps float64) Venue {
	return Venue{
		Name: "okx",
		Fees: economics.FeeSchedule{
			SpotTakerBps:      10.0,
			FuturesTakerBps:   5.0,
			SlippageBpsPerLeg: fallbackSlippageBps,
		},
		FeesVerified:                false,
		Source:                      OKXFeeProvenance,
		SpotAvailable:               true,
		DefaultFundingIntervalHours: 8,
		Notes: "USDT swaps. 2026-08-06: 421 USDT perps, 352 USDT spot pairs, 214 hedgeable. " +
			"Symbols normalised from BTC-USDT-SWAP to BTCUSDT so the journal can be " +
			"joined across venues.",
	}
}

// okxResp is the envelope on every v5 endpoint. code is a STRING "0" on
// success, and errors arrive inside HTTP 200 -- the status code alone is not
// enough to know the call worked.
type okxResp[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []T    `json:"data"`
}

// okxFunding is one entry from /public/funding-rate?instId=ANY, verified live
// on 2026-08-06:
//
//	{"fundingRate":"-0.0000798946257635","fundingTime":"1785974400000",
//	 "instId":"LAYER-USDT-SWAP","maxFundingRate":"0.01","minFundingRate":"-0.01",
//	 "nextFundingTime":"1785988800000","prevFundingTime":"1785960000000",
//	 "settState":"settled","ts":"1785965303386"}
//
// THE NAMING IS A TRAP. fundingTime is the NEXT settlement -- for BTCUSDT it
// sat 2.5 hours in the future while prevFundingTime was in the past.
// nextFundingTime is the settlement AFTER that. Reading nextFundingTime as
// "next" puts every accrual one full period late.
//
// The gap between them is the funding interval, which OKX does not otherwise
// publish here: BTC 8h, LAYER 4h.
//
// instId=ANY returns all 536 swaps in a single call. There is no instType
// batch form -- that returns error 50014 "Parameter instId can not be empty".
// Without ANY this venue would need 400+ requests per poll and would not be
// worth including.
type okxFunding struct {
	InstID          string `json:"instId"`
	FundingRate     string `json:"fundingRate"`
	FundingTime     string `json:"fundingTime"`
	NextFundingTime string `json:"nextFundingTime"`
	MaxFundingRate  string `json:"maxFundingRate"`
	MinFundingRate  string `json:"minFundingRate"`
}

// okxTicker serves both SWAP and SPOT, and volCcy24h MEANS DIFFERENT THINGS
// on each. Verified 2026-08-06:
//
//	SWAP BTC-USDT-SWAP: vol24h 8031466.27 (contracts), volCcy24h 80314.6627 (BTC)
//	                    8031466.27 x ctVal 0.01 = 80314.66 exactly
//	SPOT XASTS-USDT:    vol24h 246.647 (base), volCcy24h 17127.96 (USDT)
//
// So on SWAP volCcy24h is BASE COIN and must be multiplied by price to get USD.
// On SPOT it is already USD. Treating the swap figure as dollars would have
// reported OKX's BTC perp as an $80k market instead of $5.19bn -- rejecting the
// deepest book on the venue while passing genuinely thin ones.
type okxTicker struct {
	InstID    string `json:"instId"`
	Last      string `json:"last"`
	AskPx     string `json:"askPx"`
	AskSz     string `json:"askSz"`
	BidPx     string `json:"bidPx"`
	BidSz     string `json:"bidSz"`
	VolCcy24h string `json:"volCcy24h"`
}

// okxMarkPrice comes from /public/mark-price?instType=SWAP. The ticker
// endpoint carries no mark or index price at all.
type okxMarkPrice struct {
	InstID string `json:"instId"`
	MarkPx string `json:"markPx"`
}

// OKXSource adapts OKX's read-only public API to RateSource.
type OKXSource struct {
	http  *http.Client
	venue Venue
}

// NewOKX builds the OKX rate source.
func NewOKX(fallbackSlippageBps float64) *OKXSource {
	return &OKXSource{
		http:  newVenueHTTP(),
		venue: OKXVenue(fallbackSlippageBps),
	}
}

// Venue implements RateSource.
func (o *OKXSource) Venue() Venue { return o.venue }

// okxNormalise turns BTC-USDT-SWAP into BTCUSDT.
//
// Every venue client emits the same symbol spelling so three months of journal
// can be grouped by asset across venues without a lookup table. The venue's
// native instId is reconstructable by inserting the dash before USDT.
func okxNormalise(instID string) string {
	s := strings.TrimSuffix(instID, "-SWAP")
	return strings.ReplaceAll(s, "-", "")
}

// FundingRates implements RateSource.
//
// Four calls: funding for all swaps, swap tickers, spot tickers, mark prices.
func (o *OKXSource) FundingRates(ctx context.Context) ([]Observation, error) {
	var fund okxResp[okxFunding]
	if err := httpJSON(ctx, o.http, okxBase+"/api/v5/public/funding-rate?instId=ANY", &fund); err != nil {
		return nil, err
	}
	if fund.Code != "0" {
		return nil, fmt.Errorf("exchange: okx funding-rate code=%s: %s", fund.Code, fund.Msg)
	}

	perpTickers := map[string]okxTicker{}
	var pt okxResp[okxTicker]
	if err := httpJSON(ctx, o.http, okxBase+"/api/v5/market/tickers?instType=SWAP", &pt); err == nil && pt.Code == "0" {
		for _, t := range pt.Data {
			perpTickers[t.InstID] = t
		}
	}

	spotTickers := map[string]okxTicker{}
	var st okxResp[okxTicker]
	if err := httpJSON(ctx, o.http, okxBase+"/api/v5/market/tickers?instType=SPOT", &st); err == nil && st.Code == "0" {
		for _, t := range st.Data {
			spotTickers[t.InstID] = t
		}
	}

	marks := map[string]float64{}
	var mp okxResp[okxMarkPrice]
	if err := httpJSON(ctx, o.http, okxBase+"/api/v5/public/mark-price?instType=SWAP", &mp); err == nil && mp.Code == "0" {
		for _, m := range mp.Data {
			if v, ok := parseF(m.MarkPx); ok {
				marks[m.InstID] = v
			}
		}
	}

	now := time.Now().UTC()
	out := make([]Observation, 0, len(fund.Data))

	for _, f := range fund.Data {
		// USDT-settled swaps only. instId=ANY also returns USD- and
		// USDC-margined contracts, which are a different instrument with
		// different collateral and must not be pooled with these.
		if !strings.HasSuffix(f.InstID, "-USDT-SWAP") {
			continue
		}

		rate, ok := parseF(f.FundingRate)
		if !ok {
			continue
		}
		ratePct := rate * 100

		// Reject anything outside the venue's own published bounds: an
		// implausible rate is a parsing error, not an opportunity.
		if hi, ok := parseF(f.MaxFundingRate); ok && hi > 0 && ratePct > hi*100 {
			continue
		}
		if lo, ok := parseF(f.MinFundingRate); ok && lo < 0 && ratePct < lo*100 {
			continue
		}

		// fundingTime is the NEXT settlement; nextFundingTime is the one after.
		// Their difference is the interval.
		nextSettle := parseMsString(f.FundingTime)
		afterNext := parseMsString(f.NextFundingTime)
		intervalHours := o.venue.DefaultFundingIntervalHours
		if !nextSettle.IsZero() && !afterNext.IsZero() && afterNext.After(nextSettle) {
			if h := afterNext.Sub(nextSettle).Hours(); h > 0 && h <= 24 {
				intervalHours = h
			}
		}

		symbol := okxNormalise(f.InstID)
		spotID := strings.TrimSuffix(f.InstID, "-SWAP")

		obs := Observation{
			Venue:           o.venue.Name,
			Symbol:          symbol,
			FundingRatePct:  ratePct,
			IntervalHours:   intervalHours,
			MarkPrice:       marks[f.InstID],
			NextFundingTime: nextSettle,
			ObservedAt:      now,
		}

		perp, hasPerp := perpTickers[f.InstID]
		var perpLast float64
		if hasPerp {
			bid, okB := parseF(perp.BidPx)
			ask, okA := parseF(perp.AskPx)
			bidSz, _ := parseF(perp.BidSz)
			askSz, _ := parseF(perp.AskSz)
			perpLast, _ = parseF(perp.Last)

			if okB && okA {
				obs.PerpHalfSpreadBps = halfSpreadBps(bid, ask)
				obs.PerpTopOfBookUSD = topOfBookUSD(bid, bidSz, ask, askSz)
			} else {
				hasPerp = false
			}

			// SWAP volCcy24h is BASE COIN. Convert to USD with the last price.
			if baseVol, ok := parseF(perp.VolCcy24h); ok && perpLast > 0 {
				obs.PerpQuoteVolume24hUSD = baseVol * perpLast
			}
			if obs.MarkPrice == 0 {
				obs.MarkPrice = perpLast
			}
		}

		spot, hasSpot := spotTickers[spotID]
		obs.SpotSymbolAvailable = hasSpot
		if hasSpot {
			sBid, okB := parseF(spot.BidPx)
			sAsk, okA := parseF(spot.AskPx)
			sBidSz, _ := parseF(spot.BidSz)
			sAskSz, _ := parseF(spot.AskSz)

			// SPOT volCcy24h is already QUOTE currency. No conversion.
			obs.SpotQuoteVolume24hUSD, _ = parseF(spot.VolCcy24h)

			if okB && okA {
				obs.SpotHalfSpreadBps = halfSpreadBps(sBid, sAsk)
				obs.SpotTopOfBookUSD = topOfBookUSD(sBid, sBidSz, sAsk, sAskSz)
			} else {
				hasSpot = false
			}

			// OKX's ticker endpoints carry no index price, and its
			// index-tickers endpoint returns a different shape that was not
			// verified during this build. Spot last is used instead, so
			// basis_bps here is PERP MARK vs SPOT LAST -- the actual
			// convergence gap a cash-and-carry closes -- rather than mark vs
			// the venue's index as on Binance. Diagnostic only; nothing gates
			// on it. Do not compare basis_bps across venues without reading
			// this note.
			if sLast, ok := parseF(spot.Last); ok {
				obs.IndexPrice = sLast
			}
		}

		obs.LiquidityMeasured = hasPerp && hasSpot && obs.SpotTopOfBookUSD > 0 && obs.PerpTopOfBookUSD > 0

		out = append(out, obs)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("exchange: okx returned %d funding entries but no usable USDT swaps", len(fund.Data))
	}
	return out, nil
}
