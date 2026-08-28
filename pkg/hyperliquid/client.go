// Package hyperliquid reads Hyperliquid's public market data.
//
// WHY THIS EXISTS SEPARATELY FROM pkg/binance
//
// Hyperliquid is the venue that generates the cross-venue signal. Binance and
// Bybit both settle funding every EIGHT hours, so their rates are slow and
// track each other closely -- measured 2026-08-11, BTC funding was 0.007451 on
// Binance against 0.005732 on Bybit, a spread of 0.0017%, which does not pay
// for one leg of a round trip. Hyperliquid settles EVERY HOUR. It reprices 24
// times a day and moves far enough to be worth acting on.
//
// Every dispersion row logged so far has Hyperliquid on one side. Without it
// there is essentially no cross-venue trade.
//
// # WHAT THIS PACKAGE REFUSES TO DO
//
// cmd/dispersion currently uses the venue's own impactSpreadBps as a proxy for
// depth. That is a number Hyperliquid computes, not a book anyone has read. It
// is good enough to RANK candidates and not good enough to SIZE a position.
//
// So the important thing here is SweepCost: walk the actual resting orders and
// work out what a given USD notional would pay against the mid. That is a
// measurement. Everything downstream that sizes a position must use it, and an
// unread book must produce a refusal rather than a default.
//
// # AUTHENTICATION
//
// None. Every endpoint here is public and unauthenticated -- one POST to /info
// with a type field. No key exists for this package to leak.
//
// # THE DEX RISK THIS PACKAGE DOES NOT PRICE
//
// Hyperliquid holds collateral in a contract. There is no support desk and no
// chargeback. Nothing in this code models that, and no amount of measured book
// depth substitutes for it.
package hyperliquid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/imtiyaz/vega-bot/pkg/orderbook"
	"time"
)

// InfoURL is the public info endpoint. Everything here POSTs to it.
const InfoURL = "https://api.hyperliquid.xyz/info"

// FundingIntervalHours is Hyperliquid's settlement interval.
//
// This constant is the single most dangerous number in the cross-venue work.
// Hyperliquid quotes a per-HOUR rate; Binance and Bybit quote per-8h. Comparing
// them raw is an 8x error, and it flatters whichever side you were hoping for.
// It is a named constant so that every conversion points back to this comment.
const FundingIntervalHours = 1.0

// Client reads Hyperliquid's public info endpoint.
type Client struct {
	HTTP *http.Client
	URL  string
}

// New returns a client with a timeout short enough that a hung venue cannot
// stall a poll loop.
func New() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 15 * time.Second},
		URL:  InfoURL,
	}
}

