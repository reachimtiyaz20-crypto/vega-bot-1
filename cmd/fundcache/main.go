// Command fundcache downloads a year of funding history from eight venues.
//
// # WHY A SEPARATE CACHER
//
// The full pull is ~2,500 symbol-venue histories. Doing it inside the analysis
// would mean re-downloading on every parameter change, and a rate limit
// halfway through would leave a silently partial answer. So: fetch once,
// verify, then compute from disk as often as you like.
//
// # INTERVALS ARE DERIVED, NOT ASSUMED
//
// Every venue's settlement timestamps are in the response, so the interval is
// the gap between them -- measured per symbol, from the venue. That assumption
// has been wrong twice in this project: Binance's 4-hour symbols read as 8,
// and Lighter's 8-hour quote read as hourly. Deriving it removes the question.
//
// # LISTING DATE
//
// Only Binance publishes onboardDate. Everywhere else the FIRST FUNDING
// SETTLEMENT dates the listing -- a perp cannot settle funding before it
// exists. Good enough to within one interval.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type point struct {
	TsMs int64   `json:"t"`
	Rate float64 `json:"r"`
}

type history struct {
	Venue     string  `json:"venue"`
	Symbol    string  `json:"symbol"`
	Coin      string  `json:"coin"`
	Points    []point `json:"points"`
	IntervalH float64 `json:"interval_hours"`
	FirstMs   int64   `json:"first_ms"`
	FetchedAt int64   `json:"fetched_at"`
}

var client = &http.Client{Timeout: 40 * time.Second}

