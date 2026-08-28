package margin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// BYBIT RISK LIMITS
//
// Bybit publishes its maintenance-margin tiers on a PUBLIC endpoint. No key,
// no signature. That is the reason Bybit is the venue this modelling can
// actually be done against -- Binance's equivalent requires an authenticated
// request, so its tiers have to be entered by hand and carry a checked-on
// date instead.
//
// A schedule fetched from the venue's own API is Verified by definition: it
// came from the authority. A schedule typed in by a person is verified only
// as of the day someone read it, and both facts are recorded so they can be
// told apart later.
//
// # THE UNIT TRAP
//
// maintenanceMargin arrives as a decimal fraction: "0.005" means 0.5%. If a
// venue ever switched to percent-style ("0.5" for the same thing) every
// liquidation price in this project would move by 100x, silently, in the
// direction that makes positions look safe. So a rate outside a plausible
// band is REFUSED rather than rescaled by a guess about which unit was meant.
const (
	bybitAPIBase = "https://api.bybit.com"

	// maxPlausibleMMR guards against a UNIT error, not against a large number.
	//
	// Set to 0.5 originally, on the assumption that no real maintenance rate
	// exceeds 50%. Wrong: Bybit's TOP risk tier for BTCUSDT requires 60%, and
	// those brackets exist precisely to make enormous positions prohibitive.
	// The check refused legitimate data on 2026-08-14 -- correctly refusing
	// rather than guessing, but calibrated against the wrong thing.
	//
	// A percent-for-fraction mix-up produces 50 or 100, not 0.6. So the bound
	// belongs just past 1.0, where a maintenance rate stops being arithmetic
	// and starts being a unit error.
	maxPlausibleMMR = 1.0
)

// RiskSource is a venue that can supply a symbol's risk-limit schedule.
type RiskSource interface {
	Venue() string
	Fetch(ctx context.Context, symbol string) (Schedule, error)
	Cached(symbol string) (Schedule, bool)
}

// BybitRisk reads linear-perp risk limits from Bybit's public API.
type BybitRisk struct {
	HTTP    *http.Client
	BaseURL string

	mu    sync.RWMutex
	cache map[string]Schedule
}

// NewBybitRisk returns a reader with a short timeout.
func NewBybitRisk() *BybitRisk {
	return &BybitRisk{
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: bybitAPIBase,
		cache:   map[string]Schedule{},
	}
}

func (b *BybitRisk) Venue() string { return "bybit" }

func (b *BybitRisk) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (b *BybitRisk) base() string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	return bybitAPIBase
}

// bybitEnvelope is Bybit's v5 wrapper.
//
// retCode is where failures live; the HTTP status is 200 either way. Decoding
// an error into an empty tier list would produce a schedule with no brackets,
// which TierFor would then refuse -- correct by accident, but for the wrong
// reason and with no explanation.
type bybitEnvelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
}

// Fetch reads one symbol's risk-limit tiers.
func (b *BybitRisk) Fetch(ctx context.Context, symbol string) (Schedule, error) {
	sch := Schedule{Venue: "bybit", Symbol: symbol}

	u := fmt.Sprintf("%s/v5/market/risk-limit?category=linear&symbol=%s",
		b.base(), url.QueryEscape(symbol))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return sch, err
	}
	resp, err := b.client().Do(req)
	if err != nil {
		return sch, fmt.Errorf("bybit risk-limit %s: %w", symbol, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return sch, err
	}
	if resp.StatusCode != http.StatusOK {
		return sch, fmt.Errorf("bybit risk-limit %s: HTTP %d", symbol, resp.StatusCode)
	}

	var env bybitEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return sch, fmt.Errorf("bybit risk-limit %s: decoding envelope: %w", symbol, err)
	}
	if env.RetCode != 0 {
		return sch, fmt.Errorf("bybit risk-limit %s: retCode %d: %s",
			symbol, env.RetCode, env.RetMsg)
	}

	var res struct {
		List []struct {
			ID                int    `json:"id"`
			Symbol            string `json:"symbol"`
			RiskLimitValue    string `json:"riskLimitValue"`
			MaintenanceMargin string `json:"maintenanceMargin"`
			InitialMargin     string `json:"initialMargin"`
			MaxLeverage       string `json:"maxLeverage"`
			IsLowestRisk      int    `json:"isLowestRisk"`
		} `json:"list"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return sch, fmt.Errorf("bybit risk-limit %s: decoding result: %w", symbol, err)
	}
	if len(res.List) == 0 {
		return sch, fmt.Errorf("bybit risk-limit %s: no tiers returned", symbol)
	}

	for _, r := range res.List {
		cap, okC := parseNum(r.RiskLimitValue)
		mmr, okM := parseNum(r.MaintenanceMargin)
		lev, _ := parseNum(r.MaxLeverage)

		if !okC || !okM || cap <= 0 || mmr <= 0 {
			return sch, fmt.Errorf(
				"bybit risk-limit %s: unusable tier (cap %q, maintenance %q)",
				symbol, r.RiskLimitValue, r.MaintenanceMargin)
		}
		if mmr > maxPlausibleMMR {
			return sch, fmt.Errorf(
				"bybit risk-limit %s: maintenance rate %.4f exceeds the %.2f bound; a rate "+
					"above 100%% is not a margin requirement, it is a percent written where a "+
					"fraction was expected, and rescaling it by a guess would move every "+
					"liquidation price by 100x",
				symbol, mmr, maxPlausibleMMR)
		}

		sch.Tiers = append(sch.Tiers, Tier{
			MaxNotionalUSD:        cap,
			MaintenanceMarginRate: mmr,
			MaxLeverage:           lev,
		})
	}

	sch.Source = u
	sch.VerifiedAt = time.Now().UTC()
	// Read from the venue's own API: the authority on its own liquidation
	// rules. This is the one case where Verified can be set by code.
	sch.Verified = true

	b.mu.Lock()
	if b.cache == nil {
		b.cache = map[string]Schedule{}
	}
	b.cache[symbol] = sch
	b.mu.Unlock()

	return sch, nil
}

// Cached returns a previously fetched schedule.
func (b *BybitRisk) Cached(symbol string) (Schedule, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.cache[symbol]
	return s, ok
}

// FetchAll loads several symbols, pacing requests so the venue does not rate
// limit the run. Errors are collected per symbol rather than aborting the
// batch: one delisted symbol should not stop a replay of forty.
func (b *BybitRisk) FetchAll(ctx context.Context, symbols []string, pace time.Duration) (map[string]Schedule, map[string]error) {
	if pace <= 0 {
		pace = 60 * time.Millisecond
	}
	out := map[string]Schedule{}
	errs := map[string]error{}

	for _, sym := range symbols {
		s, err := b.Fetch(ctx, sym)
		if err != nil {
			errs[sym] = err
		} else {
			out[sym] = s
		}
		time.Sleep(pace)
	}
	return out, errs
}

// parseNum parses one of Bybit's string-encoded numbers.
//
// ok=false rather than 0, because a zero maintenance rate means "never
// liquidated", which is the most dangerous wrong answer this file could give.
func parseNum(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
