// Command paperrebuild recomputes cash-and-carry P&L from each venue's OWN
// record of what actually settled.
//
// # WHY IT EXISTS
//
// pkg/funding/paper.go credited the funding rate observed AFTER a settlement
// boundary to the interval that had just ended, and counted one settlement
// however many had elapsed. Both defects were found in pkg/crossvenue on
// 17 August and fixed there; nobody checked for the same shape here until an
// external code review on 19 August. Every cash-and-carry figure this project
// has quoted -- including 2.9%/yr -- was produced by that arithmetic.
//
// # WHY IT DOES NOT REPLAY THE JOURNAL
//
// data/journal has a full 5-minute observation history back to 5 August, but
// the records carry no next-funding timestamp, so settlement boundaries would
// have to be INFERRED from UTC alignment. That is exactly the class of
// assumption this project refuses. Venues publish the settled rate at the
// settled timestamp; that is authoritative and costs a handful of requests.
//
// Read-only. Touches no live state.
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
	"time"
)

type position struct {
	ID                  string    `json:"id"`
	Venue               string    `json:"venue"`
	Symbol              string    `json:"symbol"`
	OpenedAt            time.Time `json:"opened_at"`
	ClosedAt            time.Time `json:"closed_at"`
	IntervalHours       float64   `json:"interval_hours"`
	NotionalUSD         float64   `json:"notional_usd"`
	CapitalUSD          float64   `json:"capital_usd"`
	EntryCostBps        float64   `json:"entry_cost_bps"`
	ExitCostBps         float64   `json:"exit_cost_bps"`
	FundingCollectedBps float64   `json:"funding_collected_bps"`
	IntervalsCollected  int       `json:"intervals_collected"`
}

type book struct {
	Open   map[string]position `json:"open"`
	Closed []position          `json:"closed"`
}

type settlement struct {
	tsMs int64
	bps  float64
}

func get(url string, out any) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "vega-paperrebuild")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %.120s", resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

// settled fetches the venue's actual settlement record for a window.
func settled(venue, symbol string, fromMs, toMs int64) ([]settlement, error) {
	var out []settlement
	switch venue {
	case "binance":
		var rows []struct {
			FundingRate string `json:"fundingRate"`
			FundingTime int64  `json:"fundingTime"`
		}
		u := fmt.Sprintf("https://fapi.binance.com/fapi/v1/fundingRate?symbol=%s&startTime=%d&endTime=%d&limit=1000",
			symbol, fromMs, toMs)
		if err := get(u, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			var f float64
			fmt.Sscanf(r.FundingRate, "%g", &f)
			out = append(out, settlement{r.FundingTime, f * 10000})
		}
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
		u := fmt.Sprintf("https://api.bybit.com/v5/market/funding/history?category=linear&symbol=%s&startTime=%d&endTime=%d&limit=200",
			symbol, fromMs, toMs)
		if err := get(u, &env); err != nil {
			return nil, err
		}
		if env.RetCode != 0 {
			return nil, fmt.Errorf("bybit retCode %d", env.RetCode)
		}
		for _, r := range env.Result.List {
			var f float64
			var ts int64
			fmt.Sscanf(r.FundingRate, "%g", &f)
			fmt.Sscanf(r.FundingRateTimestamp, "%d", &ts)
			out = append(out, settlement{ts, f * 10000})
		}
	default:
		return nil, fmt.Errorf("no settlement fetcher for %s", venue)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].tsMs < out[j].tsMs })
	return out, nil
}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	flag.Parse()

	raw, err := os.ReadFile(filepath.Join(*dataDir, "positions.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var bk book
	if err := json.Unmarshal(raw, &bk); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var all []position
	for _, p := range bk.Open {
		all = append(all, p)
	}
	all = append(all, bk.Closed...)
	sort.Slice(all, func(i, j int) bool { return all[i].OpenedAt.Before(all[j].OpenedAt) })

	fmt.Printf("CASH-AND-CARRY REBUILD FROM VENUE SETTLEMENT RECORDS\n\n")
	fmt.Printf("%-22s %-9s %6s %9s %9s %9s %8s %8s\n",
		"POSITION", "STATE", "HELD d", "RECORDED", "ACTUAL", "DELTA", "N rec", "N act")
	fmt.Println("--------------------------------------------------------------------------------------------")

	var recTotalUSD, actTotalUSD, capital float64
	var earliest time.Time
	for _, p := range all {
		end := p.ClosedAt
		state := "closed"
		if end.IsZero() || end.Year() < 2000 {
			end = time.Now().UTC()
			state = "open"
		}
		ss, err := settled(p.Venue, p.Symbol, p.OpenedAt.UnixMilli(), end.UnixMilli())
		if err != nil {
			fmt.Printf("%-22s %-9s  %v\n", p.ID, state, err)
			continue
		}
		actual, n := 0.0, 0
		for _, s := range ss {
			if s.tsMs > p.OpenedAt.UnixMilli() && s.tsMs <= end.UnixMilli() {
				actual += s.bps
				n++
			}
		}
		held := end.Sub(p.OpenedAt).Hours() / 24
		recUSD := p.FundingCollectedBps / 10000 * p.NotionalUSD
		actUSD := actual / 10000 * p.NotionalUSD
		recTotalUSD += recUSD
		actTotalUSD += actUSD
		capital += p.CapitalUSD
		if earliest.IsZero() || p.OpenedAt.Before(earliest) {
			earliest = p.OpenedAt
		}
		fmt.Printf("%-22s %-9s %6.1f %+9.3f %+9.3f %+9.3f %8d %8d\n",
			p.ID, state, held, p.FundingCollectedBps, actual,
			actual-p.FundingCollectedBps, p.IntervalsCollected, n)
		time.Sleep(150 * time.Millisecond)
	}

	fmt.Printf("\nTOTALS\n\n")
	fmt.Printf("  capital deployed        $%.0f\n", capital)
	fmt.Printf("  funding as RECORDED     $%+.4f\n", recTotalUSD)
	fmt.Printf("  funding as SETTLED      $%+.4f\n", actTotalUSD)
	fmt.Printf("  difference              $%+.4f\n", actTotalUSD-recTotalUSD)
	if !earliest.IsZero() && capital > 0 {
		days := time.Since(earliest).Hours() / 24
		fmt.Printf("\n  over %.1f days, on capital, BEFORE entry and exit costs:\n", days)
		fmt.Printf("    recorded pace         %+.2f%%/yr\n", recTotalUSD/capital*(365/days)*100)
		fmt.Printf("    ACTUAL pace           %+.2f%%/yr\n", actTotalUSD/capital*(365/days)*100)
	}
	fmt.Print(`
Funding only. Entry and exit costs are excluded here deliberately -- they are
recorded correctly and were never in question. The figure to compare against
the 2.9%/yr previously quoted is the ACTUAL pace above, less costs.

N rec vs N act: a mismatch means settlements were miscounted, not just
mispriced.
`)
}
