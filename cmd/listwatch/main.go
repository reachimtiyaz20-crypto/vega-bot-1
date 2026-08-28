// Command listwatch records newly listed perps and tracks their funding.
//
// # WHY
//
// A trader described running funding arbitrage on new listings at ~37% a year.
// Measured against Binance's own history on 2026-08-16, the mechanism holds:
// ~11 listings a month, ~40% showing |funding| above 50%/yr in week one, and
// 16 of 18 existing on a second venue within days -- so they are hedgeable.
//
// But the window is ONE WEEK, and VEGA has no concept of a listing date. By
// the time a symbol clears the $10m volume floor its window has closed. The
// only way to test this is to WATCH FORWARD.
//
// # WHAT IT DOES NOT DO
//
// It opens nothing, changes no gate, and touches neither paper book. It writes
// three files and exits. A venue that breaks costs a missing log line.
//
// WHAT IT RECORDS
//
//	symbols-<venue>.json   the symbol set, so the NEXT run can diff it
//	events.jsonl           one line per newly detected symbol
//	track.jsonl            hourly: funding and book depth on every venue that
//	                       lists a coin younger than -track-days
//
// From track.jsonl you can compute what a hedged position opened N hours after
// listing would have earned -- which is the strategy, measured.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/hyperliquid"
	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

type venueSet struct {
	Venue   string          `json:"venue"`
	At      time.Time       `json:"at"`
	Symbols map[string]bool `json:"symbols"`
}

type event struct {
	TsMs   int64  `json:"ts_ms"`
	Venue  string `json:"venue"`
	Symbol string `json:"symbol"`
	Coin   string `json:"coin"`
	// FirstRun marks the very first snapshot, where EVERY symbol looks new.
	// Without it the first run would record 3,000 false listings.
	FirstRun bool `json:"first_run"`
}

type track struct {
	TsMs      string  `json:"-"`
	Ts        int64   `json:"ts_ms"`
	Coin      string  `json:"coin"`
	Venue     string  `json:"venue"`
	Symbol    string  `json:"symbol"`
	AgeHours  float64 `json:"age_hours"`
	BpsPerHr  float64 `json:"funding_bps_per_hr"`
	IntervalH float64 `json:"interval_hours"`
	BidUSD    float64 `json:"bid_depth_usd"`
	AskUSD    float64 `json:"ask_depth_usd"`
	SpreadBps float64 `json:"spread_bps"`
	RT400Bps  float64 `json:"round_trip_400_bps"`
	Fillable  bool    `json:"fillable_400"`
}

