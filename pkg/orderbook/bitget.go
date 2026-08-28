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

// BITGET USDT-MARGINED PERPETUAL FUTURES
//
// WHAT THE LIVE PROBES ESTABLISHED ON 2026-08-17, rather than what memory
// suggested:
//
//   - Order-book size is in BASE UNITS, not contracts. The BTCUSDT book carried
//     levels of 0.0002 and 0.0001, and a contract COUNT cannot be fractional.
//     So no ctVal conversion here -- unlike OKX, where omitting it overstates
//     BTC depth by 100x.
//
//   - fundInterval is published PER SYMBOL and genuinely varies: BTCUSDT 8,
//     KAITOUSDT 4. That difference is the exact shape of the bug that stopped
//     two positions out at -$7.49 on 2026-08-13, when 8 hours was assumed
//     venue-wide and a 4h symbol therefore read twice as rich as it was.
//
//   - minTradeUSDT is published per symbol, so the minimum order value is READ
//     rather than guessed.
//
// COST: two HTTP calls per pass for all 754 symbols. Cheaper than Binance.
//
// code != "00000" is an error even on HTTP 200, like Bybit's retCode.
const bitgetBase = "https://api.bitget.com"

// BitgetFunding is one symbol's funding with the interval it belongs to.
type BitgetFunding struct {
	Symbol        string
	Rate          float64 // per its own interval, as published
	IntervalHours float64
	NextFundingMs int64 // 0 means UNKNOWN, and an unknown calendar is refused
}

// BpsPerHour normalises. Meaningless without the interval, so it refuses.
func (f BitgetFunding) BpsPerHour() float64 {
	if f.IntervalHours <= 0 {
		return 0
	}
	return f.Rate * 10000 / f.IntervalHours
}

type bitgetInstrument struct {
	Symbol       string
	Base         string
	IntervalH    float64
	MinTradeUSDT float64
}

// BitgetPerp reads Bitget USDT-margined perpetual futures.
type BitgetPerp struct {
	HTTP    *httpDoer
	BaseURL string
	Depth   int

	mu          sync.RWMutex
	inst        map[string]bitgetInstrument
	byCoin      map[string]string
	fundings    map[string]BitgetFunding
	nextFunding map[string]int64
	loaded      time.Time
}

func NewBitgetPerp() *BitgetPerp {
	return &BitgetPerp{
		HTTP:        newShim(20 * time.Second),
		BaseURL:     bitgetBase,
		Depth:       15,
		inst:        map[string]bitgetInstrument{},
		byCoin:      map[string]string{},
		fundings:    map[string]BitgetFunding{},
		nextFunding: map[string]int64{},
	}
}

func (b *BitgetPerp) Venue() string { return "bitget" }

func (b *BitgetPerp) base() string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	return bitgetBase
}

type bitgetEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (e bitgetEnvelope) err(what string) error {
	if e.Code == "00000" {
		return nil
	}
	return fmt.Errorf("bitget: %s: code %s: %s", what, e.Code, e.Msg)
}

func (b *BitgetPerp) get(ctx context.Context, path string, out *bitgetEnvelope) error {
	raw, err := b.HTTP.get(ctx, b.base()+path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("bitget: decoding %s: %w", path, err)
	}
	return nil
}

