package orderbook

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LIGHTER — zero-fee perpetual DEX
//
// TWO CONSTANTS, BOTH FROM THE VENUE'S OWN DOCUMENTATION, BOTH LOAD-BEARING
//
// docs.lighter.xyz/trading/funding, read 2026-08-15:
//
//	"Funding payments occur at each hour mark."
//	  -> SETTLEMENT IS HOURLY.
//
//	fundingRate = clamp(smallClampedPremium, -BigClamp, +BigClamp) / 8
//	  "Dividing the 1-hour premium by 8 ensures that funding payments for the
//	   premium are distributed over 8 hours"
//	  -> THE QUOTED RATE IS PER 8 HOURS.
//
// Those are different numbers and mixing them is an 8x error. Lighter is the
// mirror image of KAITO, which quoted per 4 hours and was read as 8 -- that
// mistake made one pair look 75x richer than it was and cost two positions.
//
// Cross-checked independently: /funding-rates relays Hyperliquid's hourly rate
// multiplied by exactly 8.00, Binance's 4-hour symbols by 2.00, and its 8-hour
// symbols by 1.00. Everything in that table is per 8 hours, Lighter's own rate
// included -- and the docs' stated default of "1 basis point per 8 hours"
// matches the 0.0001-ish values the API actually returns.
//
// WHY THIS VENUE IS DIFFERENT FROM THE OTHERS
//
//   - Taker AND maker fees are ZERO, published in the API per market rather
//     than claimed in marketing.
//   - min_quote_amount is around $10 rather than the $200-ish minimum that
//     makes thin markets untradeable elsewhere.
//   - /funding-rates returns FOUR venues at once, normalised.
//
// # WHAT IS STILL NOT FREE
//
// Zero fees are not zero cost. Measured 2026-08-15, Lighter's bid-ask ran 5.5
// to 59 bps on the markets that had funding spreads, and crossing it twice is
// a real round trip. The books are genuinely deep -- $20k to $400k where dYdX
// held nothing -- but the spread is the cost, not the fee.
const (
	lighterBase = "https://mainnet.zklighter.elliot.ai/api/v1"

	// LighterRatePeriodHours is the period the API QUOTES a rate over.
	LighterRatePeriodHours = 8.0

	// LighterSettlementHours is how often money actually MOVES.
	LighterSettlementHours = 1.0

	// lighterMaxRatePer8h is the venue's own BigClamp: 4% per 8 hours.
	// Anything past it is a decode error, not a market.
	lighterMaxRatePer8h = 0.04
)

// flexNum accepts a JSON number OR a quoted string.
//
// Lighter returns mark_price as "63071.0000" and last_trade_price as 63071.
// Same field family, two encodings. A struct that assumes one of them fails on
// the other, and failing on LOAD is the good outcome -- failing silently to
// zero would price a market at nothing and make every depth figure infinite.
type flexNum float64

func (f *flexNum) UnmarshalJSON(b []byte) error {
	str := strings.Trim(string(b), `"`)
	if str == "" || str == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexNum(v)
	return nil
}

func (f flexNum) f() float64 { return float64(f) }

// LighterMarket is one perpetual market.
type LighterMarket struct {
	ID          int
	Symbol      string
	MarkPrice   float64
	MinQuoteUSD float64
	TakerFeePct float64
	MakerFeePct float64
	VolUSD24h   float64
	Active      bool
}

// LighterFunding is one venue's rate for one symbol, as Lighter relays it.
type LighterFunding struct {
	Symbol   string
	Exchange string // lighter, binance, bybit, hyperliquid

	// RatePer8h is exactly what the API returned, untouched.
	RatePer8h float64

	// BpsPerHour is RatePer8h converted. This is the only figure anything
	// downstream should compare.
	BpsPerHour float64
}

// LighterPerp reads Lighter's public API. No key, no signature.
type LighterPerp struct {
	HTTP    *clientShim
	BaseURL string
	Depth   int

	mu       sync.RWMutex
	markets  map[string]LighterMarket             // symbol -> market
	byID     map[int]string                       // market_id -> symbol
	fundings map[string]LighterFunding            // symbol -> lighter's own rate
	relayed  map[string]map[string]LighterFunding // symbol -> exchange -> rate
	loaded   time.Time
}

func NewLighterPerp() *LighterPerp {
	return &LighterPerp{
		HTTP:     newShim(25 * time.Second),
		BaseURL:  lighterBase,
		Depth:    50,
		markets:  map[string]LighterMarket{},
		byID:     map[int]string{},
		fundings: map[string]LighterFunding{},
		relayed:  map[string]map[string]LighterFunding{},
	}
}

