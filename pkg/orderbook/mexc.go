package orderbook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MEXC USDT-MARGINED PERPETUAL FUTURES
//
// # THE CONTRACT TRAP, WORSE HERE THAN ANYWHERE ELSE
//
// Probed 2026-08-17: BTC_USDT has contractSize 0.0001 and its book showed a
// level of [64337, 19, 1]. That is 19 CONTRACTS = 0.0019 BTC = about $122.
// Read as base units it is 19 BTC = $1.2 MILLION -- a 10,000x overstatement,
// two orders of magnitude worse than OKX's 100x. An instrument whose
// contractSize cannot be read is REFUSED.
//
// # INTERVALS VARY AND MUST BE FETCHED PER SYMBOL
//
// collectCycle is 8 for BTC_USDT and 4 for KAITO_USDT. The bulk ticker carries
// fundingRate for all 1,124 symbols but NOT the cycle, so a rate from the
// ticker alone cannot be normalised to bps/hour. Interval and next settlement
// both come from funding_rate/{symbol}, one call each, cached.
//
// A rate whose interval is unknown is not returned at all. Guessing 8 hours on
// a 4-hour symbol reads the spread twice as rich as it is, which is how two
// positions stopped out at -$7.49 on 2026-08-13.
//
// code != 0 is an error even on HTTP 200.
const mexcBase = "https://contract.mexc.com"

// MEXCFunding is one symbol's rate with its interval and calendar.
type MEXCFunding struct {
	Symbol        string
	Rate          float64
	IntervalHours float64
	NextFundingMs int64
}

func (f MEXCFunding) BpsPerHour() float64 {
	if f.IntervalHours <= 0 {
		return 0
	}
	return f.Rate * 10000 / f.IntervalHours
}

type mexcInstrument struct {
	Symbol       string
	Base         string
	ContractSize float64
	MinVol       float64
	TakerBps     float64
}

// MEXCPerp reads MEXC USDT-margined perpetual futures.
type MEXCPerp struct {
	HTTP    *httpDoer
	BaseURL string
	Depth   int

	mu          sync.RWMutex
	inst        map[string]mexcInstrument
	byCoin      map[string]string
	rawRate     map[string]float64 // from the bulk ticker
	intervals   map[string]float64 // hydrated per symbol
	nextFunding map[string]int64   // hydrated per symbol
	loaded      time.Time
}

func NewMEXCPerp() *MEXCPerp {
	return &MEXCPerp{
		HTTP:        newShim(20 * time.Second),
		BaseURL:     mexcBase,
		Depth:       20,
		inst:        map[string]mexcInstrument{},
		byCoin:      map[string]string{},
		rawRate:     map[string]float64{},
		intervals:   map[string]float64{},
		nextFunding: map[string]int64{},
	}
}

func (m *MEXCPerp) Venue() string { return "mexc" }

func (m *MEXCPerp) base() string {
	if m.BaseURL != "" {
		return m.BaseURL
	}
	return mexcBase
}

type mexcEnvelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
}

func (e mexcEnvelope) err(what string) error {
	if e.Code == 0 {
		return nil
	}
	return fmt.Errorf("mexc: %s: code %d", what, e.Code)
}

func (m *MEXCPerp) get(ctx context.Context, path string, out *mexcEnvelope) error {
	raw, err := m.HTTP.get(ctx, m.base()+path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mexc: decoding %s: %w", path, err)
	}
	return nil
}

