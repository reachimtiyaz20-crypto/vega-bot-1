// Command settledprobe verifies every settled-history fetcher against the live
// venue before any of them is allowed near an entry decision.
//
// Five venues, five API shapes, five sets of field names. A parser that
// silently returns zeros would replace an over-optimistic signal with a blind
// one, which is worse.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/crossvenue"
)

func main() {
	venue := flag.String("venue", "", "probe this venue only; empty runs the built-in cross-venue check")
	symbol := flag.String("symbol", "", "venue-native symbol, e.g. ONGUSDT or ONG-USDT-SWAP")
	ivl := flag.Float64("ivl", 0, "settlement interval hours; REQUIRED with -venue. An unknown interval is a refusal, not a guess.")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Intervals are supplied explicitly. Passing nil lets summarise default to 8
	// hours, which silently divides Lighter's HOURLY rates by eight -- a
	// diagnostic that lies is worse than no diagnostic.
	cases := []struct {
		venue, symbol string
		ivl           float64
	}{
		{"binance", "BTCUSDT", 8},
		{"binance", "KAITOUSDT", 4},
		{"bybit", "BTCUSDT", 8},
		{"bybit", "KAITOUSDT", 4},
		{"okx", "BTC-USDT-SWAP", 8},
		{"bitget", "BTCUSDT", 8},
		{"bitget", "KAITOUSDT", 4},
		{"mexc", "BTC_USDT", 8},
		{"mexc", "RED_USDT", 4},
		{"lighter", "BTC", 1},
		{"lighter", "ETH", 1},
		{"lighter", "KAITO", 1},
		{"hyperliquid", "BTC", 1},
		{"hyperliquid", "ETH", 1},
		{"hyperliquid", "KAITO", 1},
	}

	if *venue != "" || *symbol != "" {
		if *venue == "" || *symbol == "" || *ivl <= 0 {
			fmt.Fprintln(os.Stderr, "-venue, -symbol and -ivl must be given together")
			os.Exit(2)
		}
		cases = []struct {
			venue, symbol string
			ivl           float64
		}{{*venue, *symbol, *ivl}}
	}

	c := crossvenue.NewSettledCache(time.Hour, "")
	fmt.Printf("%-9s %-16s %8s %10s %10s %11s %22s\n",
		"VENUE", "SYMBOL", "SETTLES", "bps/hr", "RECENT", "SAME SIGN", "LAST SETTLEMENT")
	fmt.Println("--------------------------------------------------------------------------------")
	for _, k := range cases {
		f, bad := c.Ensure(ctx, k.venue, []string{k.symbol}, func(string) float64 { return k.ivl }, 30*time.Second, 100*time.Millisecond)
		r, ok := c.Get(k.venue, k.symbol)
		if !ok || f == 0 {
			fmt.Printf("%-9s %-16s   FAILED (%d fetched, %d failed)\n", k.venue, k.symbol, f, bad)
			continue
		}
		last := time.UnixMilli(r.LastSettleMs).UTC().Format("2006-01-02 15:04")
		fmt.Printf("%-9s %-16s %8d %10.4f %10.4f %10.0f%% %22s\n",
			k.venue, k.symbol, r.Intervals, r.BpsPerHour, r.RecentBpsPerHour, r.SameSignFrac*100, last)
	}
	fmt.Print(`
SETTLES should be 12 where the venue has that much history.
bps/hr must be plausible: BTC sits near 0.01-0.15 on most venues.
LAST SETTLEMENT must be recent -- a date from weeks ago means the parser read
the wrong field or the venue paginates oldest-first.
`)
}
