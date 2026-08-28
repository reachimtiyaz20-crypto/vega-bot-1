// Command majors runs the static long-basis book on deep coins, on paper.
//
// PAPER ONLY. There is no order placement anywhere in this binary, and adding
// it should be a separate, deliberate change reviewed on its own.
//
// Measured over five years of settled Binance funding (2021-08 to 2026-08),
// six majors, costs charged at 33 bps a round trip:
//
//	static long, 5x              18.7%/yr   29.0% max drawdown
//	flat<-0.10 for 14d, 5x       22.8%/yr   15.9% max drawdown
//	flat<-0.10 for 14d, 10x      48.4%/yr   30.1% max drawdown
//
// The de-risk rule beat holding on both return and drawdown at every leverage
// and across the whole parameter neighbourhood. But the entire benefit comes
// from sidestepping THREE crises in five years, and n=3 cannot distinguish a
// working rule from a lucky one. So this ships at 5x and stays there until the
// rule has fired live at least once.
//
// NO CAPITAL LEDGER, deliberately. The other books hunt many positions and
// need a hard ceiling to stop them overcommitting. This one holds a single
// basket of fixed notional; the ceiling IS the -notional flag, and a ledger
// enforcing what the config already fixes would be ceremony rather than
// safety.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/exchange"
	"github.com/imtiyaz/vega-bot/pkg/journal"
	"github.com/imtiyaz/vega-bot/pkg/majors"
)