// LoadSymbols fetches the instrument table and every funding rate.
//
// A symbol whose fundInterval cannot be read is REFUSED, not defaulted. A
// wrong interval does not look broken -- it looks profitable.
func (b *BitgetPerp) LoadSymbols(ctx context.Context) error {
	var env bitgetEnvelope
	if err := b.get(ctx, "/api/v2/mix/market/contracts?productType=USDT-FUTURES", &env); err != nil {
		return err
	}
	if err := env.err("contracts"); err != nil {
		return err
	}
	var rows []struct {
		Symbol       string `json:"symbol"`
		SymbolStatus string `json:"symbolStatus"`
		FundInterval string `json:"fundInterval"`
		MinTradeUSDT string `json:"minTradeUSDT"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		return fmt.Errorf("bitget: decoding contracts: %w", err)
	}

	inst := map[string]bitgetInstrument{}
	byCoin := map[string]string{}
	noInterval := 0
	for _, r := range rows {
		if r.SymbolStatus != "normal" || !strings.HasSuffix(r.Symbol, "USDT") {
			continue
		}
		h, ok := ParseNum(r.FundInterval)
		if !ok || h <= 0 || h > 24 {
			noInterval++
			continue
		}
		minUSD, _ := ParseNum(r.MinTradeUSDT)
		bse := strings.TrimSuffix(r.Symbol, "USDT")
		inst[r.Symbol] = bitgetInstrument{
			Symbol: r.Symbol, Base: bse, IntervalH: h, MinTradeUSDT: minUSD,
		}
		byCoin[strings.ToUpper(bse)] = r.Symbol
	}
	if len(inst) == 0 {
		return fmt.Errorf("bitget: no normal USDT futures with a readable funding interval "+
			"(%d rejected for an unreadable fundInterval)", noInterval)
	}

	var tenv bitgetEnvelope
	if err := b.get(ctx, "/api/v2/mix/market/tickers?productType=USDT-FUTURES", &tenv); err != nil {
		return fmt.Errorf("bitget: tickers: %w", err)
	}
	if err := tenv.err("tickers"); err != nil {
		return err
	}
	var trows []struct {
		Symbol      string `json:"symbol"`
		FundingRate string `json:"fundingRate"`
	}
	if err := json.Unmarshal(tenv.Data, &trows); err != nil {
		return fmt.Errorf("bitget: decoding tickers: %w", err)
	}
	fundings := map[string]BitgetFunding{}
	for _, r := range trows {
		in, known := inst[r.Symbol]
		if !known {
			continue
		}
		rate, ok := ParseNum(r.FundingRate)
		if !ok {
			continue
		}
		fundings[r.Symbol] = BitgetFunding{
			Symbol: r.Symbol, Rate: rate, IntervalHours: in.IntervalH,
		}
	}

	b.mu.Lock()
	b.inst, b.byCoin, b.fundings = inst, byCoin, fundings
	b.loaded = time.Now().UTC()
	b.mu.Unlock()
	return nil
}

func (b *BitgetPerp) Symbols() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.inst))
	for s := range b.inst {
		out = append(out, s)
	}
	return out
}

func (b *BitgetPerp) SymbolCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.inst)
}

// ResolveCoin maps a Hyperliquid coin name to a Bitget symbol. The k-prefix
// and 1000-prefix conventions are offered as candidates and used only if
// Bitget's own instrument list confirms them.
func (b *BitgetPerp) ResolveCoin(coin string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.byCoin) == 0 {
		return "", false
	}
	c := strings.ToUpper(strings.TrimSpace(coin))
	for _, cand := range []string{c, strings.TrimPrefix(c, "K"), "1000" + strings.TrimPrefix(c, "K")} {
		if s, ok := b.byCoin[cand]; ok {
			return s, true
		}
	}
	return "", false
}

// FundingIntervalHours is Explicit because Bitget publishes it per symbol.
func (b *BitgetPerp) FundingIntervalHours(symbol string) FundingInterval {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if in, ok := b.inst[symbol]; ok && in.IntervalH > 0 {
		return FundingInterval{Hours: in.IntervalH, Explicit: true, Ok: true}
	}
	return FundingInterval{}
}

func (b *BitgetPerp) Funding(symbol string) (BitgetFunding, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	f, ok := b.fundings[symbol]
	if ok {
		f.NextFundingMs = b.nextFunding[symbol]
	}
	return f, ok
}

// EnsureFundingTimes hydrates next-settlement times for the given symbols.
//
// # WHY THIS IS ONE CALL PER SYMBOL, WHICH IS UGLY
//
// Probed 2026-08-17: the tickers endpoint carries no funding timestamp of any
// kind, and funding-time refuses a request without a symbol (code 400172). So
// there is no bulk source. The alternative -- inferring settlement times from
// UTC alignment because 8h venues "usually" settle at 00:00/08:00/16:00 -- is
// the same class of assumption as the hardcoded 8-hour interval that cost this
// project $7.49, and is rejected for the same reason.
//
// It is cheap in practice. A next-funding time stays valid until it passes, so
// each symbol needs one call per interval, not one per poll. max bounds the
// work per pass so the first warm-up spreads over several passes instead of
// stalling one.
//
// ratePeriod is cross-checked against fundInterval. If the two endpoints
// disagree about the same symbol, one of them is wrong and the symbol is left
// unhydrated rather than trusted.
func (b *BitgetPerp) EnsureFundingTimes(ctx context.Context, symbols []string, pace time.Duration, max int) (fetched, mismatched int) {
	nowMs := time.Now().UnixMilli()
	var need []string
	b.mu.RLock()
	for _, sym := range symbols {
		if _, known := b.inst[sym]; !known {
			continue
		}
		if t, ok := b.nextFunding[sym]; !ok || t <= nowMs {
			need = append(need, sym)
		}
	}
	b.mu.RUnlock()
	if len(need) == 0 {
		return 0, 0
	}
	if max > 0 && len(need) > max {
		need = need[:max]
	}
	if pace <= 0 {
		pace = 130 * time.Millisecond
	}
	got := map[string]int64{}
	// BOUNDED BY TIME, NOT BY REQUEST COUNT. A cap of 400 calls is only a
	// 24-second cap if every call returns in 60ms; when the venue throttles,
	// each can sit on the 20s HTTP timeout instead. Latency is not ours to
	// control, elapsed time is.
	budget := 25 * time.Second
	started := time.Now()
	for i, sym := range need {
		if ctx.Err() != nil || time.Since(started) > budget {
			break
		}
		var env bitgetEnvelope
		path := "/api/v2/mix/market/funding-time?symbol=" + url.QueryEscape(sym) + "&productType=USDT-FUTURES"
		if err := b.get(ctx, path, &env); err != nil || env.err("funding-time") != nil {
			continue
		}
		var rows []struct {
			NextFundingTime string `json:"nextFundingTime"`
			RatePeriod      string `json:"ratePeriod"`
		}
		if json.Unmarshal(env.Data, &rows) != nil || len(rows) == 0 {
			continue
		}
		t, okT := ParseNum(rows[0].NextFundingTime)
		if !okT || t <= 0 {
			continue
		}
		if rp, okR := ParseNum(rows[0].RatePeriod); okR {
			b.mu.RLock()
			in := b.inst[sym]
			b.mu.RUnlock()
			if in.IntervalH > 0 && rp != in.IntervalH {
				mismatched++
				continue
			}
		}
		got[sym] = int64(t)
		if i < len(need)-1 {
			time.Sleep(pace)
		}
	}
	b.mu.Lock()
	for k, v := range got {
		b.nextFunding[k] = v
	}
	b.mu.Unlock()
	return len(got), mismatched
}

func (b *BitgetPerp) Fundings() map[string]BitgetFunding {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]BitgetFunding, len(b.fundings))
	for k, v := range b.fundings {
		v.NextFundingMs = b.nextFunding[k]
		out[k] = v
	}
	return out
}

// MinNotionalUSD is the venue's own minimum, not a preference.
func (b *BitgetPerp) MinNotionalUSD(symbol string) (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	in, ok := b.inst[symbol]
	if !ok || in.MinTradeUSDT <= 0 {
		return 0, false
	}
	return in.MinTradeUSDT, true
}

// Book reads depth. Sizes are BASE UNITS -- no contract conversion.
func (b *BitgetPerp) Book(ctx context.Context, symbol string) (Book, error) {
	out := Book{Venue: "bitget", Symbol: symbol, Kind: "swap"}
	b.mu.RLock()
	_, known := b.inst[symbol]
	b.mu.RUnlock()
	if !known {
		return out, fmt.Errorf("bitget: %s is not a loaded instrument", symbol)
	}
	depth := b.Depth
	if depth <= 0 {
		depth = 15
	}
	var env bitgetEnvelope
	path := fmt.Sprintf("/api/v2/mix/market/orderbook?symbol=%s&productType=USDT-FUTURES&limit=%d",
		url.QueryEscape(symbol), depth)
	if err := b.get(ctx, path, &env); err != nil {
		return out, err
	}
	if err := env.err("orderbook " + symbol); err != nil {
		return out, err
	}
	var d struct {
		Asks [][]string `json:"asks"`
		Bids [][]string `json:"bids"`
		Ts   string     `json:"ts"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return out, fmt.Errorf("bitget: decoding orderbook %s: %w", symbol, err)
	}
	if ts, ok := ParseNum(d.Ts); ok && ts > 0 {
		out.At = time.UnixMilli(int64(ts)).UTC()
	}
	out.Bids = parsePairs(d.Bids)
	out.Asks = parsePairs(d.Asks)
	out.Finalise()
	return out, nil
}