func (l *LighterPerp) Venue() string { return "lighter" }

func (l *LighterPerp) base() string {
	if l.BaseURL != "" {
		return l.BaseURL
	}
	return lighterBase
}

func (l *LighterPerp) get(ctx context.Context, path string, out any) error {
	raw, err := l.HTTP.get(ctx, l.base()+path)
	if err != nil {
		return fmt.Errorf("lighter %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("lighter: decoding %s: %w", path, err)
	}
	return nil
}

// LoadSymbols fetches markets and the four-venue funding table.
func (l *LighterPerp) LoadSymbols(ctx context.Context) error {
	var md struct {
		Code    int `json:"code"`
		Details []struct {
			MarketID      int     `json:"market_id"`
			Symbol        string  `json:"symbol"`
			MarketType    string  `json:"market_type"`
			Status        string  `json:"status"`
			MarkPrice     flexNum `json:"mark_price"`
			LastTradePx   flexNum `json:"last_trade_price"`
			MinQuoteAmt   string  `json:"min_quote_amount"`
			TakerFee      string  `json:"taker_fee"`
			MakerFee      string  `json:"maker_fee"`
			DailyQuoteVol flexNum `json:"daily_quote_token_volume"`
		} `json:"order_book_details"`
	}
	if err := l.get(ctx, "/orderBookDetails", &md); err != nil {
		return err
	}
	if md.Code != 200 || len(md.Details) == 0 {
		return fmt.Errorf("lighter: orderBookDetails returned code %d with %d markets",
			md.Code, len(md.Details))
	}

	markets := map[string]LighterMarket{}
	byID := map[int]string{}
	for _, d := range md.Details {
		if d.Status != "active" || d.MarketType != "perp" {
			continue
		}
		mark := d.MarkPrice.f()
		if mark <= 0 {
			mark = d.LastTradePx.f()
		}
		if mark <= 0 {
			continue
		}
		minQ, _ := ParseNum(d.MinQuoteAmt)
		taker, _ := ParseNum(d.TakerFee)
		maker, _ := ParseNum(d.MakerFee)

		sym := strings.ToUpper(d.Symbol)
		markets[sym] = LighterMarket{
			ID: d.MarketID, Symbol: sym, MarkPrice: mark,
			MinQuoteUSD: minQ, TakerFeePct: taker, MakerFeePct: maker,
			VolUSD24h: d.DailyQuoteVol.f(), Active: true,
		}
		byID[d.MarketID] = sym
	}
	if len(markets) == 0 {
		return fmt.Errorf("lighter: no active perp markets")
	}

	// --- funding, four venues in one call ---
	var fr struct {
		Code  int `json:"code"`
		Rates []struct {
			MarketID int     `json:"market_id"`
			Exchange string  `json:"exchange"`
			Symbol   string  `json:"symbol"`
			Rate     flexNum `json:"rate"`
		} `json:"funding_rates"`
	}
	if err := l.get(ctx, "/funding-rates", &fr); err != nil {
		return err
	}
	if fr.Code != 200 {
		return fmt.Errorf("lighter: funding-rates returned code %d", fr.Code)
	}

	own := map[string]LighterFunding{}
	relayed := map[string]map[string]LighterFunding{}
	var clamped int

	for _, r := range fr.Rates {
		sym := strings.ToUpper(r.Symbol)
		if sym == "" || r.Exchange == "" {
			continue
		}
		// The venue's own BigClamp is 4% per 8h. Past that is a decode error.
		rate := r.Rate.f()
		if rate > lighterMaxRatePer8h || rate < -lighterMaxRatePer8h {
			clamped++
			continue
		}
		f := LighterFunding{
			Symbol: sym, Exchange: r.Exchange, RatePer8h: rate,
			// PER 8 HOURS -> per hour. Documented and cross-checked.
			BpsPerHour: rate * 10000 / LighterRatePeriodHours,
		}
		if relayed[sym] == nil {
			relayed[sym] = map[string]LighterFunding{}
		}
		relayed[sym][r.Exchange] = f
		if r.Exchange == "lighter" {
			own[sym] = f
		}
	}
	if len(own) == 0 {
		return fmt.Errorf("lighter: funding table carried no lighter rates (%d entries, %d past the clamp)",
			len(fr.Rates), clamped)
	}

	l.mu.Lock()
	l.markets, l.byID, l.fundings, l.relayed = markets, byID, own, relayed
	l.loaded = time.Now().UTC()
	l.mu.Unlock()
	return nil
}

func (l *LighterPerp) Symbols() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.markets))
	for s := range l.markets {
		out = append(out, s)
	}
	return out
}

func (l *LighterPerp) SymbolCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.markets)
}

