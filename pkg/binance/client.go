// Package binance is a read-only client for Binance USDT-M futures public
// endpoints. No API key, no signing, no order placement.
//
// There is deliberately no code in this package that can place an order. Paper
// mode is not enforced by a config flag that someone can flip at 2am -- it is
// enforced by the absence of the capability. Adding execution means adding a
// new package and a new dependency on credentials, which is a visible act.
//
// Field shapes here were verified against the live API from the Frankfurt VPS
// on 2026-08-05 before this parser was written, not assumed from docs:
//
//	{"symbol":"BTCUSDT","markPrice":"64136.20000000",
//	 "indexPrice":"64168.77391304","estimatedSettlePrice":"64175.23943920",
//	 "lastFundingRate":"0.00003425","interestRate":"0.00010000",
//	 "nextFundingTime":1785945600000,"time":1785923611000}
//
// Every price and rate is a STRING. Decoding these into float64 fails at
// runtime with "cannot unmarshal string into Go value of type float64", so
// each is parsed explicitly below.
package binance

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

const (
	// FuturesBase is the USDT-M futures REST host.
	FuturesBase = "https://fapi.binance.com"

	// defaultTimeout bounds a single request. The monitor polls on a slow
	// cadence, so a hung connection must not wedge the loop.
	defaultTimeout = 15 * time.Second
)

// Client is safe for concurrent use.
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// New returns a client with sane timeouts.
func New() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: defaultTimeout,
		},
		BaseURL: FuturesBase,
	}
}

// rawPremiumIndex mirrors the wire format exactly. It exists only to be
// converted into PremiumIndex; nothing outside this file should use it.
type rawPremiumIndex struct {
	Symbol               string `json:"symbol"`
	MarkPrice            string `json:"markPrice"`
	IndexPrice           string `json:"indexPrice"`
	EstimatedSettlePrice string `json:"estimatedSettlePrice"`
	LastFundingRate      string `json:"lastFundingRate"`
	InterestRate         string `json:"interestRate"`
	NextFundingTime      int64  `json:"nextFundingTime"`
	Time                 int64  `json:"time"`
}

// PremiumIndex is one symbol's current funding and pricing state.
type PremiumIndex struct {
	Symbol     string
	MarkPrice  float64
	IndexPrice float64

	// LastFundingRatePct is the rate for one settlement interval as a
	// PERCENTAGE. The API returns a decimal fraction ("0.00003425"); this
	// field is that value times 100 (0.003425). economics.Opportunity expects
	// percent, and doing the conversion once, here, is the only defence
	// against a factor-of-100 error propagating into the entry gate.
	LastFundingRatePct float64

	NextFundingTime time.Time
	Time            time.Time
}

// BasisBps is the futures-vs-index spread in basis points. Positive means the
// perp trades above the index, which is the condition that normally
// accompanies positive funding.
//
// This is NOT tradeable edge. It is a sanity signal: a mark price far from the
// index on a thin symbol means the funding number is being computed off a
// price nobody can actually transact at.
func (p PremiumIndex) BasisBps() float64 {
	if p.IndexPrice == 0 {
		return 0
	}
	return (p.MarkPrice - p.IndexPrice) / p.IndexPrice * 10000
}

// PremiumIndex fetches the current funding rate and mark price for every
// symbol on USDT-M futures in a single request.
//
// Called without a symbol parameter the endpoint returns a JSON ARRAY; called
// with one it returns a bare OBJECT. This method always omits the symbol, so
// the response is always an array. Weight is 10, against a 2400/min budget --
// polling this once a minute uses 0.4% of the allowance.
func (c *Client) PremiumIndex(ctx context.Context) ([]PremiumIndex, error) {
	body, err := c.get(ctx, "/fapi/v1/premiumIndex")
	if err != nil {
		return nil, err
	}

	var raw []rawPremiumIndex
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("binance: decoding premiumIndex array: %w (first 200 bytes: %.200s)", err, body)
	}

	out := make([]PremiumIndex, 0, len(raw))
	for _, r := range raw {
		p, err := r.convert()
		if err != nil {
			// One malformed symbol must not discard the whole scan. Skip it;
			// the caller counts what it got against what it expected.
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("binance: premiumIndex returned %d entries but none parsed", len(raw))
	}
	return out, nil
}

func (r rawPremiumIndex) convert() (PremiumIndex, error) {
	mark, err := parseFloat(r.MarkPrice, "markPrice", r.Symbol)
	if err != nil {
		return PremiumIndex{}, err
	}
	index, err := parseFloat(r.IndexPrice, "indexPrice", r.Symbol)
	if err != nil {
		return PremiumIndex{}, err
	}
	rate, err := parseFloat(r.LastFundingRate, "lastFundingRate", r.Symbol)
	if err != nil {
		return PremiumIndex{}, err
	}

	return PremiumIndex{
		Symbol:     r.Symbol,
		MarkPrice:  mark,
		IndexPrice: index,
		// Decimal fraction to percent. This is the only place this
		// multiplication happens.
		LastFundingRatePct: rate * 100,
		NextFundingTime:    msToTime(r.NextFundingTime),
		Time:               msToTime(r.Time),
	}, nil
}

// IsUSDTPerp reports whether a symbol is a plain USDT-margined perpetual.
//
// The premiumIndex response also carries dated quarterly contracts such as
// BTCUSDT_250926. Those do not pay funding on the 8h schedule and must not be
// assessed as if they do.
func IsUSDTPerp(symbol string) bool {
	return strings.HasSuffix(symbol, "USDT") && !strings.Contains(symbol, "_")
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("binance: building request for %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("binance: reading %s response: %w", path, err)
	}

	// 429 is a rate-limit warning; 418 means Binance has banned this IP for
	// ignoring 429s. Both must be surfaced loudly rather than retried blindly,
	// because retrying a 418 extends the ban.
	switch resp.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("binance: rate limited (429) on %s, Retry-After=%q; BACK OFF", path, resp.Header.Get("Retry-After"))
	case http.StatusTeapot:
		return nil, fmt.Errorf("binance: IP BANNED (418) on %s, Retry-After=%q; do not retry", path, resp.Header.Get("Retry-After"))
	default:
		return nil, fmt.Errorf("binance: %s returned HTTP %d: %.200s", path, resp.StatusCode, body)
	}
}

func parseFloat(s, field, symbol string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("binance: %s: empty %s", symbol, field)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("binance: %s: parsing %s=%q: %w", symbol, field, s, err)
	}
	return v, nil
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
