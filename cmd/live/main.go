// Command live is VEGA's real-money process.
//
// It has three modes, and they are deliberately ordered so that the safest one
// is what you get by doing nothing:
//
//	WATCH      (default)  read-only. Snapshots accounts, assesses liquidation
//	                      risk, reconciles PnL against the venue ledger, and
//	                      journals all of it. Places NO orders. Runs happily
//	                      with read-only API keys.
//
//	TESTNET    -enable    places real orders against the exchange's testnet.
//	                      No money at risk. Proves the mechanics: right side,
//	                      right size, right symbol, correct error handling.
//	                      Proves NOTHING about fill quality -- testnet books
//	                      are thin and fake.
//
//	MAINNET    -enable -mode mainnet
//	                      real money. Refused entirely while the venue order
//	                      shapes are unverified.
//
// The safety checks live in pkg/live, not here. This file's job is to state
// clearly what is about to happen and then get out of the way.
//
//	live -data ~/vega-bot/data                          # watch only
//	live -data ~/vega-bot/data -enable                  # testnet trading
//	live -data ~/vega-bot/data -enable -open BTCUSDT    # testnet, open one
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/exchange"
	"github.com/imtiyaz/vega-bot/pkg/execution"
	"github.com/imtiyaz/vega-bot/pkg/journal"
	"github.com/imtiyaz/vega-bot/pkg/live"
	"github.com/imtiyaz/vega-bot/pkg/risk"
)

func main() {
	dataDir := flag.String("data", "", "data directory (required)")
	modeFlag := flag.String("mode", "testnet", "testnet or mainnet")
	enable := flag.Bool("enable", false, "permit order placement; without this the process is READ-ONLY")
	venue := flag.String("venue", "binance", "venue to operate on")
	openSymbol := flag.String("open", "", "open ONE hedge on this symbol at startup, then keep monitoring")
	notional := flag.Float64("notional", 50, "USD per leg (deployed capital is roughly double)")
	maxPos := flag.Int("max-positions", 1, "maximum concurrent hedges")
	maxSlip := flag.Float64("max-slip", 15, "abort a position if the first leg slips more than this many bps")
	poll := flag.Duration("poll", 5*time.Minute, "risk and reconciliation interval")
	killSwitch := flag.String("kill-switch", "/etc/vega/HALT", "if this file exists, no position will be opened")
	closeAll := flag.Bool("close-all", false, "close every tracked position on shutdown")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	if *dataDir == "" {
		logger.Fatal("FATAL -data is required")
	}
	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		logger.Fatalf("FATAL resolving -data: %v", err)
	}

	mode := execution.Testnet
	switch strings.ToLower(*modeFlag) {
	case "testnet":
	case "mainnet":
		mode = execution.Mainnet
	default:
		logger.Fatalf("FATAL -mode must be testnet or mainnet, got %q", *modeFlag)
	}

	j, err := journal.Open(filepath.Join(abs, "journal"))
	if err != nil {
		logger.Fatalf("FATAL opening journal: %v", err)
	}
	defer j.Close()

	// --- credentials ---------------------------------------------------------
	//
	// The READER always uses ReadOnly capability, even when trading is enabled.
	// That is not redundant: it means the reconciliation path physically cannot
	// sign a state-changing request, so a bug in reconciliation cannot move
	// money. Only the trader gets Trade capability, and only when -enable is
	// set.

	readCreds, err := execution.FromEnv(*venue, execution.ReadOnly, mode)
	if err != nil {
		logger.Fatalf("FATAL %v", err)
	}

	readers := map[string]execution.AccountReader{}
	traders := map[string]live.Trader{}

	switch strings.ToLower(*venue) {
	case "binance":
		r, err := execution.NewBinanceAccount(readCreds)
		if err != nil {
			logger.Fatalf("FATAL building binance reader: %v", err)
		}
		readers["binance"] = r
	case "bybit":
		r, err := execution.NewBybitAccount(readCreds)
		if err != nil {
			logger.Fatalf("FATAL building bybit reader: %v", err)
		}
		readers["bybit"] = r
	default:
		logger.Fatalf("FATAL unknown venue %q", *venue)
	}

	if *enable {
		tradeCreds, err := execution.FromEnv(*venue, execution.Trade, mode)
		if err != nil {
			logger.Fatalf("FATAL %v", err)
		}
		switch strings.ToLower(*venue) {
		case "binance":
			t, err := execution.NewBinanceTrader(tradeCreds)
			if err != nil {
				logger.Fatalf("FATAL building binance trader: %v", err)
			}
			traders["binance"] = t
		case "bybit":
			t, err := execution.NewBybitTrader(tradeCreds)
			if err != nil {
				logger.Fatalf("FATAL building bybit trader: %v", err)
			}
			traders["bybit"] = t
		}
	}

	cfg := live.Config{
		Mode:                 mode,
		Enabled:              *enable,
		KillSwitchPath:       *killSwitch,
		MaxOpenPositions:     *maxPos,
		NotionalUSDPerLeg:    *notional,
		MaxSlippageBpsPerLeg: *maxSlip,
		ConfirmAttempts:      5,
		ConfirmBackoff:       2 * time.Second,
		Risk:                 risk.DefaultThresholds(),
	}

	mgr := live.New(cfg, traders, readers, j, logger)
	rec := live.NewReconciler(readers)

	banner(logger, cfg, *venue, readCreds)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- one-shot open -------------------------------------------------------
	if *openSymbol != "" {
		if err := openOne(ctx, logger, mgr, *venue, strings.ToUpper(*openSymbol)); err != nil {
			logger.Printf("open failed: %v", err)
			// Not fatal. The monitoring loop below is exactly what you want
			// running after a failed open -- it will show whether anything was
			// left behind.
		}
	}

	// --- monitor loop --------------------------------------------------------
	logger.Printf("monitoring every %s; Ctrl-C or SIGTERM to stop", *poll)

	cycle(ctx, logger, mgr, rec, readers, j, cfg)

	t := time.NewTicker(*poll)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Printf("stopping: %v", ctx.Err())
			if *closeAll {
				closeEverything(context.Background(), logger, mgr, "shutdown requested")
			}
			_ = j.Flush()
			return
		case <-t.C:
			cycle(ctx, logger, mgr, rec, readers, j, cfg)
		}
	}
}

