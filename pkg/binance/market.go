package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// SpotBase is the spot REST host. Spot and futures live on different hosts,
// which is why this package needs its own absolute-URL fetcher alongside the
// futures-relative one in client.go.
const SpotBase = "https://api.binance.com"

// BookTicker is best bid and ask for one symbol.
//
// Shapes verified live from the Frankfurt VPS on 2026-08-05. Futures carries
// two extra fields; spot does not. Both quote every number as a string:
//
//	futures /fapi/v1/ticker/bookTicker  (728 entries)
//	  {"symbol":"BTCUSDT","bidPrice":"64166.10","bidQty":"6.304",
//	   "askPrice":"64166.20","askQty":"13.871","time":...,"lastUpdateId":...}
//
//	spot    /api/v3/ticker/bookTicker   (3673 entries)
//	  {"symbol":"ETHBTC","bidPrice":"0.02919000","bidQty":"40.88670000",
//	   "askPrice":"0.02920000","askQty":"67.28450000"}
type BookTicker struct {
	Symbol   string
	BidPrice float64
	BidQty   float64
	AskPrice float64
	AskQty   float64
}

type rawBookTicker struct {
	Symbol   string `json:"symbol"`
	BidPrice string `json:"bidPrice"`
	BidQty   string `json:"bidQty"`
	AskPrice string `json:"askPrice"`
	AskQty   string `json:"askQty"`
}

// Mid is the midpoint of the book.
func (b BookTicker) Mid() float64 {
	if b.BidPrice <= 0 || b.AskPrice <= 0 {
		return 0
	}
	return (b.BidPrice + b.AskPrice) / 2
}

// HalfSpreadBps is the cost, in basis points, of crossing from mid to the far
// side of the book for one leg.
//
// This is a FLOOR on execution cost, not the cost itself. It assumes the whole
// order fills at the touch. If the order is larger than the quoted size at the
// touch -- see TopOfBookUSD -- the real cost is worse, and this number becomes
// a lower bound that flatters the trade.
//
// Measured on 2026-08-05: BTCUSDT 0.008 bps, HFTUSDT 2.90, BNCUSDT 5.93.
// A single assumed constant cannot cover a range that wide.
func (b BookTicker) HalfSpreadBps() float64 {
	mid := b.Mid()
	if mid <= 0 {
		return 0
	}
	return (b.AskPrice - b.BidPrice) / mid * 10000 / 2
}

// TopOfBookUSD is the smaller of the two sides at the touch, in quote
// currency. An order above this size cannot fill at the touch, so
// HalfSpreadBps understates its cost.
func (b BookTicker) TopOfBookUSD() float64 {
	bid := b.BidPrice * b.BidQty
	ask := b.AskPrice * b.AskQty
	if bid < ask {
		return bid
	}
	return ask
}

// Valid reports whether the book is usable for a cost estimate.
func (b BookTicker) Valid() bool {
	return b.BidPrice > 0 && b.AskPrice > 0 && b.AskPrice >= b.BidPrice
}

// FuturesBookTickers returns the best bid/ask for every futures symbol.
// Returns 728 entries against 855 from premiumIndex -- symbols absent here
// have no measurable spread and must be treated as UNMEASURED, never as free.
func (c *Client) FuturesBookTickers(ctx context.Context) (map[string]BookTicker, error) {
	return c.bookTickers(ctx, c.BaseURL+"/fapi/v1/ticker/bookTicker")
}

// SpotBookTickers returns the best bid/ask for every spot symbol.
func (c *Client) SpotBookTickers(ctx context.Context) (map[string]BookTicker, error) {
	return c.bookTickers(ctx, SpotBase+"/api/v3/ticker/bookTicker")
}

func (c *Client) bookTickers(ctx context.Context, url string) (map[string]BookTicker, error) {
	body, err := c.getAbs(ctx, url)
	if err != nil {
		return nil, err
	}

	var raw []rawBookTicker
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("binance: decoding bookTicker from %s: %w (first 200 bytes: %.200s)", url, err, body)
	}

	out := make(map[string]BookTicker, len(raw))
	for _, r := range raw {
		bt := BookTicker{Symbol: r.Symbol}
		bt.BidPrice, _ = parseOptional(r.BidPrice)
		bt.BidQty, _ = parseOptional(r.BidQty)
		bt.AskPrice, _ = parseOptional(r.AskPrice)
		bt.AskQty, _ = parseOptional(r.AskQty)
		if bt.Valid() {
			out[r.Symbol] = bt
		}
	}
	return out, nil
}

