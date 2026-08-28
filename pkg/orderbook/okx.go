package orderbook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OKX PERPETUAL SWAPS
//
// THE CONTRACT-SIZE TRAP, WHICH IS THE WHOLE REASON THIS FILE IS CAREFUL
//
// OKX quotes order-book size in CONTRACTS, not base units. BTC-USDT-SWAP has
// ctVal 0.01, so a level showing sz 517.93 is 5.1793 BTC -- about $326,000.
// Read as BTC it is $32.6 MILLION, a 100x overstatement, and every depth test
// in this project would pass on liquidity that does not exist.
//
// ctVal varies per instrument: 0.01 for BTC, 0.1 for ETH, 1 for many alts. So
// the error is not a constant that might be noticed -- it is a different
// multiple per symbol. An instrument whose ctVal cannot be read is REFUSED.
//
// This is the same shape as Bybit's marketUnit trap, where a spot market BUY
// treats qty as the quote amount and mis-sizes by the price of the asset.
//
// WHAT IS GOOD HERE
//
//   - funding-rate?instId=ANY returns EVERY instrument in one call, so OKX
//     costs one request per pass rather than 431.
//   - The funding interval is derivable per symbol from nextFundingTime minus
//     fundingTime, so it is measured rather than assumed -- unlike the
//     hardcoded 8 hours that cost this project a day on 2026-08-13.
//
// code != "0" is an error even when the HTTP status is 200, exactly like
// Bybit's retCode.

const okxBase = "https://www.okx.com"

// OKXFunding is one instrument's funding, with its interval measured.
type OKXFunding struct {
	InstID        string
	Rate          float64 // per its own interval, as published
	IntervalHours float64
	NextFundingMs int64
	FundingMs     int64
}

// BpsPerHour normalises. Valid only when IntervalHours > 0.
func (f OKXFunding) BpsPerHour() float64 {
	if f.IntervalHours <= 0 {
		return 0
	}
	return f.Rate * 10000 / f.IntervalHours
}

type okxInstrument struct {
	InstID   string
	CtVal    float64
	CtValCcy string
	Base     string
}

// OKXPerp reads OKX USDT-margined perpetual swaps.
type OKXPerp struct {
	HTTP    *httpDoer
	BaseURL string
	Depth   int

	mu        sync.RWMutex
	inst      map[string]okxInstrument // instId -> instrument
	byCoin    map[string]string        // BASE -> instId
	intervals map[string]float64
	fundings  map[string]OKXFunding
	loaded    time.Time
}

// httpDoer lets tests inject a client without importing net/http here twice.
type httpDoer = clientShim

func NewOKXPerp() *OKXPerp {
	return &OKXPerp{
		HTTP:      newShim(20 * time.Second),
		BaseURL:   okxBase,
		Depth:     20,
		inst:      map[string]okxInstrument{},
		byCoin:    map[string]string{},
		intervals: map[string]float64{},
		fundings:  map[string]OKXFunding{},
	}
}

func (o *OKXPerp) Venue() string { return "okx" }

func (o *OKXPerp) base() string {
	if o.BaseURL != "" {
		return o.BaseURL
	}
	return okxBase
}

type okxEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (e okxEnvelope) err(what string) error {
	if e.Code == "0" {
		return nil
	}
	return fmt.Errorf("okx: %s: code %s: %s", what, e.Code, e.Msg)
}

func (o *OKXPerp) get(ctx context.Context, path string, out *okxEnvelope) error {
	raw, err := o.HTTP.get(ctx, o.base()+path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("okx: decoding %s: %w", path, err)
	}
	return nil
}

