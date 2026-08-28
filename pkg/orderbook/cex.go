package orderbook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// BINANCE AND BYBIT PERP DEPTH AND FUNDING INTERVALS, PUBLIC AND UNAUTHENTICATED
//
// THREE TRAPS LIVE HERE
//
// 1. THE FUNDING INTERVAL IS PER SYMBOL, NOT PER VENUE.
//    This one cost real money. pkg/hyperliquid hardcoded Binance at 8 hours
//    for every symbol. KAITOUSDT settles every FOUR -- confirmed from Binance's
//    own settlement history and from /fapi/v1/fundingInfo. The spread on that
//    pair read 4.4x too rich for a full day and two positions closed at a loss
//    partly because of it. Both venues publish the true figure and always did.
//
// 2. BYBIT RETURNS HTTP 200 ON FAILURE. Errors arrive as retCode in the body.
//    Checking only the status code decodes an error into an EMPTY BOOK, and an
//    unflagged empty book reads as "no liquidity" rather than "no answer".
//
// 3. SYMBOL NAMES DO NOT MATCH. Hyperliquid says KAITO; the CEXs say
//    KAITOUSDT. Hyperliquid prefixes small-denomination contracts with k where
//    the CEXs use 1000. Every symbol is checked against the venue's own
//    instrument list and REFUSED if absent.

const (
	binanceFapiBase = "https://fapi.binance.com"
	bybitBase       = "https://api.bybit.com"
)

func getJSON(ctx context.Context, hc *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		snip := raw
		if len(snip) > 300 {
			snip = snip[:300]
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snip)
	}
	return json.Unmarshal(raw, out)
}

// FundingInterval is how often a SYMBOL settles funding.
//
// Explicit records whether the venue PUBLISHED this figure for this symbol, or
// whether it is a venue-wide default applied because the symbol was absent from
// the override list. Both are usable; only one is verified, and the difference
// is exactly what went wrong on 2026-08-12.
type FundingInterval struct {
	Hours    float64
	Explicit bool
	Ok       bool
}

// PerpReader is a venue that can be asked for a perp book and a symbol's
// settlement interval.
type PerpReader interface {
	Venue() string
	LoadSymbols(ctx context.Context) error
	SymbolCount() int
	ResolveCoin(coin string) (symbol string, ok bool)
	Book(ctx context.Context, symbol string) (Book, error)
	FundingIntervalHours(symbol string) FundingInterval

	// Symbols is every tradable symbol on this venue.
	//
	// A new listing is defined by APPEARING, so the universe has to come from
	// the venue itself. Deriving it from Hyperliquid's 232 coins would miss
	// most new Binance listings -- Binance lists 527 -- including DOSUSDT,
	// which is exactly what a listing watcher exists to catch.
	Symbols() []string
}

// candidateSymbols returns the names worth checking for a Hyperliquid coin, in
// order of confidence. Whichever the venue's instrument list confirms is used;
// none is assumed.
func candidateSymbols(coin string) []string {
	c := strings.ToUpper(strings.TrimSpace(coin))
	if c == "" {
		return nil
	}
	out := []string{c + "USDT"}
	if strings.HasPrefix(coin, "k") && len(coin) > 1 {
		rest := strings.ToUpper(coin[1:])
		out = append(out, "1000"+rest+"USDT", rest+"USDT")
	}
	if strings.HasPrefix(c, "1000") {
		out = append(out, strings.TrimPrefix(c, "1000")+"USDT")
	}
	return out
}

func parsePairs(rows [][]string) []Level {
	out := make([]Level, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		px, okP := ParseNum(row[0])
		sz, okS := ParseNum(row[1])
		if !okP || !okS || px <= 0 || sz <= 0 {
			continue
		}
		out = append(out, Level{Px: px, Sz: sz})
	}
	return out
}

// --- Binance USD-M futures ----------------------------------------------------

type BinancePerp struct {
	HTTP    *http.Client
	BaseURL string
	Limit   int

	mu        sync.RWMutex
	symbols   map[string]bool
	intervals map[string]float64 // published overrides only
	loaded    time.Time
}

func NewBinancePerp() *BinancePerp {
	return &BinancePerp{
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: binanceFapiBase,
		Limit:   20,
	}
}

func (b *BinancePerp) Venue() string { return "binance" }

func (b *BinancePerp) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (b *BinancePerp) base() string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	return binanceFapiBase
}

