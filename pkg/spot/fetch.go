package spot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FETCHING SPOT UNIVERSES
//
// Three public endpoints, no authentication. Each venue is parsed against its
// own field names and status vocabulary, because they disagree: Binance says
// status "TRADING", Bybit says "Trading", OKX says state "live". Treating any
// of those as a boolean would quietly admit delisted markets.
//
// Fetch and parse are separated on purpose. A parser bug here does not produce
// an error -- it produces an empty or wrong universe, which reads downstream as
// "this coin has no spot market" and routes a perfectly good cash-and-carry
// into cross-venue. That failure is silent, so the parsers are tested against
// recorded payloads rather than trusted.
//
// Only USDT-quoted markets are kept: the perp universe is USDT-quoted, and a
// spot market in another quote cannot hedge a USDT perp without a second
// currency leg nobody has priced.

const spotQuote = "USDT"

// Fetcher reads one venue's spot universe.
type Fetcher interface {
	Venue() string
	URL() string
	Parse(raw []byte) ([]Market, error)
}

// Fetchers returns every venue this package can scan.
func Fetchers() []Fetcher {
	return []Fetcher{binanceSpot{}, bybitSpot{}, okxSpot{}}
}

// Scan runs every fetcher and returns a Table.
//
// A venue that fails is recorded as a failure with no markets, so Spot()
// answers Unknown for it rather than Absent. One venue being unreachable must
// not silently redirect every coin into the worse structure.
func Scan(ctx context.Context, c *http.Client, now time.Time) *Table {
	t := NewTable(now)
	for _, f := range Fetchers() {
		markets, err := fetchOne(ctx, c, f)
		if err != nil {
			t.SetResult(VenueResult{
				Venue: f.Venue(), OK: false, Err: err.Error(), FetchedAt: time.Now().UTC(),
			})
			continue
		}
		for _, m := range markets {
			t.Put(m)
		}
		t.SetResult(VenueResult{
			Venue: f.Venue(), OK: true, Count: len(markets), FetchedAt: time.Now().UTC(),
		})
	}
	return t
}

func fetchOne(ctx context.Context, c *http.Client, f Fetcher) ([]Market, error) {
	raw, err := getBytes(ctx, c, f.URL())
	if err != nil {
		return nil, fmt.Errorf("%s spot: %w", f.Venue(), err)
	}
	m, err := f.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s spot: %w", f.Venue(), err)
	}
	return m, nil
}

func getBytes(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Include a slice of the body: a 451 geo-block page and a 500 are very
		// different problems, and "status 451" alone does not say which.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode,
			strings.TrimSpace(string(snippet)))
	}
	return io.ReadAll(resp.Body)
}

func atof(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// ------------------------------------------------------------------ binance

type binanceSpot struct{}

func (binanceSpot) Venue() string { return "binance" }
func (binanceSpot) URL() string   { return "https://api.binance.com/api/v3/exchangeInfo" }

func (binanceSpot) Parse(raw []byte) ([]Market, error) {
	var r struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Filters    []struct {
				FilterType  string `json:"filterType"`
				MinNotional string `json:"minNotional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decoding exchangeInfo: %w", err)
	}
	if len(r.Symbols) == 0 {
		// An empty list is not an empty exchange. Erroring keeps it from being
		// written as "binance has no spot markets".
		return nil, fmt.Errorf("exchangeInfo returned no symbols")
	}

	var out []Market
	for _, s := range r.Symbols {
		if s.QuoteAsset != spotQuote || s.Status != "TRADING" {
			continue
		}
		m := Market{
			Venue: "binance", Coin: strings.ToUpper(s.BaseAsset),
			Symbol: s.Symbol, Quote: s.QuoteAsset,
		}
		for _, f := range s.Filters {
			if f.FilterType == "NOTIONAL" || f.FilterType == "MIN_NOTIONAL" {
				if v := atof(f.MinNotional); v > 0 {
					m.MinNotionalUSD = v
				}
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// -------------------------------------------------------------------- bybit

type bybitSpot struct{}

func (bybitSpot) Venue() string { return "bybit" }
func (bybitSpot) URL() string {
	return "https://api.bybit.com/v5/market/instruments-info?category=spot&limit=1000"
}

func (bybitSpot) Parse(raw []byte) ([]Market, error) {
	var r struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			List []struct {
				Symbol        string `json:"symbol"`
				BaseCoin      string `json:"baseCoin"`
				QuoteCoin     string `json:"quoteCoin"`
				Status        string `json:"status"`
				LotSizeFilter struct {
					MinOrderAmt string `json:"minOrderAmt"`
				} `json:"lotSizeFilter"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decoding instruments-info: %w", err)
	}
	if r.RetCode != 0 {
		return nil, fmt.Errorf("retCode %d: %s", r.RetCode, r.RetMsg)
	}
	if len(r.Result.List) == 0 {
		return nil, fmt.Errorf("instruments-info returned no symbols")
	}

	var out []Market
	for _, s := range r.Result.List {
		if s.QuoteCoin != spotQuote || s.Status != "Trading" {
			continue
		}
		out = append(out, Market{
			Venue: "bybit", Coin: strings.ToUpper(s.BaseCoin),
			Symbol: s.Symbol, Quote: s.QuoteCoin,
			MinNotionalUSD: atof(s.LotSizeFilter.MinOrderAmt),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------- okx

type okxSpot struct{}

func (okxSpot) Venue() string { return "okx" }
func (okxSpot) URL() string {
	return "https://www.okx.com/api/v5/public/instruments?instType=SPOT"
}

func (okxSpot) Parse(raw []byte) ([]Market, error) {
	var r struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID   string `json:"instId"`
			BaseCcy  string `json:"baseCcy"`
			QuoteCcy string `json:"quoteCcy"`
			State    string `json:"state"`
			MinSz    string `json:"minSz"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decoding instruments: %w", err)
	}
	if r.Code != "0" {
		return nil, fmt.Errorf("code %s: %s", r.Code, r.Msg)
	}
	if len(r.Data) == 0 {
		return nil, fmt.Errorf("instruments returned no symbols")
	}

	var out []Market
	for _, s := range r.Data {
		if s.QuoteCcy != spotQuote || s.State != "live" {
			continue
		}
		// OKX minSz is in BASE units, not quote. Recording it as a USD minimum
		// would be wrong by the price of the coin, so it is left unset rather
		// than converted with a price this package does not have.
		out = append(out, Market{
			Venue: "okx", Coin: strings.ToUpper(s.BaseCcy),
			Symbol: s.InstID, Quote: s.QuoteCcy,
		})
	}
	return out, nil
}