func main() {
	dataDir := flag.String("data", "data", "data directory")
	trackDays := flag.Float64("track-days", 14, "how long to follow a new listing")
	notional := flag.Float64("notional", 400, "size to test fillability at")
	verbose := flag.Bool("v", false, "print every tracked observation")
	flag.Parse()

	dir := filepath.Join(*dataDir, "listings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	now := time.Now().UTC()

	// --- current symbol sets ---
	readers := orderbook.Readers()
	current := map[string]map[string]bool{}

	for name, r := range readers {
		if err := r.LoadSymbols(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING %s: %v\n", name, err)
			continue
		}
		set := map[string]bool{}
		// THE VENUE'S OWN LIST. A universe borrowed from another venue cannot
		// see a listing that has only happened here yet, which is the point.
		for _, sym := range r.Symbols() {
			set[sym] = true
		}
		current[name] = set
	}

	hl := hyperliquid.New()
	rates, _, err := hl.PredictedFundings(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL hyperliquid: %v\n", err)
		os.Exit(1)
	}
	coins := make([]string, 0, len(rates))
	for c := range rates {
		coins = append(coins, c)
	}
	sort.Strings(coins)

	current["hyperliquid"] = map[string]bool{}
	for _, c := range coins {
		current["hyperliquid"][c] = true
	}

	// --- diff against last time ---
	var events []event
	firstEver := false

	for venue, set := range current {
		path := filepath.Join(dir, "symbols-"+venue+".json")
		var prev venueSet
		raw, err := os.ReadFile(path)
		isFirst := err != nil
		if !isFirst {
			if json.Unmarshal(raw, &prev) != nil || len(prev.Symbols) == 0 {
				isFirst = true
			}
		}
		if isFirst {
			firstEver = true
		}
		for sym := range set {
			if !isFirst && prev.Symbols[sym] {
				continue
			}
			events = append(events, event{
				TsMs: now.UnixMilli(), Venue: venue, Symbol: sym,
				Coin: coinOf(sym), FirstRun: isFirst,
			})
		}
		_ = writeJSON(path, venueSet{Venue: venue, At: now, Symbols: set})
	}

	if len(events) > 0 {
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), events); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING events: %v\n", err)
		}
	}

	newReal := 0
	for _, e := range events {
		if !e.FirstRun {
			newReal++
		}
	}
	if firstEver {
		fmt.Printf("FIRST RUN: recorded %d symbols as the baseline. None are new listings --\n"+
			"the next run is the first that can tell.\n", len(events))
	} else {
		fmt.Printf("new symbols since last run: %d\n", newReal)
		for _, e := range events {
			if !e.FirstRun {
				fmt.Printf("  NEW  %-14s on %s\n", e.Symbol, e.Venue)
			}
		}
	}

	// --- track everything young ---
	firstSeen := loadFirstSeen(filepath.Join(dir, "events.jsonl"))
	var rows []track
	cutoff := now.Add(-time.Duration(*trackDays*24) * time.Hour)

	for coin, fs := range firstSeen {
		if fs.Before(cutoff) {
			continue
		}
		age := now.Sub(fs).Hours()

		if r, ok := rates[coin]; ok {
			for _, vr := range r {
				if !vr.Known() {
					continue
				}
				rows = append(rows, track{
					Ts: now.UnixMilli(), Coin: coin, Venue: vr.Venue, Symbol: coin,
					AgeHours: age, BpsPerHr: vr.BpsPerHour, IntervalH: vr.IntervalHours,
				})
			}
		}
		for name, r := range readers {
			sym, ok := r.ResolveCoin(coin)
			if !ok {
				continue
			}
			b, err := r.Book(ctx, sym)
			if err != nil || !b.Measured {
				continue
			}
			bid, ask, _, _ := b.TopOfBookUSD()
			sp, _ := b.SpreadBps()
			rt, fill := b.RoundTripSlippageBps(*notional)
			rows = append(rows, track{
				Ts: now.UnixMilli(), Coin: coin, Venue: name, Symbol: sym,
				AgeHours: age, BidUSD: bid, AskUSD: ask, SpreadBps: sp,
				RT400Bps: rt, Fillable: fill,
			})
			time.Sleep(60 * time.Millisecond)
		}
	}

	if len(rows) > 0 {
		if err := appendJSONL(filepath.Join(dir, "track.jsonl"), rows); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING track: %v\n", err)
		}
	}
	fmt.Printf("tracking %d coins younger than %.0f days, %d observations written\n",
		countCoins(rows), *trackDays, len(rows))

	if *verbose {
		for _, r := range rows {
			fmt.Printf("  %-10s %-12s age %6.1fh  funding %+8.4f bps/hr  rt@400 %6.2f  fill=%v\n",
				r.Coin, r.Venue, r.AgeHours, r.BpsPerHr, r.RT400Bps, r.Fillable)
		}
	}
}

func coinOf(sym string) string {
	s := strings.ToUpper(sym)
	for _, suf := range []string{"-USDT-SWAP", "USDT", "-USD"} {
		s = strings.TrimSuffix(s, suf)
	}
	return s
}

func countCoins(rows []track) int {
	m := map[string]bool{}
	for _, r := range rows {
		m[r.Coin] = true
	}
	return len(m)
}

func loadFirstSeen(path string) map[string]time.Time {
	out := map[string]time.Time{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var e event
		if err := dec.Decode(&e); err != nil {
			break
		}
		if e.FirstRun || e.Coin == "" {
			continue
		}
		t := time.UnixMilli(e.TsMs).UTC()
		if cur, ok := out[e.Coin]; !ok || t.Before(cur) {
			out[e.Coin] = t
		}
	}
	return out
}

func writeJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendJSONL(path string, rows any) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	switch v := rows.(type) {
	case []event:
		for _, r := range v {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
	case []track:
		for _, r := range v {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
	}
	return nil
}
