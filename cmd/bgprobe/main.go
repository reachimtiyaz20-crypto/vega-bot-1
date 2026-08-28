// Command bgprobe verifies the Bitget client against the live venue.
//
// It exists to check the two things that have broken before: that the funding
// interval genuinely varies per symbol, and that book sizes are base units
// rather than contracts.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b := orderbook.NewBitgetPerp()
	if err := b.LoadSymbols(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	fmt.Printf("bitget: %d instruments with a readable funding interval\n\n", b.SymbolCount())
	fmt.Printf("%-8s %-14s %9s %10s %11s %13s\n",
		"COIN", "SYMBOL", "INTERVAL", "bps/hr", "SPREAD bps", "TOP OF BOOK")
	for _, coin := range []string{"BTC", "ETH", "KAITO", "SOL", "DOGE"} {
		sym, ok := b.ResolveCoin(coin)
		if !ok {
			fmt.Printf("%-8s not listed\n", coin)
			continue
		}
		fi := b.FundingIntervalHours(sym)
		f, _ := b.Funding(sym)
		bk, err := b.Book(ctx, sym)
		if err != nil {
			fmt.Printf("%-8s %-14s book error: %v\n", coin, sym, err)
			continue
		}
		sp, _ := bk.SpreadBps()
		_, _, top, _ := bk.TopOfBookUSD()
		mn, _ := b.MinNotionalUSD(sym)
		fmt.Printf("%-8s %-14s %8.0fh %10.4f %11.2f %12.0f  min $%.0f\n",
			coin, sym, fi.Hours, f.BpsPerHour(), sp, top, mn)
		if coin == "BTC" && top > 50000000 {
			fmt.Println("  REFUSE: BTC top-of-book over $50M means sizes are CONTRACTS, not base units")
		}
		time.Sleep(150 * time.Millisecond)
	}
}