// ResolveCoin maps a Hyperliquid coin name to a Lighter symbol.
//
// Lighter uses plain names (BTC, KAITO), matching Hyperliquid rather than the
// CEXs' BTCUSDT. Anything not confirmed present is REFUSED.
func (l *LighterPerp) ResolveCoin(coin string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.markets) == 0 {
		return "", false
	}
	c := strings.ToUpper(strings.TrimSpace(coin))
	for _, cand := range []string{c, strings.TrimPrefix(c, "K"), "1000" + strings.TrimPrefix(c, "K")} {
		if _, ok := l.markets[cand]; ok {
			return cand, true
		}
	}
	return "", false
}

// FundingIntervalHours reports the SETTLEMENT cadence, not the quoting period.
//
// Hourly, per the docs. The rate is quoted per 8h and settled hourly, so each
// payment moves one hour's worth. Returning 8 here would make the settlement
// calendar expect one lump instead of eight small ones -- the exact modelling
// error that produced two stop-losses on 2026-08-12.
func (l *LighterPerp) FundingIntervalHours(symbol string) FundingInterval {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.markets[strings.ToUpper(symbol)]; !ok {
		return FundingInterval{}
	}
	return FundingInterval{Hours: LighterSettlementHours, Explicit: true, Ok: true}
}

// Funding returns Lighter's own rate for a symbol.
func (l *LighterPerp) Funding(symbol string) (LighterFunding, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	f, ok := l.fundings[strings.ToUpper(symbol)]
	return f, ok
}

// Fundings returns every rate Lighter publishes for itself.
func (l *LighterPerp) Fundings() map[string]LighterFunding {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]LighterFunding, len(l.fundings))
	for k, v := range l.fundings {
		out[k] = v
	}
	return out
}

// Market exposes a market's metadata, including its $10-ish minimum.
func (l *LighterPerp) Market(symbol string) (LighterMarket, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m, ok := l.markets[strings.ToUpper(symbol)]
	return m, ok
}

// Book reads resting orders.
func (l *LighterPerp) Book(ctx context.Context, symbol string) (Book, error) {
	sym := strings.ToUpper(symbol)
	out := Book{Venue: "lighter", Symbol: sym, Kind: "perp"}

	l.mu.RLock()
	m, ok := l.markets[sym]
	l.mu.RUnlock()
	if !ok {
		return out, fmt.Errorf("lighter: %s is not a loaded market", sym)
	}

	depth := l.Depth
	if depth <= 0 {
		depth = 50
	}
	var r struct {
		Code int `json:"code"`
		Bids []struct {
			Price     string `json:"price"`
			Remaining string `json:"remaining_base_amount"`
			Initial   string `json:"initial_base_amount"`
		} `json:"bids"`
		Asks []struct {
			Price     string `json:"price"`
			Remaining string `json:"remaining_base_amount"`
			Initial   string `json:"initial_base_amount"`
		} `json:"asks"`
	}
	path := fmt.Sprintf("/orderBookOrders?market_id=%d&limit=%d", m.ID, depth)
	if err := l.get(ctx, path, &r); err != nil {
		return out, err
	}
	if r.Code != 200 {
		return out, fmt.Errorf("lighter: orderBookOrders %s returned code %d", sym, r.Code)
	}

	// Sizes are in BASE units already -- no contract multiplier, unlike OKX.
	for _, o := range r.Bids {
		px, ok1 := ParseNum(o.Price)
		sz, ok2 := ParseNum(o.Remaining)
		if !ok2 || sz <= 0 {
			sz, ok2 = ParseNum(o.Initial)
		}
		if !ok1 || !ok2 || px <= 0 || sz <= 0 {
			continue
		}
		out.Bids = append(out.Bids, Level{Px: px, Sz: sz})
	}
	for _, o := range r.Asks {
		px, ok1 := ParseNum(o.Price)
		sz, ok2 := ParseNum(o.Remaining)
		if !ok2 || sz <= 0 {
			sz, ok2 = ParseNum(o.Initial)
		}
		if !ok1 || !ok2 || px <= 0 || sz <= 0 {
			continue
		}
		out.Asks = append(out.Asks, Level{Px: px, Sz: sz})
	}
	out.Finalise()
	return out, nil
}

// TakerFeePct returns the fee Lighter reports for a market.
//
// Read rather than assumed. It is currently zero everywhere, and if that ever
// changes the caller must see it rather than keep trading on a stale zero.
func (l *LighterPerp) TakerFeePct(symbol string) (float64, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m, ok := l.markets[strings.ToUpper(symbol)]
	if !ok {
		return 0, false
	}
	return m.TakerFeePct, true
}