// banner states, at startup and in the log, exactly what this process can do.
//
// Three months from now the only way to know whether a journal was produced by
// a read-only watcher or a live trader is to have written it down at the time.
func banner(logger *log.Logger, cfg live.Config, venue string, creds execution.Credentials) {
	logger.Printf("========================================================")
	logger.Printf("VEGA live -- venue %s, mode %s", venue, cfg.Mode)
	logger.Printf("credentials: %s", creds) // redacted by Credentials.String

	if !cfg.Enabled {
		logger.Printf("ORDER PLACEMENT: DISABLED. This process is READ-ONLY.")
		logger.Printf("  It will snapshot accounts, assess liquidation risk and")
		logger.Printf("  reconcile PnL. It holds no trading client at all.")
	} else {
		logger.Printf("ORDER PLACEMENT: ENABLED on %s", cfg.Mode)
		logger.Printf("  notional  $%.2f per leg (~$%.2f deployed per position)",
			cfg.NotionalUSDPerLeg, 2*cfg.NotionalUSDPerLeg)
		logger.Printf("  max open  %d", cfg.MaxOpenPositions)
		logger.Printf("  max slip  %.1f bps on the first leg before aborting", cfg.MaxSlippageBpsPerLeg)
		logger.Printf("  kill file %s (create it to block all opening)", cfg.KillSwitchPath)

		if cfg.Mode == execution.Mainnet {
			logger.Printf("  *** REAL MONEY ***")
		}
	}

	logger.Printf("order response shapes verified: binance=%v bybit=%v",
		execution.BinanceOrderShapesVerified, execution.BybitOrderShapesVerified)
	if cfg.Mode == execution.Mainnet &&
		!(execution.BinanceOrderShapesVerified && execution.BybitOrderShapesVerified) {
		logger.Printf("  mainnet orders will be REFUSED until these are verified")
	}
	logger.Printf("risk thresholds: critical <%.0f%%, danger <%.0f%%, watch <%.0f%% to liquidation",
		cfg.Risk.CriticalPct, cfg.Risk.DangerPct, cfg.Risk.WatchPct)
	logger.Printf("========================================================")
}

// cycle is one pass: reconcile, then risk, then act.
func cycle(ctx context.Context, logger *log.Logger, mgr *live.Manager,
	rec *live.Reconciler, readers map[string]execution.AccountReader,
	j *journal.Journal, cfg live.Config) {

	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if halted, why := mgr.Halted(); halted {
		logger.Printf("HALTED: %s", why)
	}

	positions := mgr.Positions()

	// --- reconcile: the numbers come from the venue, not from us ---
	rep, err := rec.Reconcile(c, positions, cfg.Mode)
	if err != nil {
		logger.Printf("reconcile error: %v", err)
	} else {
		logger.Printf("RECONCILE %s", rep.Summary)
		for _, p := range rep.Positions {
			logger.Printf("  %s", p)
		}
		for _, w := range rep.Warnings {
			logger.Printf("  WARNING %s", w)
		}
		for _, d := range rep.Divergences {
			if d.Material {
				logger.Printf("  DIVERGENCE %s", d)
			}
		}
		writeJSON(j, logger, "live_reconcile", rep)
	}

	// --- liquidation risk, read fresh from each venue ---
	pr, ok := assessRisk(c, logger, readers, cfg)
	if !ok {
		return
	}
	logger.Printf("RISK %s -- %s", pr.WorstName, pr.Summary)
	for _, a := range pr.Alerts {
		logger.Printf("  %s", a)
	}
	writeJSON(j, logger, "live_risk", pr)

	// --- act: close anything critical, both legs ---
	if pr.CriticalCount > 0 {
		for _, p := range positions {
			if !p.Open() {
				continue
			}
			logger.Printf("CRITICAL risk on %s -- closing", p.ID)
			if err := mgr.CloseHedge(c, p, "liquidation risk CRITICAL"); err != nil {
				logger.Printf("  close failed: %v", err)
			}
		}
	}
}

