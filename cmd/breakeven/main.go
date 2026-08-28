// Command breakeven prints the corrected funding-arbitrage arithmetic.
//
// It talks to nothing and trades nothing. Its only job is to put the numbers
// that killed the previous bot on screen, in full, before another line of
// code is written on top of them.
//
//	go run ./cmd/breakeven
//	go run ./cmd/breakeven -slip 0     # exact 30 bps, matches the brief
//	go run ./cmd/breakeven -notional 100000
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"text/tabwriter"

	"github.com/imtiyaz/vega-bot/pkg/economics"
)

func main() {
	slip := flag.Float64("slip", 1.0, "slippage allowance in bps per leg (0 = fees only)")
	notional := flag.Float64("notional", 10000, "notional USD per leg")
	flag.Parse()

	fees := economics.FeeSchedule{
		SpotTakerBps:      10,
		FuturesTakerBps:   5,
		SlippageBpsPerLeg: *slip,
	}
	if err := fees.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	fmt.Println("VEGA -- funding arbitrage cost model")
	fmt.Println("====================================")
	fmt.Println()

	// ---- 1. the cost side, which the previous bot did not have -----------
	fmt.Println("ROUND TRIP COST (Binance VIP 0, no BNB discount, taker on all legs)")
	fmt.Fprintf(w, "  buy spot\t%.1f bps\n", fees.SpotTakerBps)
	fmt.Fprintf(w, "  short futures\t%.1f bps\n", fees.FuturesTakerBps)
	fmt.Fprintf(w, "  close futures\t%.1f bps\n", fees.FuturesTakerBps)
	fmt.Fprintf(w, "  sell spot\t%.1f bps\n", fees.SpotTakerBps)
	fmt.Fprintf(w, "  slippage allowance (4 legs)\t%.1f bps\n", 4*(*slip))
	fmt.Fprintf(w, "  ROUND TRIP\t%.1f bps\n", fees.RoundTripBps())
	w.Flush()
	fmt.Println()

	// ---- 2. break-even hold, by rate --------------------------------------
	fmt.Println("BREAK-EVEN HOLD TIME  (days until funding covers the round trip)")
	fmt.Fprintln(w, "  rate/8h\tannualised on notional\tbreak-even\tnote")
	fmt.Fprintln(w, "  -------\t----------------------\t----------\t----")
	rates := []struct {
		pct  float64
		note string
	}{
		{0.001, "near zero, common on majors in calm markets"},
		{0.005, "the previous bot's MIN_FUNDING_RATE"},
		{0.010, "typical baseline"},
		{0.030, "elevated"},
		{0.050, "altcoin, elevated"},
		{0.100, "cap on many symbols; rarely persists"},
	}
	for _, r := range rates {
		be := economics.BreakEvenDays(r.pct, fees, economics.IntervalsPerDay)
		fmt.Fprintf(w, "  %.3f%%\t%.2f%%\t%.1f d\t%s\n",
			r.pct, economics.AnnualizedPct(r.pct, economics.IntervalsPerDay), be, r.note)
	}
	w.Flush()
	fmt.Println()

	// ---- 3. the entry gate ------------------------------------------------
	fmt.Println("MINIMUM VIABLE RATE  (the entry gate; replaces the hardcoded constant)")
	fmt.Fprintln(w, "  planned hold\tminimum rate/8h\tequivalent annualised")
	fmt.Fprintln(w, "  ------------\t---------------\t---------------------")
	for _, hold := range []float64{1, 3, 7, 14, 30, 60} {
		minRate := economics.MinViableRatePct(fees, hold)
		fmt.Fprintf(w, "  %.0f days\t%.4f%%\t%.1f%%\n",
			hold, minRate, economics.AnnualizedPct(minRate, economics.IntervalsPerDay))
	}
	w.Flush()
	fmt.Println()

	// ---- 4. the post-mortem, reproduced -----------------------------------
	fmt.Println("THE PREVIOUS BOT'S CONFIGURATION, ASSESSED")
	fmt.Println("  MIN_FUNDING_RATE=0.005  MAX_POSITION_HOURS=72")
	old, err := economics.Assess(economics.Opportunity{
		Symbol:         "BTCUSDT",
		FundingRatePct: 0.005,
		NotionalUSD:    *notional,
	}, fees, 3)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	printAssessment(w, old)
	fmt.Println()

	// ---- 5. the same rate, held long enough -------------------------------
	fmt.Printf("THE SAME RATE, HELD TO BREAK-EVEN (%.0f days)\n", math.Ceil(old.BreakEvenDays)+1)
	fixed, _ := economics.Assess(economics.Opportunity{
		Symbol:         "BTCUSDT",
		FundingRatePct: 0.005,
		NotionalUSD:    *notional,
	}, fees, math.Ceil(old.BreakEvenDays)+1)
	printAssessment(w, fixed)
	fmt.Println()

	// ---- 6. what the strategy actually returns ----------------------------
	fmt.Println("RETURN ON DEPLOYED CAPITAL, IF A RATE PERSISTED FOR A FULL YEAR")
	fmt.Println("  (both legs need funding, so capital is ~2x notional)")
	fmt.Fprintln(w, "  rate/8h\tannual on notional\tannual on capital\tper month")
	fmt.Fprintln(w, "  -------\t------------------\t-----------------\t---------")
	for _, r := range []float64{0.005, 0.01, 0.02, 0.03, 0.05, 0.10} {
		a, _ := economics.Assess(economics.Opportunity{
			Symbol: "X", FundingRatePct: r, NotionalUSD: *notional,
		}, fees, economics.DaysPerYear)
		fmt.Fprintf(w, "  %.3f%%\t%.2f%%\t%.2f%%\t%.2f%%\n",
			r, a.AnnualizedPct, a.NetAnnualOnCapital, a.NetAnnualOnCapital/12)
	}
	w.Flush()
	fmt.Println()
	fmt.Println("  Sustained 0.01-0.02% per 8h is the realistic baseline, which is")
	fmt.Println("  0.4-1.2% per month net on deployed capital. The higher rows require")
	fmt.Println("  an elevated regime to hold for a full year, which does not happen.")
	fmt.Println("  These are scalings of one rate, not forecasts. The ledger decides.")
}

func printAssessment(w *tabwriter.Writer, a economics.Assessment) {
	fmt.Fprintf(w, "  funding over %.0f d (%.0f intervals)\t%+.2f bps\n",
		a.HoldDays, a.IntervalsHeld, a.ExpectedFundingBps)
	fmt.Fprintf(w, "  round trip cost\t-%.2f bps\n", a.CostBps)
	fmt.Fprintf(w, "  NET\t%+.2f bps\t= %+.2f USD on %.0f notional\n",
		a.NetBps, a.NetUSD, a.NotionalUSD)
	fmt.Fprintf(w, "  break-even needs\t%.1f days\n", a.BreakEvenDays)
	fmt.Fprintf(w, "  VIABLE\t%v\n", a.Viable)
	fmt.Fprintf(w, "  reason\t%s\n", a.Reason)
	w.Flush()
}
