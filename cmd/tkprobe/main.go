package main

import (
	"context"
	"fmt"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

func main() {
	ctx, c := context.WithTimeout(context.Background(), 60*time.Second)
	defer c()
	mx := orderbook.NewMEXCPerp()
	if err := mx.LoadSymbols(ctx); err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Println("symbols:", mx.SymbolCount())
	for _, coin := range []string{"MOVE", "RED", "COW", "BTC", "1000000MOG"} {
		sym, ok := mx.ResolveCoin(coin)
		if !ok {
			fmt.Printf("  %-12s ResolveCoin FAILED\n", coin)
			continue
		}
		bps, ok2 := mx.TakerBps(sym)
		fmt.Printf("  %-12s -> %-14s taker %.1f bps  ok=%v\n", coin, sym, bps, ok2)
	}
}