// LoadSymbols fetches instruments and the bulk rate table.
//
// This alone does NOT produce usable rates -- the interval is missing and comes
// from EnsureFundingMeta. That is deliberate: a rate without its interval is
// not a rate.
func (m *MEXCPerp) LoadSymbols(ctx context.Context) error {
	var env mexcEnvelope
	if err := m.get(ctx, "/api/v1/contract/detail", &env); err != nil {
		return err
	}
	if err := env.err("contract/detail"); err != nil {
		return err
	}
	var rows []struct {
		Symbol       string  `json:"symbol"`
		SettleCoin   string  `json:"settleCoin"`
		BaseCoin     string  `json:"baseCoin"`
		ContractSize float64 `json:"contractSize"`
		MinVol       float64 `json:"minVol"`
		TakerFeeRate float64 `json:"takerFeeRate"`
		State        int     `json:"state"`
		APIAllowed   bool    `json:"apiAllowed"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		return fmt.Errorf("mexc: decoding contract/detail: %w", err)
	}
	inst := map[string]mexcInstrument{}
	byCoin := map[string]string{}
	noSize := 0
	for _, r := range rows {
		// state 0 is enabled. apiAllowed false means it cannot be traded
		// programmatically, so measuring it would be measuring a fiction.
		if r.State != 0 || !r.APIAllowed || r.SettleCoin != "USDT" {
			continue
		}
		if r.ContractSize <= 0 {
			noSize++
			continue
		}
		bse := r.BaseCoin
		if bse == "" {
			bse = strings.SplitN(r.Symbol, "_", 2)[0]
		}
		inst[r.Symbol] = mexcInstrument{
			Symbol: r.Symbol, Base: bse,
			ContractSize: r.ContractSize, MinVol: r.MinVol,
			TakerBps: r.TakerFeeRate * 10000,
		}
		byCoin[strings.ToUpper(bse)] = r.Symbol
	}
	if len(inst) == 0 {
		return fmt.Errorf("mexc: no tradeable USDT contracts with a readable contract size (%d rejected)", noSize)
	}

	var tenv mexcEnvelope
	if err := m.get(ctx, "/api/v1/contract/ticker", &tenv); err != nil {
		return fmt.Errorf("mexc: ticker: %w", err)
	}
	if err := tenv.err("ticker"); err != nil {
		return err
	}
	var trows []struct {
		Symbol      string  `json:"symbol"`
		FundingRate float64 `json:"fundingRate"`
	}
	if err := json.Unmarshal(tenv.Data, &trows); err != nil {
		return fmt.Errorf("mexc: decoding ticker: %w", err)
	}
	raw := map[string]float64{}
	for _, r := range trows {
		if _, known := inst[r.Symbol]; known {
			raw[r.Symbol] = r.FundingRate
		}
	}

	m.mu.Lock()
	m.inst, m.byCoin, m.rawRate = inst, byCoin, raw
	m.loaded = time.Now().UTC()
	m.mu.Unlock()
	return nil
}

// EnsureFundingMeta hydrates collectCycle and nextSettleTime per symbol.
//
// The interval is cached for the process lifetime -- venues do change a
// symbol's cycle, but not within a single run, and re-reading it every pass
// would cost 1,124 requests. The next settlement is re-fetched once it passes.
func (m *MEXCPerp) EnsureFundingMeta(ctx context.Context, symbols []string, pace time.Duration, max int) (fetched int) {
	nowMs := time.Now().UnixMilli()
	var need []string
	m.mu.RLock()
	for _, s := range symbols {
		if _, known := m.inst[s]; !known {
			continue
		}
		if m.intervals[s] <= 0 || m.nextFunding[s] <= nowMs {
			need = append(need, s)
		}
	}
	m.mu.RUnlock()
	if len(need) == 0 {
		return 0
	}
	if max > 0 && len(need) > max {
		need = need[:max]
	}
	if pace <= 0 {
		pace = 120 * time.Millisecond
	}
	type meta struct {
		ivl  float64
		next int64
	}
	got := map[string]meta{}
	// BOUNDED BY TIME, NOT BY REQUEST COUNT.
	//
	// A cap of 200 calls is only a 24-second cap if every call returns in
	// 120ms. When the venue throttles, each one can sit on the 20s HTTP
	// timeout instead -- 200 of those is over an hour, and on 2026-08-17 that
	// stalled a whole pass of the union book after the MEXC merge went in.
	// Latency is not ours to control; elapsed time is.
	budget := 25 * time.Second
	started := time.Now()
	for i, s := range need {
		if ctx.Err() != nil || time.Since(started) > budget {
			break
		}
		var env mexcEnvelope
		if err := m.get(ctx, "/api/v1/contract/funding_rate/"+s, &env); err != nil || env.err("funding_rate") != nil {
			continue
		}
		var d struct {
			CollectCycle   float64 `json:"collectCycle"`
			NextSettleTime int64   `json:"nextSettleTime"`
			FundingRate    float64 `json:"fundingRate"`
		}
		if json.Unmarshal(env.Data, &d) != nil || d.CollectCycle <= 0 || d.CollectCycle > 24 || d.NextSettleTime <= 0 {
			continue
		}
		got[s] = meta{d.CollectCycle, d.NextSettleTime}
		if i < len(need)-1 {
			time.Sleep(pace)
		}
	}
	m.mu.Lock()
	for k, v := range got {
		m.intervals[k], m.nextFunding[k] = v.ivl, v.next
	}
	m.mu.Unlock()
	return len(got)
}

func (m *MEXCPerp) Symbols() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.inst))
	for s := range m.inst {
		out = append(out, s)
	}
	return out
}

func (m *MEXCPerp) SymbolCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.inst)
}

// ResolveCoin maps a coin name to a MEXC symbol. MEXC writes BTC_USDT.
func (m *MEXCPerp) ResolveCoin(coin string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.byCoin) == 0 {
		return "", false
	}
	c := strings.ToUpper(strings.TrimSpace(coin))
	for _, cand := range []string{c, strings.TrimPrefix(c, "K"), "1000" + strings.TrimPrefix(c, "K")} {
		if s, ok := m.byCoin[cand]; ok {
			return s, true
		}
	}
	return "", false
}

func (m *MEXCPerp) FundingIntervalHours(symbol string) FundingInterval {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if h, ok := m.intervals[symbol]; ok && h > 0 {
		return FundingInterval{Hours: h, Explicit: true, Ok: true}
	}
	return FundingInterval{}
}

// Fundings returns only symbols whose interval AND calendar are known.
func (m *MEXCPerp) Fundings() map[string]MEXCFunding {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]MEXCFunding, len(m.rawRate))
	for sym, r := range m.rawRate {
		ivl, next := m.intervals[sym], m.nextFunding[sym]
		if ivl <= 0 || next <= 0 {
			continue
		}
		out[sym] = MEXCFunding{Symbol: sym, Rate: r, IntervalHours: ivl, NextFundingMs: next}
	}
	return out
}

// TakerBps is this symbol's OWN taker fee, read from the venue.
//
// MEXC does not have a venue-wide futures taker rate. Probed 2026-08-17 across
// 1,116 live contracts: 540 charge ZERO, 472 charge 2 bps, 66 charge 4, and 25
// charge 10. A single venue-level number would be wrong for almost every
// symbol -- too high on half the book, too low on the tail. So it is read per
// symbol, exactly like contractSize and collectCycle.
//
// ok is false for an unknown symbol. A fee that cannot be read is not assumed.
func (m *MEXCPerp) TakerBps(symbol string) (float64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	in, ok := m.inst[symbol]
	if !ok {
		return 0, false
	}
	return in.TakerBps, true
}

// ContractValue exposes contractSize for audits.
func (m *MEXCPerp) ContractValue(symbol string) (float64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	in, ok := m.inst[symbol]
	if !ok {
		return 0, false
	}
	return in.ContractSize, true
}

// Book reads depth, converting CONTRACTS to base units.
//
// Skipping this multiplies BTC depth by 10,000.
func (m *MEXCPerp) Book(ctx context.Context, symbol string) (Book, error) {
	out := Book{Venue: "mexc", Symbol: symbol, Kind: "swap"}
	m.mu.RLock()
	in, known := m.inst[symbol]
	m.mu.RUnlock()
	if !known {
		return out, fmt.Errorf("mexc: %s is not a loaded instrument; its contract size is unknown "+
			"and every depth figure would be wrong by an unknown multiple", symbol)
	}
	var env mexcEnvelope
	if err := m.get(ctx, "/api/v1/contract/depth/"+symbol, &env); err != nil {
		return out, err
	}
	if err := env.err("depth " + symbol); err != nil {
		return out, err
	}
	var d struct {
		Asks [][]float64 `json:"asks"`
		Bids [][]float64 `json:"bids"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return out, fmt.Errorf("mexc: decoding depth %s: %w", symbol, err)
	}
	conv := func(rows [][]float64) []Level {
		lv := make([]Level, 0, len(rows))
		for _, r := range rows {
			if len(r) < 2 || r[0] <= 0 || r[1] <= 0 {
				continue
			}
			// CONTRACTS -> BASE UNITS.
			lv = append(lv, Level{Px: r[0], Sz: r[1] * in.ContractSize})
		}
		return lv
	}
	out.Bids = conv(d.Bids)
	out.Asks = conv(d.Asks)
	out.At = time.Now().UTC()
	out.Finalise()
	return out, nil
}
