package crossvenue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SETTLED FUNDING, NOT PREDICTED FUNDING.
//
// # THE MISTAKE THIS CORRECTS
//
// Until 2026-08-19 every entry was decided on the venue's PREDICTED funding
// rate. Measured against what those same positions actually settled:
//
//	mean predicted   7.30 bps/hr
//	mean settled     0.45 bps/hr
//	8 of 8 missed LOW, average miss -6.86 bps/hr
//	4 of 8 settled NEGATIVE
//
// Funding is a time-weighted average premium over the interval. Early in an
// interval that average is drawn from a short, noisy window, so the published
// prediction is an extreme that converges toward truth as the interval fills.
// Screening thousands of pairs for the WIDEST spread selects precisely the
// pairs where that estimator's error is largest -- and then differences two of
// them, doubling the noise.
//
// There was no decaying edge. There was estimation error, and the screen was
// sorting for it.
//
// # WHAT THIS DOES INSTEAD
//
// Reads each venue's record of funding it has ALREADY PAID. A spread that has
// been wide across several settled intervals is an economic difference between
// two venues. A spread that is wide in one prediction is a measurement.
//
// The predicted rate keeps two jobs it is good at: the settlement calendar, and
// telling an open position what is happening now. It no longer decides entries.
const settledTimeout = 25 * time.Second

// SettledRate summarises what a symbol actually paid over recent settlements.
type SettledRate struct {
	Venue      string  `json:"venue"`
	Symbol     string  `json:"symbol"`
	BpsPerHour float64 `json:"bps_per_hour"` // mean across the whole window
	// RecentBpsPerHour is the mean over the last few settlements only.
	//
	// The full window says whether a spread is REAL; the recent window says
	// whether it is STILL HAPPENING. BICO on 2026-08-20 averaged 3.67 bps/hr
	// across twelve settlements while the live spread had already fallen to
	// 0.50 -- a genuine episode, over before the mean revealed it.
	RecentBpsPerHour float64   `json:"recent_bps_per_hour"`
	RecentIntervals  int       `json:"recent_intervals"`
	Intervals        int       `json:"intervals"`      // how many settlements
	SameSignFrac     float64   `json:"same_sign_frac"` // persistence, 0..1
	LastSettleMs     int64     `json:"last_settle_ms"`
	FetchedAt        time.Time `json:"fetched_at"`
}

// Known reports whether this is usable. An unfetched or empty history is
// refused rather than treated as zero -- zero is a claim, absence is not.
func (s SettledRate) Known() bool { return s.Intervals > 0 }

type settlement struct {
	tsMs int64
	bps  float64 // per interval, as settled
}

// SettledCache holds recent settled history per venue+symbol.
//
// Entries expire on a TTL rather than per poll: settled history only changes
// when a settlement happens, which is every 1 to 8 hours depending on the
// symbol. Refetching it every ten minutes would be 5,000 pointless requests.
type SettledCache struct {
	HTTP     *http.Client
	TTL      time.Duration
	DataPath string

	mu         sync.RWMutex
	data       map[string]SettledRate
	lighterIDs map[string]int // SYMBOL -> market_id, loaded once
}

func NewSettledCache(ttl time.Duration, dataPath string) *SettledCache {
	if ttl <= 0 {
		ttl = 45 * time.Minute
	}
	c := &SettledCache{
		HTTP:       &http.Client{Timeout: settledTimeout},
		TTL:        ttl,
		DataPath:   dataPath,
		data:       map[string]SettledRate{},
		lighterIDs: map[string]int{},
	}
	c.load()
	return c
}

func (c *SettledCache) load() {
	if c.DataPath == "" {
		return
	}
	b, err := os.ReadFile(c.DataPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "settled: cannot read %s: %v\n", c.DataPath, err)
		}
		return
	}

	var savedData map[string]SettledRate
	if err := json.Unmarshal(b, &savedData); err != nil {
		fmt.Fprintf(os.Stderr, "settled: %s is corrupt, ignoring it: %v\n", c.DataPath, err)
		return
	}

	loaded := 0
	dropped := 0
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, v := range savedData {
		if time.Since(v.FetchedAt) > c.TTL {
			dropped++
			continue
		}
		c.data[k] = v
		loaded++
	}

	fmt.Fprintf(os.Stderr, "settled: loaded %d entries, dropped %d expired from %s\n", loaded, dropped, c.DataPath)
}

func (c *SettledCache) save() error {
	if c.DataPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.DataPath), 0o755); err != nil {
		return err
	}

	c.mu.RLock()
	dataCopy := make(map[string]SettledRate, len(c.data))
	for k, v := range c.data {
		dataCopy[k] = v
	}
	c.mu.RUnlock()

	tmpPath := c.DataPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	if err := json.NewEncoder(f).Encode(dataCopy); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, c.DataPath)
}