func (c *Client) post(ctx context.Context, body any, out any) error {
	url := c.URL
	if url == "" {
		url = InfoURL
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("hyperliquid: encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("hyperliquid: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("hyperliquid: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("hyperliquid: reading body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := raw
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("hyperliquid: HTTP %d: %s", resp.StatusCode, snippet)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hyperliquid: decoding response: %w", err)
	}
	return nil
}

// num parses one of Hyperliquid's string-encoded numbers.
//
// Returning ok=false rather than 0 is deliberate. Zero is a legitimate value
// for a funding rate and a catastrophic one for a price or a size, and a
// silently-zeroed price would size a position against nothing.
func num(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// --- meta and asset contexts -------------------------------------------------

// Asset is one perp's static definition plus its live context.
type Asset struct {
	Coin         string
	SzDecimals   int
	MaxLeverage  int
	OnlyIsolated bool
	Delisted     bool

	MarkPx   float64
	OraclePx float64
	MidPx    float64

	// FundingHourly is the rate for ONE HOUR, as a fraction (not bps, not
	// percent). Hyperliquid quotes it this way natively.
	FundingHourly float64

	OpenInterest float64 // in base units
	DayNtlVlmUSD float64

	// ImpactSpreadBps is the venue's own depth proxy: the gap between its
	// impact bid and ask. Useful for RANKING. Never use it to size -- read the
	// book with L2Book and call SweepCost.
	ImpactSpreadBps float64
	ImpactBid       float64
	ImpactAsk       float64

	// PricesOk is false if any price field failed to parse. A caller must
	// refuse such an asset rather than work with partial numbers.
	PricesOk bool
}

// FundingBpsPerHour converts to the unit every comparison in VEGA uses.
func (a Asset) FundingBpsPerHour() float64 { return a.FundingHourly * 10000 }

// FundingBpsPerDay is what a position earns or pays in a day at this rate.
func (a Asset) FundingBpsPerDay() float64 {
	return a.FundingBpsPerHour() * (24 / FundingIntervalHours)
}

// OpenInterestUSD values the open interest at the mark.
func (a Asset) OpenInterestUSD() float64 { return a.OpenInterest * a.MarkPx }

type metaUniverse struct {
	Universe []struct {
		Name         string `json:"name"`
		SzDecimals   int    `json:"szDecimals"`
		MaxLeverage  int    `json:"maxLeverage"`
		OnlyIsolated bool   `json:"onlyIsolated"`
		IsDelisted   bool   `json:"isDelisted"`
	} `json:"universe"`
}

type assetCtx struct {
	Funding      string   `json:"funding"`
	OpenInterest string   `json:"openInterest"`
	PrevDayPx    string   `json:"prevDayPx"`
	DayNtlVlm    string   `json:"dayNtlVlm"`
	Premium      *string  `json:"premium"`
	OraclePx     string   `json:"oraclePx"`
	MarkPx       string   `json:"markPx"`
	MidPx        *string  `json:"midPx"`
	ImpactPxs    []string `json:"impactPxs"`
}

// Assets returns every perp with its live context, keyed by coin.
//
// Hyperliquid returns this as a two-element array: [meta, contexts], positional
// and parallel. A length mismatch between the two is treated as a hard error --
// if the arrays have drifted, every coin's context belongs to a different coin,
// and that is the kind of bug that reads as a profitable strategy.
func (c *Client) Assets(ctx context.Context) (map[string]Asset, error) {
	var raw []json.RawMessage
	err := c.post(ctx, map[string]string{"type": "metaAndAssetCtxs"}, &raw)
	if err != nil {
		return nil, err
	}
	if len(raw) != 2 {
		return nil, fmt.Errorf("hyperliquid: metaAndAssetCtxs returned %d elements, want 2", len(raw))
	}

	var meta metaUniverse
	if err := json.Unmarshal(raw[0], &meta); err != nil {
		return nil, fmt.Errorf("hyperliquid: decoding universe: %w", err)
	}
	var ctxs []assetCtx
	if err := json.Unmarshal(raw[1], &ctxs); err != nil {
		return nil, fmt.Errorf("hyperliquid: decoding contexts: %w", err)
	}
	if len(meta.Universe) != len(ctxs) {
		return nil, fmt.Errorf(
			"hyperliquid: universe has %d entries but contexts has %d; "+
				"the arrays are positional so every coin would be paired with the wrong context",
			len(meta.Universe), len(ctxs))
	}

	out := make(map[string]Asset, len(ctxs))
	for i, u := range meta.Universe {
		k := ctxs[i]
		a := Asset{
			Coin:         u.Name,
			SzDecimals:   u.SzDecimals,
			MaxLeverage:  u.MaxLeverage,
			OnlyIsolated: u.OnlyIsolated,
			Delisted:     u.IsDelisted,
			PricesOk:     true,
		}

		if v, ok := num(k.MarkPx); ok {
			a.MarkPx = v
		} else {
			a.PricesOk = false
		}
		if v, ok := num(k.OraclePx); ok {
			a.OraclePx = v
		}
		if k.MidPx != nil {
			if v, ok := num(*k.MidPx); ok {
				a.MidPx = v
			}
		}
		if a.MidPx == 0 {
			a.MidPx = a.MarkPx
		}
		if v, ok := num(k.Funding); ok {
			a.FundingHourly = v
		} else {
			a.PricesOk = false
		}
		if v, ok := num(k.OpenInterest); ok {
			a.OpenInterest = v
		}
		if v, ok := num(k.DayNtlVlm); ok {
			a.DayNtlVlmUSD = v
		}

		// impactPxs is [bid, ask]. Its width is the venue's depth proxy.
		if len(k.ImpactPxs) == 2 {
			bid, okB := num(k.ImpactPxs[0])
			ask, okA := num(k.ImpactPxs[1])
			if okB && okA && bid > 0 && ask > 0 {
				a.ImpactBid, a.ImpactAsk = bid, ask
				mid := (bid + ask) / 2
				if mid > 0 {
					a.ImpactSpreadBps = (ask - bid) / mid * 10000
				}
			}
		}

		out[u.Name] = a
	}
	return out, nil
}

// --- predicted fundings ------------------------------------------------------

// venueLabel maps Hyperliquid's short venue names to readable ones.
//
// This is the ONLY venue-wide table left in this file. The interval table that
// used to sit beside it was deleted on 2026-08-12: it hardcoded Binance and
// Bybit at 8 hours when both publish the interval PER SYMBOL, and KAITOUSDT
// settles every 4. Intervals now come from
// orderbook.PerpReader.FundingIntervalHours. Nothing may reintroduce a
// venue-wide interval constant here.
var venueLabel = map[string]string{
	"HlPerp":    "hyperliquid",
	"BinPerp":   "binance",
	"BybitPerp": "bybit",
}

// VenueRate is one venue's funding rate for one coin.
//
// IntervalHours is ZERO for anything other than Hyperliquid, and BpsPerHour is
// meaningless until it is filled in.
//
// This used to carry a hardcoded 8 for Binance and Bybit. Binance sets the
// interval PER SYMBOL -- KAITOUSDT settles every 4 hours -- so that constant
// made every 4-hour pair read 4.4x too rich, and two positions closed at a
// loss on 2026-08-12 partly because of it. The caller must now resolve the
// interval from the venue's own instrument data and call WithInterval.
type VenueRate struct {
	Venue string

	// RawRate is exactly what the venue quotes, per ITS OWN interval,
	// untouched. Everything else here is derived from it.
	RawRate float64

	// IntervalHours is 0 when unknown. Never guess it.
	IntervalHours float64

	// BpsPerHour is valid only when IntervalHours > 0.
	BpsPerHour float64

	NextFundingMs int64
}

// Known reports whether this rate can be compared with another.
func (r VenueRate) Known() bool { return r.IntervalHours > 0 }

// WithInterval returns a copy normalised to the given settlement interval.
func (r VenueRate) WithInterval(hours float64) VenueRate {
	if hours <= 0 {
		r.IntervalHours, r.BpsPerHour = 0, 0
		return r
	}
	r.IntervalHours = hours
	r.BpsPerHour = r.RawRate * 10000 / hours
	return r
}

// PredictedFundings returns each venue's RAW funding rate per coin.
//
// One public call covers Binance, Bybit and Hyperliquid, which is what makes
// the cross-venue scan cheap.
//
// ONLY HYPERLIQUID'S RATE ARRIVES NORMALISED. Hyperliquid settles hourly for
// every asset, so its interval is a property of the venue. Binance and Bybit
// set it per symbol, so their rates come back with IntervalHours 0 and MUST be
// passed through WithInterval before being compared with anything.
func (c *Client) PredictedFundings(ctx context.Context) (rates map[string]map[string]VenueRate, unknownVenues int, err error) {
	var raw []json.RawMessage
	if err := c.post(ctx, map[string]string{"type": "predictedFundings"}, &raw); err != nil {
		return nil, 0, err
	}

	rates = make(map[string]map[string]VenueRate, len(raw))

	for _, entry := range raw {
		// Each entry is [coin, [[venueName, {fundingRate, nextFundingTime}], ...]]
		var pair []json.RawMessage
		if json.Unmarshal(entry, &pair) != nil || len(pair) != 2 {
			continue
		}
		var coin string
		if json.Unmarshal(pair[0], &coin) != nil || coin == "" {
			continue
		}

		var venues [][]json.RawMessage
		if json.Unmarshal(pair[1], &venues) != nil {
			continue
		}

		for _, v := range venues {
			if len(v) != 2 {
				continue
			}
			var name string
			if json.Unmarshal(v[0], &name) != nil {
				continue
			}
			label, known := venueLabel[name]
			if !known {
				unknownVenues++
				continue
			}

			var detail struct {
				FundingRate     string `json:"fundingRate"`
				NextFundingTime int64  `json:"nextFundingTime"`
			}
			if json.Unmarshal(v[1], &detail) != nil {
				continue
			}
			rate, ok := num(detail.FundingRate)
			if !ok {
				continue
			}

			if rates[coin] == nil {
				rates[coin] = map[string]VenueRate{}
			}
			r := VenueRate{
				Venue:         label,
				RawRate:       rate,
				NextFundingMs: detail.NextFundingTime,
			}
			// Hyperliquid alone has a venue-wide interval.
			if label == "hyperliquid" {
				r = r.WithInterval(FundingIntervalHours)
			}
			rates[coin][label] = r
		}
	}
	return rates, unknownVenues, nil
}

// --- the order book ----------------------------------------------------------

// Book, Level and Sweep live in pkg/orderbook so that Binance and Bybit perp
// depth is measured with the SAME arithmetic as Hyperliquid's.
//
// A cross-venue cost is the sum of what each leg pays to cross its own book. If
// those came from two different sweep implementations the total would not be a
// cost, it would be two incomparable numbers added together.
//
// These aliases keep every existing caller compiling unchanged.
type (
	Level = orderbook.Level
	Book  = orderbook.Book
	Sweep = orderbook.Sweep
)

type l2Response struct {
	Coin   string `json:"coin"`
	Time   int64  `json:"time"`
	Levels [][]struct {
		Px string `json:"px"`
		Sz string `json:"sz"`
		N  int    `json:"n"`
	} `json:"levels"`
}

// L2Book reads the resting orders for one coin.
//
// Hyperliquid returns levels as a two-element array: [bids, asks]. Anything
// else is a hard error rather than a partial book -- half a book prices a
// one-way trade, and there is no such thing here.
func (c *Client) L2Book(ctx context.Context, coin string) (orderbook.Book, error) {
	var r l2Response
	b := orderbook.Book{Venue: "hyperliquid", Symbol: coin}

	if err := c.post(ctx, map[string]string{"type": "l2Book", "coin": coin}, &r); err != nil {
		return b, err
	}
	if r.Time > 0 {
		b.At = time.UnixMilli(r.Time).UTC()
	}
	if len(r.Levels) != 2 {
		return b, fmt.Errorf("hyperliquid: l2Book for %s returned %d sides, want 2", coin, len(r.Levels))
	}

	for side, raw := range r.Levels {
		for _, lv := range raw {
			px, okP := orderbook.ParseNum(lv.Px)
			sz, okS := orderbook.ParseNum(lv.Sz)
			if !okP || !okS || px <= 0 || sz <= 0 {
				continue
			}
			l := orderbook.Level{Px: px, Sz: sz, N: lv.N}
			if side == 0 {
				b.Bids = append(b.Bids, l)
			} else {
				b.Asks = append(b.Asks, l)
			}
		}
	}

	b.Finalise()
	return b, nil
}