func main() {
	dataDir := flag.String("data", "data/majors",
		"directory for majors_state.json and journal/")
	poll := flag.Duration("poll", 5*time.Minute, "how often to read funding")
	basket := flag.String("basket", "BTC,ETH,SOL,BNB,XRP,DOGE",
		"base assets to hold, comma separated")
	quote := flag.String("quote", "USDT", "quote currency")
	venue := flag.String("venue", "binance",
		"venue to trade on. One venue: spot and perp must share a margin pool, "+
			"and margin cannot net across exchanges")
	notional := flag.Float64("notional", 1000, "total basket notional in USD")
	leverage := flag.Float64("leverage", 5,
		"notional per dollar of capital. MODELLED -- no margin is posted. "+
			"Running this live needs unified margin with spot hedging enabled, "+
			"or the perp leg liquidates on price moves while the spot leg watches")
	trail := flag.Int("trail-days", 14,
		"window for the de-risk signal. 7 and 30 also beat holding, so 14 is a "+
			"choice inside a working range rather than a fitted parameter")
	exitBps := flag.Float64("exit-bps-hr", -0.10,
		"go flat when trailing funding falls below this")
	reenterBps := flag.Float64("reenter-bps-hr", 0.0,
		"return only once trailing funding climbs back above this. The gap from "+
			"-exit-bps-hr is hysteresis; without it the book oscillates and pays for it")
	roundTrip := flag.Float64("round-trip-bps", 33.0,
		"cost charged on notional at every entry and exit")
	minTop := flag.Float64("min-spot-top", 500,
		"refuse a symbol whose spot touch cannot carry its share")
	minVol := flag.Float64("min-vol", 10_000_000, "minimum 24h volume, both legs")
	touchFrac := flag.Float64("touch-fraction", 0.25,
		"share of a symbol's notional that must rest at the spot touch. 1.0 "+
			"demands the whole position instantly, which is wrong for a 30-day "+
			"hold accumulated over hours -- at 1.0 it refused BNB and XRP")
	fallbackSlip := flag.Float64("fallback-slip", 2,
		"slippage assumed when the book cannot be read")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)

	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		logger.Fatalf("FATAL resolving -data: %v", err)
	}

	cfg := majors.DefaultConfig()
	cfg.Basket = splitNonEmpty(*basket)
	cfg.Quote = *quote
	cfg.Venue = *venue
	cfg.NotionalUSD = *notional
	cfg.Leverage = *leverage
	cfg.TrailDays = *trail
	cfg.ExitBpsHr = *exitBps
	cfg.ReenterBpsHr = *reenterBps
	cfg.RoundTripBps = *roundTrip
	cfg.MinSpotTopUSD = *minTop
	cfg.MinVol24hUSD = *minVol
	cfg.TouchFraction = *touchFrac

	if len(cfg.Basket) == 0 {
		logger.Fatalf("FATAL -basket is empty")
	}

	book, err := majors.New(abs, cfg)
	if err != nil {
		logger.Fatalf("FATAL opening majors book: %v", err)
	}

	j, err := journal.Open(filepath.Join(abs, "journal"))
	if err != nil {
		logger.Fatalf("FATAL opening journal: %v", err)
	}
	defer j.Close()

	reg := exchange.NewRegistry(
		exchange.NewBinance(*fallbackSlip),
		exchange.NewBybit(*fallbackSlip),
	)

	logger.Printf("VEGA majors -- PAPER MEASUREMENT ONLY, no order placement exists in this binary")
	syms := make([]string, 0, len(cfg.Basket))
	for _, base := range cfg.Basket {
		syms = append(syms, cfg.Symbol(base))
	}
	logger.Printf("basket: %s on %s", strings.Join(syms, " "), cfg.Venue)
	logger.Printf("policy: $%.0f notional at %gx (capital $%.0f), flat below %.3f bps/hr "+
		"over %d complete days, back in above %.3f, round trip %.0f bps",
		cfg.NotionalUSD, cfg.Leverage, cfg.NotionalUSD/cfg.Leverage,
		cfg.ExitBpsHr, cfg.TrailDays, cfg.ReenterBpsHr, cfg.RoundTripBps)
	logger.Printf("state: %s", book.Summary())
	logger.Printf("journal: %s", filepath.Join(abs, "journal"))

	// Record the configuration in the journal so any run can be reproduced
	// from its own record rather than from whatever the unit file says today.
	_ = j.Write(map[string]any{
		"type": "majors_config", "ts_ms": time.Now().UTC().UnixMilli(),
		"config": cfg,
	})

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	t := time.NewTicker(*poll)
	defer t.Stop()

	run := func() {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		raw, errs := reg.CollectAll(cctx)
		for venueName, e := range errs {
			if e != nil {
				// A venue that failed is not a venue with no funding. Say so
				// and let the poll proceed on what did arrive.
				logger.Printf("WARN %s: %v", venueName, e)
			}
		}
		obs := make([]majors.Obs, 0, len(raw))
		for _, o := range raw {
			obs = append(obs, majors.Obs{
				Venue:             o.Venue,
				Symbol:            o.Symbol,
				FundingRatePct:    o.FundingRatePct,
				IntervalHours:     o.IntervalHours,
				SpotAvailable:     o.SpotSymbolAvailable,
				LiquidityMeasured: o.LiquidityMeasured,
				SpotHalfSpreadBps: o.SpotHalfSpreadBps,
				PerpHalfSpreadBps: o.PerpHalfSpreadBps,
				SpotTopUSD:        o.SpotTopOfBookUSD,
				PerpTopUSD:        o.PerpTopOfBookUSD,
				SpotVol24hUSD:     o.SpotQuoteVolume24hUSD,
				PerpVol24hUSD:     o.PerpQuoteVolume24hUSD,
			})
		}

		res := book.Poll(obs, time.Now().UTC())
		if err := j.Write(res); err != nil {
			logger.Printf("WARN journal write: %v", err)
		}
		if err := book.Save(); err != nil {
			// A book that cannot persist will silently restart from stale
			// state. Loud, every time.
			logger.Printf("ERROR saving majors state: %v", err)
		}

		if res.Changed != "" {
			logger.Printf("*** %s: trailing %.4f bps/hr over %d days, basket now %.4f ***",
				res.Changed, res.TrailBps, res.TrailDays, res.BasketBps)
		}
		pos := "FLAT"
		if res.Invested {
			pos = "LONG"
		}
		logger.Printf("poll: %d usable, %d refused, basket %+.4f bps/hr, "+
			"trail %+.4f (%d/%d days), %s, net %+.2f bps = %+.3f%% on capital",
			len(res.Usable), len(res.Refused), res.BasketBps, res.TrailBps,
			res.TrailDays, cfg.TrailDays, pos, res.NetBps, res.ReturnPct)
		if len(res.Refused) > 0 && len(res.Usable) < len(cfg.Basket) {
			logger.Printf("  refused: %v", res.Refused)
		}
	}

	run()
	for {
		select {
		case <-ctx.Done():
			if err := book.Save(); err != nil {
				logger.Printf("ERROR saving on shutdown: %v", err)
			}
			if err := j.Flush(); err != nil {
				logger.Printf("ERROR flushing journal: %v", err)
			}
			logger.Printf("shutdown clean: %s", book.Summary())
			return
		case <-t.C:
			run()
		}
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(strings.ToUpper(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