func (c *SettledCache) key(venue, symbol string) string { return venue + "|" + symbol }

// Get returns a cached rate. ok is false when absent or stale.
func (c *SettledCache) Get(venue, symbol string) (SettledRate, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.data[c.key(venue, symbol)]
	if !ok || time.Since(r.FetchedAt) > c.TTL {
		return SettledRate{}, false
	}
	return r, true
}

// Size reports how many symbols are cached, for the pass summary.
func (c *SettledCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// Ensure fetches settled history for the given venue symbols that are missing
// or stale, bounded by TIME rather than by request count.
//
// Bounding by count is only a time bound if every request is fast. When a venue
// throttles, each can sit on the HTTP timeout instead -- 200 of those is over
// an hour, and that stalled a whole pass of this book on 2026-08-18.
func (c *SettledCache) Ensure(ctx context.Context, venue string, symbols []string,
	intervalHours func(string) float64, budget time.Duration, pace time.Duration) (fetched, failed int) {

	if budget <= 0 {
		budget = 20 * time.Second
	}
	if pace <= 0 {
		pace = 120 * time.Millisecond
	}
	started := time.Now()
	for _, sym := range symbols {
		if ctx.Err() != nil || time.Since(started) > budget {
			break
		}
		if _, ok := c.Get(venue, sym); ok {
			continue
		}
		// AN UNKNOWN INTERVAL IS A REFUSAL, NOT AN 8-HOUR GUESS.
		//
		// This defaulted to 8 until 2026-08-20. MEXC publishes collectCycle per
		// symbol -- 1h for COTI, 4h for HOME and ACE -- and settledscan never
		// hydrated it, so every MEXC rate was divided by 8 regardless. A 4h
		// symbol read half its true rate and a 1h symbol an eighth, which
		// manufactured venue "spreads" of exactly 2.0x and 8.1x and had me
		// asserting a funding-cap mechanism that does not exist.
		//
		// It is the same defect this whole project exists because of, written
		// into the tool built to measure it.
		ivl := 0.0
		if intervalHours != nil {
			ivl = intervalHours(sym)
		}
		if ivl <= 0 {
			failed++
			continue
		}
		ss, err := c.fetch(ctx, venue, sym)
		if err != nil || len(ss) == 0 {
			failed++
			time.Sleep(pace)
			continue
		}
		c.mu.Lock()
		c.data[c.key(venue, sym)] = summarise(venue, sym, ss, ivl)
		c.mu.Unlock()
		fetched++
		time.Sleep(pace)
	}

	if fetched > 0 {
		if err := c.save(); err != nil {
			fmt.Fprintf(os.Stderr, "settled: failed to save cache: %v\n", err)
		}
	}
	return fetched, failed
}

// summarise reduces a settlement series to a rate and a persistence measure.
//
// SameSignFrac is the point. A mean of +6 bps/hr built from +30, -20, +12, -4
// is not an edge; the same mean built from +6, +7, +5, +6 is. One number cannot
// tell those apart, so both are carried.
func summarise(venue, symbol string, ss []settlement, intervalHours float64) SettledRate {
	if intervalHours <= 0 {
		intervalHours = 8
	}
	// Newest first, so the recent window is a prefix. Venues disagree about
	// ordering, so this is sorted rather than assumed.
	sort.Slice(ss, func(i, j int) bool { return ss[i].tsMs > ss[j].tsMs })
	var recentSum float64
	recentN := 0
	for i := 0; i < len(ss) && i < recentWindow; i++ {
		recentSum += ss[i].bps / intervalHours
		recentN++
	}

	var sum float64
	pos, neg := 0, 0
	var last int64
	for _, s := range ss {
		sum += s.bps / intervalHours
		if s.bps > 0 {
			pos++
		} else if s.bps < 0 {
			neg++
		}
		if s.tsMs > last {
			last = s.tsMs
		}
	}
	n := len(ss)
	frac := 0.0
	if n > 0 {
		frac = math.Max(float64(pos), float64(neg)) / float64(n)
	}
	return SettledRate{
		Venue: venue, Symbol: symbol,
		BpsPerHour: sum / float64(n), Intervals: n,
		RecentBpsPerHour: func() float64 {
			if recentN == 0 {
				return 0
			}
			return recentSum / float64(recentN)
		}(),
		RecentIntervals: recentN,
		SameSignFrac:    frac, LastSettleMs: last,
		FetchedAt: time.Now().UTC(),
	}
}

func (c *SettledCache) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vega-settled")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %.120s", resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

func (c *SettledCache) postJSON(ctx context.Context, url string, body any, out any) error {
	reqBody, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vega-settled")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %.120s", resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

const settledWindow = 12 // settlements to summarise

// recentWindow is how many of the most recent settlements define "still
// happening". Three is a trade: fewer is noisier, more is staler.
const recentWindow = 3

const lighterSettledBase = "https://mainnet.zklighter.elliot.ai/api/v1"

// lighterMarketID maps a symbol to Lighter's numeric market id, loaded once.
//
// Lighter addresses markets by integer while everything else here is keyed by
// symbol, so the translation lives with the fetcher rather than leaking a
// venue quirk into the cache's interface.
func (c *SettledCache) lighterMarketID(ctx context.Context, symbol string) (int, error) {
	up := strings.ToUpper(strings.TrimSpace(symbol))
	c.mu.RLock()
	id, ok := c.lighterIDs[up]
	c.mu.RUnlock()
	if ok {
		return id, nil
	}
	var raw map[string]json.RawMessage
	if err := c.getJSON(ctx, lighterSettledBase+"/orderBookDetails", &raw); err != nil {
		return 0, err
	}
	var rows []struct {
		MarketID int    `json:"market_id"`
		Symbol   string `json:"symbol"`
	}
	for _, k := range []string{"order_book_details", "orderBookDetails", "order_books"} {
		if v, present := raw[k]; present && json.Unmarshal(v, &rows) == nil && len(rows) > 0 {
			break
		}
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("lighter: could not read the market list")
	}
	m := make(map[string]int, len(rows))
	for _, r := range rows {
		m[strings.ToUpper(r.Symbol)] = r.MarketID
	}
	c.mu.Lock()
	c.lighterIDs = m
	c.mu.Unlock()
	if id, ok := m[up]; ok {
		return id, nil
	}
	return 0, fmt.Errorf("lighter: no market for %s", symbol)
}

// fetch reads one symbol's settled funding history from its venue.
//
// A venue without a fetcher here returns an error, which becomes a refusal
// upstream. That is deliberate: a pair whose settled history cannot be read
// must not be entered on a prediction instead.
func (c *SettledCache) fetch(ctx context.Context, venue, symbol string) ([]settlement, error) {
	switch venue {
	case "hyperliquid":
		var env []struct {
			FundingRate string `json:"fundingRate"`
			Time        int64  `json:"time"`
		}
		// Hyperliquid settles HOURLY.
		now := time.Now().UnixMilli()
		from := now - int64(settledWindow+2)*3600*1000
		req := map[string]any{
			"type":      "fundingHistory",
			"coin":      symbol,
			"startTime": from,
		}
		if err := c.postJSON(ctx, "https://api.hyperliquid.xyz/info", req, &env); err != nil {
			return nil, err
		}
		// Return up to settledWindow items
		start := len(env) - settledWindow
		if start < 0 {
			start = 0
		}
		out := make([]settlement, 0, len(env)-start)
		for i := start; i < len(env); i++ {
			r := env[i]
			out = append(out, settlement{r.Time, parseNum(r.FundingRate) * 10000})
		}
		return out, nil

	case "binance":
		var rows []struct {
			FundingRate string `json:"fundingRate"`
			FundingTime int64  `json:"fundingTime"`
		}
		u := fmt.Sprintf("https://fapi.binance.com/fapi/v1/fundingRate?symbol=%s&limit=%d", symbol, settledWindow)
		if err := c.getJSON(ctx, u, &rows); err != nil {
			return nil, err
		}
		out := make([]settlement, 0, len(rows))
		for _, r := range rows {
			out = append(out, settlement{r.FundingTime, parseNum(r.FundingRate) * 10000})
		}
		return out, nil

	case "bybit":
		var env struct {
			RetCode int `json:"retCode"`
			Result  struct {
				List []struct {
					FundingRate          string `json:"fundingRate"`
					FundingRateTimestamp string `json:"fundingRateTimestamp"`
				} `json:"list"`
			} `json:"result"`
		}
		u := fmt.Sprintf("https://api.bybit.com/v5/market/funding/history?category=linear&symbol=%s&limit=%d", symbol, settledWindow)
		if err := c.getJSON(ctx, u, &env); err != nil {
			return nil, err
		}
		if env.RetCode != 0 {
			return nil, fmt.Errorf("bybit retCode %d", env.RetCode)
		}
		out := make([]settlement, 0, len(env.Result.List))
		for _, r := range env.Result.List {
			out = append(out, settlement{int64(parseNum(r.FundingRateTimestamp)), parseNum(r.FundingRate) * 10000})
		}
		return out, nil

	case "okx":
		var env struct {
			Code string `json:"code"`
			Data []struct {
				FundingRate  string `json:"fundingRate"`
				RealizedRate string `json:"realizedRate"`
				FundingTime  string `json:"fundingTime"`
			} `json:"data"`
		}
		u := fmt.Sprintf("https://www.okx.com/api/v5/public/funding-rate-history?instId=%s&limit=%d", symbol, settledWindow)
		if err := c.getJSON(ctx, u, &env); err != nil {
			return nil, err
		}
		if env.Code != "0" {
			return nil, fmt.Errorf("okx code %s", env.Code)
		}
		out := make([]settlement, 0, len(env.Data))
		for _, r := range env.Data {
			// realizedRate is what was actually charged; fundingRate is the
			// quote. Prefer the realised one where OKX supplies it.
			v := r.RealizedRate
			if v == "" {
				v = r.FundingRate
			}
			out = append(out, settlement{int64(parseNum(r.FundingTime)), parseNum(v) * 10000})
		}
		return out, nil

	case "bitget":
		var env struct {
			Code string `json:"code"`
			Data []struct {
				FundingRate string `json:"fundingRate"`
				FundingTime string `json:"fundingTime"`
			} `json:"data"`
		}
		u := fmt.Sprintf("https://api.bitget.com/api/v2/mix/market/history-fund-rate?symbol=%s&productType=USDT-FUTURES&pageSize=%d&pageNo=1", symbol, settledWindow)
		if err := c.getJSON(ctx, u, &env); err != nil {
			return nil, err
		}
		out := make([]settlement, 0, len(env.Data))
		for _, r := range env.Data {
			out = append(out, settlement{int64(parseNum(r.FundingTime)), parseNum(r.FundingRate) * 10000})
		}
		return out, nil

	case "mexc":
		var env struct {
			Success bool `json:"success"`
			Data    struct {
				ResultList []struct {
					FundingRate float64 `json:"fundingRate"`
					SettleTime  int64   `json:"settleTime"`
				} `json:"resultList"`
			} `json:"data"`
		}
		u := fmt.Sprintf("https://contract.mexc.com/api/v1/contract/funding_rate/history?symbol=%s&page_num=1&page_size=%d", symbol, settledWindow)
		if err := c.getJSON(ctx, u, &env); err != nil {
			return nil, err
		}
		out := make([]settlement, 0, len(env.Data.ResultList))
		for _, r := range env.Data.ResultList {
			out = append(out, settlement{r.SettleTime, r.FundingRate * 10000})
		}
		return out, nil
	case "lighter":
		// UNITS, ESTABLISHED FROM THE VENUE'S OWN DATA 2026-08-19.
		//
		// rate is a PERCENTAGE applied at each HOURLY settlement. Confirmed by
		// cross-multiplying against the value field the same response carries:
		//
		//	rate 0.0007% x BTC $64,337 = $0.4504   vs   value $0.45097
		//
		// and by BTC landing at ~0.0875 bps/hr against 0.031-0.060 on the five
		// centralised venues. The three other readings of that field give
		// 0.011, 1.09 and 8.75 bps/hr, none of which are survivable.
		//
		// Note this is the SETTLED series, one record per hourly payment. It is
		// not the live quote, which Lighter publishes over an 8-hour period.
		id, err := c.lighterMarketID(ctx, symbol)
		if err != nil {
			return nil, err
		}
		now := time.Now().Unix()
		from := now - int64(settledWindow+2)*3600
		var env struct {
			Code     int `json:"code"`
			Fundings []struct {
				Timestamp int64  `json:"timestamp"`
				Rate      string `json:"rate"`
				Direction string `json:"direction"`
			} `json:"fundings"`
		}
		u := fmt.Sprintf("%s/fundings?market_id=%d&resolution=1h&start_timestamp=%d&end_timestamp=%d&count_back=%d",
			lighterSettledBase, id, from, now, settledWindow)
		if err := c.getJSON(ctx, u, &env); err != nil {
			return nil, err
		}
		out := make([]settlement, 0, len(env.Fundings))
		for _, r := range env.Fundings {
			bps := parseNum(r.Rate) * 100
			// direction names who PAYS. "short" means shorts pay longs, which
			// is a negative rate in the convention every other venue here uses.
			if strings.EqualFold(r.Direction, "short") {
				bps = -bps
			}
			out = append(out, settlement{r.Timestamp * 1000, bps})
		}
		return out, nil
	}
	return nil, fmt.Errorf("no settled-history fetcher for %s", venue)
}

func parseNum(s string) float64 {
	var f float64
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	fmt.Sscanf(s, "%g", &f)
	return f
}