// assessRisk reads every venue account fresh and scores it.
//
// Snapshots are never cached between cycles. A cached liquidation price is a
// liquidation price you do not have -- the number moves with every tick of
// mark price, and the whole point of this loop is to notice it moving.
func assessRisk(ctx context.Context, logger *log.Logger,
	readers map[string]execution.AccountReader, cfg live.Config) (risk.PortfolioRisk, bool) {

	snaps := make([]execution.AccountSnapshot, 0, len(readers))
	for venue, r := range readers {
		s, err := r.Snapshot(ctx)
		if err != nil {
			// One venue unreadable makes the WHOLE portfolio assessment
			// unreliable, so this abandons the pass rather than reporting a
			// reassuring number computed from the venues that did answer.
			logger.Printf("RISK UNKNOWN -- could not read %s: %v", venue, err)
			return risk.PortfolioRisk{}, false
		}
		snaps = append(snaps, s)
	}
	return risk.AssessPortfolio(snaps, cfg.Risk), true
}

// openOne places a single hedge, using prices read at this moment.
func openOne(ctx context.Context, logger *log.Logger, mgr *live.Manager, venue, symbol string) error {
	c, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// Reference prices come from the PUBLIC market data layer, the same one the
	// paper monitor has been using for weeks. MarkPrice for the perp;
	// IndexPrice for the spot leg.
	//
	// IndexPrice is a composite across several exchanges, so it is close to but
	// not identical to the spot price you will actually pay here. That is fine
	// and in fact useful: the difference shows up as measured slippage rather
	// than disappearing into an assumption.
	var src exchange.RateSource
	switch strings.ToLower(venue) {
	case "binance":
		src = exchange.NewBinance(2.0)
	case "bybit":
		src = exchange.NewBybit(2.0)
	default:
		return fmt.Errorf("no public source for venue %q", venue)
	}

	obs, err := src.FundingRates(c)
	if err != nil {
		return fmt.Errorf("reading %s market data: %w", venue, err)
	}

	for _, o := range obs {
		if !strings.EqualFold(o.Symbol, symbol) {
			continue
		}
		if o.MarkPrice <= 0 || o.IndexPrice <= 0 {
			return fmt.Errorf("%s %s has no usable reference price (mark %.8f, index %.8f)",
				venue, symbol, o.MarkPrice, o.IndexPrice)
		}
		plan := live.HedgePlan{
			Venue:          venue,
			Symbol:         o.Symbol,
			SpotRef:        o.IndexPrice,
			PerpRef:        o.MarkPrice,
			FundingRatePct: o.FundingRatePct,
			DecidedAt:      time.Now().UTC(),
		}
		logger.Printf("opening %s %s at funding %.6f%%/%.0fh (spot ref %.8f, perp ref %.8f)",
			venue, symbol, o.FundingRatePct, o.IntervalHours, plan.SpotRef, plan.PerpRef)

		pos, err := mgr.OpenHedge(c, plan)
		if err != nil {
			return err
		}
		logger.Printf("opened %s", pos.ID)
		return nil
	}
	return fmt.Errorf("%s does not list %s", venue, symbol)
}

// closeEverything flattens on the way out.
func closeEverything(ctx context.Context, logger *log.Logger, mgr *live.Manager, reason string) {
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for _, p := range mgr.Positions() {
		if !p.Open() {
			continue
		}
		if err := mgr.CloseHedge(c, p, reason); err != nil {
			logger.Printf("FAILED to close %s: %v", p.ID, err)
			logger.Printf("  THIS POSITION IS STILL OPEN. Check the exchange manually.")
		}
	}
}

// writeJSON journals a value, logging rather than failing on error.
func writeJSON(j *journal.Journal, logger *log.Logger, kind string, v any) {
	if j == nil {
		return
	}
	wrapper := map[string]any{
		"type":  kind,
		"ts_ms": time.Now().UnixMilli(),
		"data":  v,
	}
	if err := j.Write(wrapper); err != nil {
		logger.Printf("journal write failed (%s): %v", kind, err)
		return
	}
	// Flushed immediately. Unlike the paper monitor, which batches because
	// volume matters there, every record here describes real money.
	_ = j.Flush()
}