// LoadSymbols fetches tradable perpetuals AND their funding intervals.
func (b *BinancePerp) LoadSymbols(ctx context.Context) error {
	var r struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			Status       string `json:"status"`
			ContractType string `json:"contractType"`
			QuoteAsset   string `json:"quoteAsset"`
		} `json:"symbols"`
	}
	if err := getJSON(ctx, b.client(), b.base()+"/fapi/v1/exchangeInfo", &r); err != nil {
		return fmt.Errorf("binance: exchangeInfo: %w", err)
	}

	set := make(map[string]bool, len(r.Symbols))
	for _, s := range r.Symbols {
		if s.Status == "TRADING" && s.ContractType == "PERPETUAL" && s.QuoteAsset == "USDT" {
			set[s.Symbol] = true
		}
	}
	if len(set) == 0 {
		return fmt.Errorf("binance: exchangeInfo returned no tradable USDT perpetuals")
	}

	// Binance publishes ONLY symbols whose settings differ from its defaults,
	// so an absent symbol is 8h -- documented, unverified, and marked as such.
	var fi []struct {
		Symbol               string `json:"symbol"`
		FundingIntervalHours int    `json:"fundingIntervalHours"`
	}
	intervals := map[string]float64{}
	if err := getJSON(ctx, b.client(), b.base()+"/fapi/v1/fundingInfo", &fi); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING binance fundingInfo unavailable (%v); "+
			"every interval falls back to the unverified 8h default\n", err)
	} else {
		for _, r := range fi {
			if r.FundingIntervalHours > 0 {
				intervals[r.Symbol] = float64(r.FundingIntervalHours)
			}
		}
	}

	b.mu.Lock()
	b.symbols, b.intervals, b.loaded = set, intervals, time.Now().UTC()
	b.mu.Unlock()
	return nil
}

// FundingIntervalHours returns the published interval, or the documented 8h
// default marked NOT explicit.
func (b *BinancePerp) FundingIntervalHours(symbol string) FundingInterval {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.symbols[symbol] {
		return FundingInterval{}
	}
	if h, ok := b.intervals[symbol]; ok {
		return FundingInterval{Hours: h, Explicit: true, Ok: true}
	}
	return FundingInterval{Hours: 8, Explicit: false, Ok: true}
}

func (b *BinancePerp) Symbols() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.symbols))
	for s := range b.symbols {
		out = append(out, s)
	}
	return out
}

func (b *BinancePerp) SymbolCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.symbols)
}

func (b *BinancePerp) ResolveCoin(coin string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.symbols) == 0 {
		return "", false
	}
	for _, cand := range candidateSymbols(coin) {
		if b.symbols[cand] {
			return cand, true
		}
	}
	return "", false
}

func (b *BinancePerp) Book(ctx context.Context, symbol string) (Book, error) {
	out := Book{Venue: "binance", Symbol: symbol}

	limit := b.Limit
	if limit <= 0 {
		limit = 20
	}
	u := fmt.Sprintf("%s/fapi/v1/depth?symbol=%s&limit=%d",
		b.base(), url.QueryEscape(symbol), limit)

	var r struct {
		Bids [][]string `json:"bids"`
		Asks [][]string `json:"asks"`
		E    int64      `json:"E"`
	}
	if err := getJSON(ctx, b.client(), u, &r); err != nil {
		return out, fmt.Errorf("binance: depth %s: %w", symbol, err)
	}
	if r.E > 0 {
		out.At = time.UnixMilli(r.E).UTC()
	}
	out.Bids = parsePairs(r.Bids)
	out.Asks = parsePairs(r.Asks)
	out.Finalise()
	return out, nil
}

// --- Bybit linear -------------------------------------------------------------

type BybitPerp struct {
	HTTP    *http.Client
	BaseURL string
	Limit   int

	mu        sync.RWMutex
	symbols   map[string]bool
	intervals map[string]float64
	loaded    time.Time
}

func NewBybitPerp() *BybitPerp {
	return &BybitPerp{
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: bybitBase,
		Limit:   25,
	}
}

func (b *BybitPerp) Venue() string { return "bybit" }

func (b *BybitPerp) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (b *BybitPerp) base() string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	return bybitBase
}

// bybitEnvelope is Bybit's v5 wrapper. retCode is where failures live; the
// HTTP status is 200 either way.
type bybitEnvelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
	Time    int64           `json:"time"`
}

func (c bybitEnvelope) err(what string) error {
	if c.RetCode == 0 {
		return nil
	}
	return fmt.Errorf("bybit: %s: retCode %d: %s", what, c.RetCode, c.RetMsg)
}