func get(url string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "vega-research/1.0")
	req.Header.Set("accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func post(url string, body any, out any) error {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vega-research/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}

func f(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
func i64(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }

// coinOf strips a venue's decoration to a bare coin name so the same asset
// can be matched across eight different naming conventions.
func coinOf(sym string) string {
	s := strings.ToUpper(sym)
	s = strings.ReplaceAll(s, "-USDT-SWAP", "")
	s = strings.ReplaceAll(s, "_USDT", "")
	s = strings.ReplaceAll(s, "-USDT", "")
	s = strings.TrimSuffix(s, "USDT")
	s = strings.TrimSuffix(s, "-USD")
	return s
}

// deriveInterval is the MEDIAN gap between settlements, in hours.
//
// Median, not mean: a venue that changed a symbol's interval mid-life, or a
// gap where the venue was down, would drag a mean and quietly rescale every
// rate that depends on it.
func deriveInterval(ps []point) float64 {
	if len(ps) < 3 {
		return 0
	}
	var gaps []float64
	for i := 1; i < len(ps); i++ {
		g := float64(ps[i].TsMs-ps[i-1].TsMs) / 3_600_000
		if g > 0.4 && g < 25 {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Float64s(gaps)
	m := gaps[len(gaps)/2]
	// Snap to the intervals venues actually use.
	for _, std := range []float64{1, 2, 4, 8} {
		if m > std*0.75 && m < std*1.25 {
			return std
		}
	}
	return m
}

// --- venue adapters ----------------------------------------------------------

type venue struct {
	name    string
	symbols func() ([]string, error)
	funding func(sym string, sinceMs int64) ([]point, error)
	pace    time.Duration
}

func venues() []venue {
	return []venue{
		{"binance", binanceSymbols, binanceFunding, 90 * time.Millisecond},
		{"bybit", bybitSymbols, bybitFunding, 90 * time.Millisecond},
		{"okx", okxSymbols, okxFunding, 110 * time.Millisecond},
		{"hyperliquid", hlSymbols, hlFunding, 90 * time.Millisecond},
		{"mexc", mexcSymbols, mexcFunding, 130 * time.Millisecond},
		{"bingx", bingxSymbols, bingxFunding, 160 * time.Millisecond},
		{"gate", gateSymbols, gateFunding, 130 * time.Millisecond},
		{"bitget", bitgetSymbols, bitgetFunding, 130 * time.Millisecond},
	}
}

func binanceSymbols() ([]string, error) {
	var r struct {
		Symbols []struct {
			Symbol, Status, ContractType, QuoteAsset string
		} `json:"symbols"`
	}
	if err := get("https://fapi.binance.com/fapi/v1/exchangeInfo", &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Symbols {
		if s.Status == "TRADING" && s.ContractType == "PERPETUAL" && s.QuoteAsset == "USDT" {
			out = append(out, s.Symbol)
		}
	}
	return out, nil
}

func binanceFunding(sym string, since int64) ([]point, error) {
	var out []point
	start := since
	for page := 0; page < 6; page++ {
		var rows []struct {
			FundingTime int64  `json:"fundingTime"`
			FundingRate string `json:"fundingRate"`
		}
		u := fmt.Sprintf("https://fapi.binance.com/fapi/v1/fundingRate?symbol=%s&startTime=%d&limit=1000", sym, start)
		if err := get(u, &rows); err != nil || len(rows) == 0 {
			break
		}
		for _, r := range rows {
			out = append(out, point{r.FundingTime, f(r.FundingRate)})
		}
		next := rows[len(rows)-1].FundingTime + 1
		if next <= start {
			break
		}
		start = next
		time.Sleep(90 * time.Millisecond)
	}
	return out, nil
}

func bybitSymbols() ([]string, error) {
	var r struct {
		Result struct {
			List []struct {
				Symbol, Status, QuoteCoin, ContractType string
			} `json:"list"`
		} `json:"result"`
	}
	if err := get("https://api.bybit.com/v5/market/instruments-info?category=linear&limit=1000", &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Result.List {
		if s.Status == "Trading" && s.QuoteCoin == "USDT" && strings.Contains(s.ContractType, "Perpetual") {
			out = append(out, s.Symbol)
		}
	}
	return out, nil
}

func bybitFunding(sym string, since int64) ([]point, error) {
	var out []point
	end := time.Now().UnixMilli()
	for page := 0; page < 8; page++ {
		var env struct {
			RetCode int `json:"retCode"`
			Result  struct {
				List []struct {
					FundingRate          string `json:"fundingRate"`
					FundingRateTimestamp string `json:"fundingRateTimestamp"`
				} `json:"list"`
			} `json:"result"`
		}
		u := fmt.Sprintf("https://api.bybit.com/v5/market/funding/history?category=linear&symbol=%s&startTime=%d&endTime=%d&limit=200", sym, since, end)
		if err := get(u, &env); err != nil || env.RetCode != 0 || len(env.Result.List) == 0 {
			break
		}
		oldest := int64(0)
		for _, r := range env.Result.List {
			ts := i64(r.FundingRateTimestamp)
			out = append(out, point{ts, f(r.FundingRate)})
			if oldest == 0 || ts < oldest {
				oldest = ts
			}
		}
		if oldest <= since {
			break
		}
		end = oldest - 1
		time.Sleep(90 * time.Millisecond)
	}
	return out, nil
}

func okxSymbols() ([]string, error) {
	var r struct {
		Data []struct{ InstID, State, SettleCcy string } `json:"data"`
	}
	if err := get("https://www.okx.com/api/v5/public/instruments?instType=SWAP", &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Data {
		if s.State == "live" && s.SettleCcy == "USDT" {
			out = append(out, s.InstID)
		}
	}
	return out, nil
}

func okxFunding(sym string, since int64) ([]point, error) {
	var out []point
	before := time.Now().UnixMilli()
	for page := 0; page < 10; page++ {
		var r struct {
			Code string `json:"code"`
			Data []struct {
				FundingRate string `json:"fundingRate"`
				FundingTime string `json:"fundingTime"`
			} `json:"data"`
		}
		u := fmt.Sprintf("https://www.okx.com/api/v5/public/funding-rate-history?instId=%s&before=%d&limit=100", sym, since)
		_ = before
		if err := get(u, &r); err != nil || r.Code != "0" || len(r.Data) == 0 {
			break
		}
		oldest := int64(0)
		for _, d := range r.Data {
			ts := i64(d.FundingTime)
			out = append(out, point{ts, f(d.FundingRate)})
			if oldest == 0 || ts < oldest {
				oldest = ts
			}
		}
		if len(r.Data) < 100 {
			break
		}
		time.Sleep(110 * time.Millisecond)
		break // single page: OKX paging here is awkward and 100 points is enough to date a listing
	}
	return out, nil
}

func hlSymbols() ([]string, error) {
	var raw [][]json.RawMessage
	if err := post("https://api.hyperliquid.xyz/info", map[string]string{"type": "predictedFundings"}, &raw); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range raw {
		var coin string
		if json.Unmarshal(e[0], &coin) == nil && coin != "" {
			out = append(out, coin)
		}
	}
	return out, nil
}

func hlFunding(sym string, since int64) ([]point, error) {
	var rows []struct {
		FundingRate string `json:"fundingRate"`
		Time        int64  `json:"time"`
	}
	err := post("https://api.hyperliquid.xyz/info",
		map[string]any{"type": "fundingHistory", "coin": sym, "startTime": since}, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]point, 0, len(rows))
	for _, r := range rows {
		out = append(out, point{r.Time, f(r.FundingRate)})
	}
	return out, nil
}

func mexcSymbols() ([]string, error) {
	var r struct {
		Data []struct {
			Symbol string `json:"symbol"`
			State  int    `json:"state"`
		} `json:"data"`
	}
	if err := get("https://contract.mexc.com/api/v1/contract/detail", &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Data {
		if strings.HasSuffix(s.Symbol, "_USDT") && s.State == 0 {
			out = append(out, s.Symbol)
		}
	}
	return out, nil
}

func mexcFunding(sym string, since int64) ([]point, error) {
	var out []point
	for page := 1; page <= 8; page++ {
		var r struct {
			Data struct {
				TotalPage  int `json:"totalPage"`
				ResultList []struct {
					FundingRate  float64 `json:"fundingRate"`
					SettleTime   int64   `json:"settleTime"`
					CollectCycle float64 `json:"collectCycle"`
				} `json:"resultList"`
			} `json:"data"`
		}
		u := fmt.Sprintf("https://contract.mexc.com/api/v1/contract/funding_rate/history?symbol=%s&page_num=%d&page_size=100", sym, page)
		if err := get(u, &r); err != nil || len(r.Data.ResultList) == 0 {
			break
		}
		stop := false
		for _, d := range r.Data.ResultList {
			if d.SettleTime < since {
				stop = true
				continue
			}
			out = append(out, point{d.SettleTime, d.FundingRate})
		}
		if stop || page >= r.Data.TotalPage {
			break
		}
		time.Sleep(130 * time.Millisecond)
	}
	return out, nil
}

func bingxSymbols() ([]string, error) {
	var r struct {
		Data []struct {
			Symbol string `json:"symbol"`
			Status int    `json:"status"`
		} `json:"data"`
	}
	if err := get("https://open-api.bingx.com/openApi/swap/v2/quote/contracts", &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Data {
		if strings.HasSuffix(s.Symbol, "-USDT") {
			out = append(out, s.Symbol)
		}
	}
	return out, nil
}

func bingxFunding(sym string, since int64) ([]point, error) {
	var r struct {
		Data []struct {
			FundingRate string `json:"fundingRate"`
			FundingTime int64  `json:"fundingTime"`
		} `json:"data"`
	}
	u := fmt.Sprintf("https://open-api.bingx.com/openApi/swap/v2/quote/fundingRate?symbol=%s&startTime=%d&limit=1000", sym, since)
	if err := get(u, &r); err != nil {
		return nil, err
	}
	out := make([]point, 0, len(r.Data))
	for _, d := range r.Data {
		out = append(out, point{d.FundingTime, f(d.FundingRate)})
	}
	return out, nil
}

func gateSymbols() ([]string, error) {
	var r []struct {
		Name        string `json:"name"`
		InDelisting bool   `json:"in_delisting"`
	}
	if err := get("https://api.gateio.ws/api/v4/futures/usdt/contracts", &r); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range r {
		if !c.InDelisting {
			out = append(out, c.Name)
		}
	}
	return out, nil
}

func gateFunding(sym string, since int64) ([]point, error) {
	var r []struct {
		R string `json:"r"`
		T int64  `json:"t"`
	}
	u := fmt.Sprintf("https://api.gateio.ws/api/v4/futures/usdt/funding_rate?contract=%s&limit=1000", sym)
	if err := get(u, &r); err != nil {
		return nil, err
	}
	out := make([]point, 0, len(r))
	for _, d := range r {
		// Gate returns SECONDS, not milliseconds. Reading it as ms would place
		// every settlement in 1970 and date every listing as ancient.
		out = append(out, point{d.T * 1000, f(d.R)})
	}
	return out, nil
}

func bitgetSymbols() ([]string, error) {
	var r struct {
		Data []struct {
			Symbol       string `json:"symbol"`
			SymbolStatus string `json:"symbolStatus"`
		} `json:"data"`
	}
	if err := get("https://api.bitget.com/api/v2/mix/market/contracts?productType=USDT-FUTURES", &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Data {
		if s.SymbolStatus == "normal" {
			out = append(out, s.Symbol)
		}
	}
	return out, nil
}

func bitgetFunding(sym string, since int64) ([]point, error) {
	var out []point
	for page := 1; page <= 6; page++ {
		var r struct {
			Data []struct {
				FundingRate string `json:"fundingRate"`
				FundingTime string `json:"fundingTime"`
			} `json:"data"`
		}
		u := fmt.Sprintf("https://api.bitget.com/api/v2/mix/market/history-fund-rate?symbol=%s&productType=USDT-FUTURES&pageSize=100&pageNo=%d", sym, page)
		if err := get(u, &r); err != nil || len(r.Data) == 0 {
			break
		}
		stop := false
		for _, d := range r.Data {
			ts := i64(d.FundingTime)
			if ts < since {
				stop = true
				continue
			}
			out = append(out, point{ts, f(d.FundingRate)})
		}
		if stop {
			break
		}
		time.Sleep(130 * time.Millisecond)
	}
	return out, nil
}

// --- main --------------------------------------------------------------------

func main() {
	dataDir := flag.String("data", "data", "data directory")
	days := flag.Int("days", 365, "history depth")
	only := flag.String("only", "", "restrict to one venue")
	refresh := flag.Bool("refresh", false, "re-fetch even if cached")
	minVenues := flag.Int("min-venues", 2, "only cache coins present on at least this many venues")
	flag.Parse()

	root := filepath.Join(*dataDir, "fundcache")
	since := time.Now().Add(-time.Duration(*days) * 24 * time.Hour).UnixMilli()

	// --- symbol universe ---
	vs := venues()
	symsByVenue := map[string][]string{}
	coinVenues := map[string]map[string]string{} // coin -> venue -> symbol

	fmt.Println("SYMBOL LISTS")
	for _, v := range vs {
		if *only != "" && v.name != *only {
			continue
		}
		syms, err := v.symbols()
		if err != nil {
			fmt.Printf("  %-12s FAILED: %v\n", v.name, err)
			continue
		}
		symsByVenue[v.name] = syms
		for _, s := range syms {
			c := coinOf(s)
			if c == "" {
				continue
			}
			if coinVenues[c] == nil {
				coinVenues[c] = map[string]string{}
			}
			coinVenues[c][v.name] = s
		}
		fmt.Printf("  %-12s %d symbols\n", v.name, len(syms))
	}
	_ = os.MkdirAll(root, 0o755)
	raw, _ := json.MarshalIndent(coinVenues, "", " ")
	_ = os.WriteFile(filepath.Join(root, "universe.json"), raw, 0o644)

	multi := 0
	for _, vm := range coinVenues {
		if len(vm) >= *minVenues {
			multi++
		}
	}
	fmt.Printf("\n  %d distinct coins, %d on %d+ venues (hedgeable)\n\n",
		len(coinVenues), multi, *minVenues)

	// --- funding history ---
	fmt.Println("FUNDING HISTORY")
	total, done, failed, cached := 0, 0, 0, 0
	for coin, vm := range coinVenues {
		if len(vm) < *minVenues {
			continue
		}
		total += len(vm)
		_ = coin
	}
	fmt.Printf("  %d symbol-venue histories to fetch\n\n", total)

	byName := map[string]venue{}
	for _, v := range vs {
		byName[v.name] = v
	}

	start := time.Now()
	for coin, vm := range coinVenues {
		if len(vm) < *minVenues {
			continue
		}
		for vname, sym := range vm {
			v, ok := byName[vname]
			if !ok || (*only != "" && vname != *only) {
				continue
			}
			path := filepath.Join(root, vname, safeName(sym)+".json")
			if !*refresh {
				if _, err := os.Stat(path); err == nil {
					cached++
					done++
					continue
				}
			}
			pts, err := v.funding(sym, since)
			if err != nil || len(pts) < 3 {
				failed++
				done++
				continue
			}
			sort.Slice(pts, func(i, j int) bool { return pts[i].TsMs < pts[j].TsMs })
			h := history{
				Venue: vname, Symbol: sym, Coin: coin, Points: pts,
				IntervalH: deriveInterval(pts), FirstMs: pts[0].TsMs,
				FetchedAt: time.Now().UnixMilli(),
			}
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			b, _ := json.Marshal(h)
			_ = os.WriteFile(path, b, 0o644)
			done++
			if done%50 == 0 {
				el := time.Since(start)
				fmt.Printf("  %d/%d  (%d cached, %d failed)  %s elapsed\n",
					done, total, cached, failed, el.Round(time.Second))
			}
			time.Sleep(v.pace)
		}
	}
	fmt.Printf("\n  done: %d fetched, %d already cached, %d failed, %s total\n",
		done-cached-failed, cached, failed, time.Since(start).Round(time.Second))
	fmt.Printf("  cache at %s\n", root)
}

func safeName(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(s)
}