type rawExchangeInfo struct {
	Symbols []struct {
		Symbol               string `json:"symbol"`
		Status               string `json:"status"`
		QuoteAsset           string `json:"quoteAsset"`
		IsSpotTradingAllowed bool   `json:"isSpotTradingAllowed"`
	} `json:"symbols"`
}

// SpotUSDTSymbols returns the set of USDT-quoted spot pairs currently TRADING.
//
// This is the filter that was missing and that invalidated the first scan.
// Of 806 USDT perpetuals on 2026-08-05, only 371 had a matching spot pair.
// The other 435 are perp-only listings: cash-and-carry on them is not
// expensive, it is IMPOSSIBLE, because there is nothing to buy for the long
// leg. Ranking them produced a top-20 in which six of the first eight symbols
// could not be traded at any price.
func (c *Client) SpotUSDTSymbols(ctx context.Context) (map[string]bool, error) {
	body, err := c.getAbs(ctx, SpotBase+"/api/v3/exchangeInfo")
	if err != nil {
		return nil, err
	}

	var raw rawExchangeInfo
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("binance: decoding spot exchangeInfo: %w", err)
	}

	out := make(map[string]bool, 512)
	for _, s := range raw.Symbols {
		if s.QuoteAsset == "USDT" && s.Status == "TRADING" && s.IsSpotTradingAllowed {
			out[s.Symbol] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("binance: spot exchangeInfo parsed %d symbols but no tradable USDT pairs", len(raw.Symbols))
	}
	return out, nil
}

type rawTicker24h struct {
	Symbol      string `json:"symbol"`
	QuoteVolume string `json:"quoteVolume"`
}

// Futures24hQuoteVolume returns 24h volume in USDT per symbol.
//
// Scale matters here more than it looks: on 2026-08-05 BTCUSDT traded
// $8.63bn while BNCUSDT traded $451k -- a factor of 19,000. A $10,000
// position is 0.0001% of one and 2.2% of the other, and you must do it twice.
func (c *Client) Futures24hQuoteVolume(ctx context.Context) (map[string]float64, error) {
	body, err := c.get(ctx, "/fapi/v1/ticker/24hr")
	if err != nil {
		return nil, err
	}

	var raw []rawTicker24h
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("binance: decoding ticker/24hr: %w (first 200 bytes: %.200s)", err, body)
	}

	out := make(map[string]float64, len(raw))
	for _, r := range raw {
		if v, ok := parseOptional(r.QuoteVolume); ok {
			out[r.Symbol] = v
		}
	}
	return out, nil
}

// getAbs fetches an absolute URL. client.go's get() is relative to the futures
// host; spot endpoints live on a different host entirely.
func (c *Client) getAbs(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("binance: building request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("binance: reading %s: %w", url, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// A wrong path returns Binance's HTML error page. That is how
		// /fapi/v1/bookTicker (missing the /ticker segment) silently produced
		// CSS instead of JSON during this build. Catch it here rather than in
		// a confusing unmarshal error thirty lines away.
		if strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
			return nil, fmt.Errorf("binance: %s returned HTML, not JSON -- wrong path? (first 120 bytes: %.120s)", url, body)
		}
		return body, nil
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("binance: rate limited (429) on %s, Retry-After=%q; BACK OFF", url, resp.Header.Get("Retry-After"))
	case http.StatusTeapot:
		return nil, fmt.Errorf("binance: IP BANNED (418) on %s, Retry-After=%q; do not retry", url, resp.Header.Get("Retry-After"))
	default:
		return nil, fmt.Errorf("binance: %s returned HTTP %d: %.200s", url, resp.StatusCode, body)
	}
}

// parseOptional parses a numeric string, reporting failure rather than
// returning a silent zero. A zero price is not the same as an unparseable one.
func parseOptional(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