// LoadSymbols fetches tradable linear perpetuals and their funding intervals.
func (b *BybitPerp) LoadSymbols(ctx context.Context) error {
	set := map[string]bool{}
	intervals := map[string]float64{}
	cursor := ""

	for page := 0; page < 20; page++ {
		u := b.base() + "/v5/market/instruments-info?category=linear&limit=1000"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}

		var env bybitEnvelope
		if err := getJSON(ctx, b.client(), u, &env); err != nil {
			return fmt.Errorf("bybit: instruments-info: %w", err)
		}
		if err := env.err("instruments-info"); err != nil {
			return err
		}

		var res struct {
			List []struct {
				Symbol       string `json:"symbol"`
				Status       string `json:"status"`
				QuoteCoin    string `json:"quoteCoin"`
				ContractType string `json:"contractType"`
				// MINUTES. 240 = 4h, 480 = 8h. Using the wrong unit here is
				// the same class of error as assuming the venue default.
				FundingInterval int `json:"fundingInterval"`
			} `json:"list"`
			NextPageCursor string `json:"nextPageCursor"`
		}
		if err := json.Unmarshal(env.Result, &res); err != nil {
			return fmt.Errorf("bybit: decoding instruments: %w", err)
		}

		for _, s := range res.List {
			if s.Status == "Trading" && s.QuoteCoin == "USDT" &&
				strings.Contains(s.ContractType, "Perpetual") {
				set[s.Symbol] = true
				if s.FundingInterval > 0 {
					intervals[s.Symbol] = float64(s.FundingInterval) / 60
				}
			}
		}
		if res.NextPageCursor == "" || res.NextPageCursor == cursor {
			break
		}
		cursor = res.NextPageCursor
	}

	if len(set) == 0 {
		return fmt.Errorf("bybit: instruments-info returned no tradable USDT perpetuals")
	}

	b.mu.Lock()
	b.symbols, b.intervals, b.loaded = set, intervals, time.Now().UTC()
	b.mu.Unlock()
	return nil
}

// FundingIntervalHours returns Bybit's published interval. Bybit states it for
// every instrument, so a missing value is a REFUSAL, never a default.
func (b *BybitPerp) FundingIntervalHours(symbol string) FundingInterval {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if h, ok := b.intervals[symbol]; ok && h > 0 {
		return FundingInterval{Hours: h, Explicit: true, Ok: true}
	}
	return FundingInterval{}
}

func (b *BybitPerp) Symbols() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.symbols))
	for s := range b.symbols {
		out = append(out, s)
	}
	return out
}

func (b *BybitPerp) SymbolCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.symbols)
}

func (b *BybitPerp) ResolveCoin(coin string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.symbols) == 0 {
		return "", false
	}
	for _, cand := range candidateSymbols(coin) {
		if b.symbols[cand] {
			return cand, true
		}
	}
	return "", false
}

func (b *BybitPerp) Book(ctx context.Context, symbol string) (Book, error) {
	out := Book{Venue: "bybit", Symbol: symbol}

	limit := b.Limit
	if limit <= 0 {
		limit = 25
	}
	u := fmt.Sprintf("%s/v5/market/orderbook?category=linear&symbol=%s&limit=%d",
		b.base(), url.QueryEscape(symbol), limit)

	var env bybitEnvelope
	if err := getJSON(ctx, b.client(), u, &env); err != nil {
		return out, fmt.Errorf("bybit: orderbook %s: %w", symbol, err)
	}
	// Checked BEFORE decoding, so a retCode failure cannot be mistaken for an
	// empty book.
	if err := env.err("orderbook " + symbol); err != nil {
		return out, err
	}

	var res struct {
		S  string     `json:"s"`
		B  [][]string `json:"b"`
		A  [][]string `json:"a"`
		Ts int64      `json:"ts"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return out, fmt.Errorf("bybit: decoding orderbook %s: %w", symbol, err)
	}
	if res.Ts > 0 {
		out.At = time.UnixMilli(res.Ts).UTC()
	}
	out.Bids = parsePairs(res.B)
	out.Asks = parsePairs(res.A)
	out.Finalise()
	return out, nil
}

// --- registry -----------------------------------------------------------------

// Readers builds the CEX readers. Hyperliquid is absent on purpose: its book
// comes from the same /info endpoint as its funding rates, and splitting that
// across packages would mean two clients for one connection.
func Readers() map[string]PerpReader {
	return map[string]PerpReader{
		"binance": NewBinancePerp(),
		"bybit":   NewBybitPerp(),
		"okx":     NewOKXPerp(),
		"bitget":  NewBitgetPerp(),
		"mexc":    NewMEXCPerp(),
		"lighter": NewLighterPerp(),
	}
}