// LoadSymbols fetches instruments AND the bulk funding table.
//
// Both in one call each. The funding table is what supplies the per-symbol
// interval, so instruments alone are not enough to trade against.
func (o *OKXPerp) LoadSymbols(ctx context.Context) error {
	var env okxEnvelope
	if err := o.get(ctx, "/api/v5/public/instruments?instType=SWAP", &env); err != nil {
		return err
	}
	if err := env.err("instruments"); err != nil {
		return err
	}

	var rows []struct {
		InstID    string `json:"instId"`
		State     string `json:"state"`
		SettleCcy string `json:"settleCcy"`
		CtVal     string `json:"ctVal"`
		CtValCcy  string `json:"ctValCcy"`
		CtType    string `json:"ctType"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		return fmt.Errorf("okx: decoding instruments: %w", err)
	}

	inst := map[string]okxInstrument{}
	byCoin := map[string]string{}
	var noCtVal int

	for _, r := range rows {
		if r.State != "live" || r.SettleCcy != "USDT" {
			continue
		}
		ctVal, ok := ParseNum(r.CtVal)
		if !ok || ctVal <= 0 {
			// Without ctVal every depth reading on this instrument is wrong by
			// an unknown multiple. Refuse it.
			noCtVal++
			continue
		}
		base := strings.SplitN(r.InstID, "-", 2)[0]
		inst[r.InstID] = okxInstrument{
			InstID: r.InstID, CtVal: ctVal, CtValCcy: r.CtValCcy, Base: base,
		}
		byCoin[strings.ToUpper(base)] = r.InstID
	}
	if len(inst) == 0 {
		return fmt.Errorf("okx: no live USDT swaps with a readable contract size")
	}

	// Bulk funding. instId=ANY returns every instrument at once.
	var fenv okxEnvelope
	if err := o.get(ctx, "/api/v5/public/funding-rate?instId=ANY", &fenv); err != nil {
		return fmt.Errorf("okx: funding: %w", err)
	}
	if err := fenv.err("funding-rate"); err != nil {
		return err
	}

	var frows []struct {
		InstID          string `json:"instId"`
		FundingRate     string `json:"fundingRate"`
		FundingTime     string `json:"fundingTime"`
		NextFundingTime string `json:"nextFundingTime"`
	}
	if err := json.Unmarshal(fenv.Data, &frows); err != nil {
		return fmt.Errorf("okx: decoding funding: %w", err)
	}

	intervals := map[string]float64{}
	fundings := map[string]OKXFunding{}
	var noInterval int

	for _, r := range frows {
		if _, known := inst[r.InstID]; !known {
			continue
		}
		rate, ok1 := ParseNum(r.FundingRate)
		ft, ok2 := ParseNum(r.FundingTime)
		nf, ok3 := ParseNum(r.NextFundingTime)
		if !ok1 || !ok2 || !ok3 || nf <= ft {
			noInterval++
			continue
		}
		// MEASURED, not assumed. The gap between this settlement and the next
		// IS the interval, per symbol, from the venue itself.
		hours := (nf - ft) / 3_600_000
		if hours <= 0 || hours > 24 {
			noInterval++
			continue
		}
		intervals[r.InstID] = hours
		fundings[r.InstID] = OKXFunding{
			InstID: r.InstID, Rate: rate, IntervalHours: hours,
			FundingMs: int64(ft), NextFundingMs: int64(nf),
		}
	}

	o.mu.Lock()
	o.inst, o.byCoin, o.intervals, o.fundings = inst, byCoin, intervals, fundings
	o.loaded = time.Now().UTC()
	o.mu.Unlock()
	return nil
}

func (o *OKXPerp) Symbols() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]string, 0, len(o.inst))
	for s := range o.inst {
		out = append(out, s)
	}
	return out
}

func (o *OKXPerp) SymbolCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.inst)
}

// ResolveCoin maps a Hyperliquid coin name to an OKX instId.
//
// OKX writes BTC-USDT-SWAP where Binance and Bybit write BTCUSDT, so nothing
// here can share the other venues' naming. The k-prefix convention is offered
// and only used if OKX confirms it.
func (o *OKXPerp) ResolveCoin(coin string) (string, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if len(o.byCoin) == 0 {
		return "", false
	}
	c := strings.ToUpper(strings.TrimSpace(coin))
	for _, cand := range []string{c, strings.TrimPrefix(c, "K"), "1000" + strings.TrimPrefix(c, "K")} {
		if id, ok := o.byCoin[cand]; ok {
			return id, true
		}
	}
	return "", false
}

func (o *OKXPerp) FundingIntervalHours(instID string) FundingInterval {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if h, ok := o.intervals[instID]; ok && h > 0 {
		// Derived from the venue's own settlement timestamps, so it is
		// explicit in the sense that matters: measured, not defaulted.
		return FundingInterval{Hours: h, Explicit: true, Ok: true}
	}
	return FundingInterval{}
}

// Funding returns the cached rate for an instrument.
func (o *OKXPerp) Funding(instID string) (OKXFunding, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	f, ok := o.fundings[instID]
	return f, ok
}

// Fundings returns every cached rate, keyed by instId.
func (o *OKXPerp) Fundings() map[string]OKXFunding {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]OKXFunding, len(o.fundings))
	for k, v := range o.fundings {
		out[k] = v
	}
	return out
}

// Book reads depth, converting CONTRACTS to base units.
//
// This conversion is the entire point of the file. sz is in contracts; base
// size is sz * ctVal. Skipping it overstates BTC depth by 100x and ETH by 10x.
func (o *OKXPerp) Book(ctx context.Context, instID string) (Book, error) {
	out := Book{Venue: "okx", Symbol: instID, Kind: "swap"}

	o.mu.RLock()
	in, known := o.inst[instID]
	o.mu.RUnlock()
	if !known {
		return out, fmt.Errorf("okx: %s is not a loaded instrument; its contract size is unknown "+
			"and every depth figure would be wrong by an unknown multiple", instID)
	}

	depth := o.Depth
	if depth <= 0 {
		depth = 20
	}
	var env okxEnvelope
	path := fmt.Sprintf("/api/v5/market/books?instId=%s&sz=%d", url.QueryEscape(instID), depth)
	if err := o.get(ctx, path, &env); err != nil {
		return out, err
	}
	if err := env.err("books " + instID); err != nil {
		return out, err
	}

	var books []struct {
		Asks [][]string `json:"asks"`
		Bids [][]string `json:"bids"`
		Ts   string     `json:"ts"`
	}
	if err := json.Unmarshal(env.Data, &books); err != nil {
		return out, fmt.Errorf("okx: decoding books %s: %w", instID, err)
	}
	if len(books) == 0 {
		return out, fmt.Errorf("okx: empty book response for %s", instID)
	}
	b := books[0]

	if ts, ok := ParseNum(b.Ts); ok && ts > 0 {
		out.At = time.UnixMilli(int64(ts)).UTC()
	}

	conv := func(rows [][]string) []Level {
		lv := make([]Level, 0, len(rows))
		for _, r := range rows {
			if len(r) < 2 {
				continue
			}
			px, ok1 := ParseNum(r[0])
			contracts, ok2 := ParseNum(r[1])
			if !ok1 || !ok2 || px <= 0 || contracts <= 0 {
				continue
			}
			// CONTRACTS -> BASE UNITS.
			lv = append(lv, Level{Px: px, Sz: contracts * in.CtVal})
		}
		return lv
	}
	out.Bids = conv(b.Bids)
	out.Asks = conv(b.Asks)
	out.Finalise()
	return out, nil
}

// ContractValue exposes ctVal for a loaded instrument, for tests and audits.
func (o *OKXPerp) ContractValue(instID string) (float64, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	in, ok := o.inst[instID]
	if !ok {
		return 0, false
	}
	return in.CtVal, true
}
