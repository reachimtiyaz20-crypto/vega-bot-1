// Command scan polls every registered venue once, applies each venue's own
// fee model and each symbol's MEASURED execution cost, and ranks what
// survives.
//
// It prints why symbols were rejected as prominently as it prints what
// passed. The first version of this tool reported 447 of 806 symbols viable;
// six of its top eight had no spot market and could not be traded at any
// price. A screen that only shows winners cannot be audited.
//
//	go run ./cmd/scan
//	go run ./cmd/scan -hold 30 -min-vol 10000000
//	go run ./cmd/scan -survey          # no filters, see the whole universe
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/exchange"
)

func main() {
	hold := flag.Float64("hold", 30, "planned hold in days")
	notional := flag.Float64("notional", 10000, "notional USD per leg")
	minVol := flag.Float64("min-vol", 50_000_000, "minimum 24h quote volume in USD, applied to BOTH legs")
	fallbackSlip := flag.Float64("fallback-slip", 2.0, "slippage bps/leg used ONLY when the book cannot be read")
	minDepth := flag.Float64("min-depth", 0.25, "required top-of-book size as a fraction of notional, both legs")
	maxSlip := flag.Float64("max-slip", 8, "reject if MEASURED round-trip slippage exceeds this many bps")
	top := flag.Int("top", 20, "how many rows to print")
	adaptive := flag.Bool("adaptive", false, "size each position to what the book can hold, instead of a fixed notional")
	survey := flag.Bool("survey", false, "drop volume and liquidity filters; shows the raw universe")
	flag.Parse()

	cons := exchange.Constraints{
		NotionalUSD:              *notional,
		HoldDays:                 *hold,
		MinQuoteVolume24hUSD:     *minVol,
		MinTopOfBookFraction:     *minDepth,
		RequireMeasuredLiquidity: true,
		MaxRoundTripSlippageBps:  *maxSlip,
		Sizing:                   exchange.DefaultSizingPolicy(*notional),
	}
	if *survey {
		cons.MinQuoteVolume24hUSD = 0
		cons.MinTopOfBookFraction = 0
		cons.RequireMeasuredLiquidity = false
	}

	reg := exchange.NewRegistry(exchange.NewBinance(*fallbackSlip), exchange.NewBybit(*fallbackSlip))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	obs, errs := reg.CollectAll(ctx)
	elapsed := time.Since(start)

	for venue, err := range errs {
		fmt.Fprintf(os.Stderr, "WARN %s: %v\n", venue, err)
	}
	if len(obs) == 0 {
		fmt.Fprintln(os.Stderr, "no observations from any venue")
		os.Exit(1)
	}

	venues := make(map[string]exchange.Venue)
	for _, s := range reg.Sources() {
		venues[s.Venue().Name] = s.Venue()
	}

	type row struct {
		o      exchange.Observation
		net    float64
		be     float64
		cost   float64
		size   float64
		ok     bool
		reason string
	}

	var passed []row
	rejected := map[string]int{}
	negative := 0

	for _, o := range obs {
		v, known := venues[o.Venue]
		if !known {
			continue
		}
		// Size to the book before costing it. A symbol whose touch cannot
		// hold the target is not necessarily untradeable -- it is tradeable
		// smaller. If sizing refuses outright, fall through unchanged and let
		// the normal depth filter reject it with its own reason.
		c := cons
		if *adaptive {
			if sized, sz := cons.ForBook(o); sz.Ok {
				c = sized
			}
		}
		a, err := exchange.Assess(v, o, c)
		if o.FundingRatePct < 0 {
			negative++
		}
		if a.Viable {
			passed = append(passed, row{o: o, net: a.NetBps, be: a.BreakEvenDays, cost: a.CostBps, size: c.NotionalUSD, ok: true, reason: a.Reason})
			continue
		}
		rejected[classify(err, a.Reason)]++
	}

	sort.Slice(passed, func(i, j int) bool { return passed[i].net > passed[j].net })

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	fmt.Printf("VEGA scan  %s UTC   (polled in %s)\n", time.Now().UTC().Format("2006-01-02 15:04:05"), elapsed.Round(time.Millisecond))
	fmt.Printf("hold=%.0fd  notional=$%.0f/leg  capital=$%.0f  volume floor=$%.0f  depth floor=%.2fx  measured-liquidity=%v\n",
		*hold, *notional, *notional*2, cons.MinQuoteVolume24hUSD, cons.MinTopOfBookFraction, cons.RequireMeasuredLiquidity)
	if *survey {
		fmt.Println("SURVEY MODE: filters off. These numbers are not enterable.")
	}
	fmt.Println()

	for _, s := range reg.Sources() {
		v := s.Venue()
		status := "VERIFIED"
		if err := v.Validate(); err != nil {
			status = "UNVERIFIED"
		}
		fmt.Printf("venue %-10s fees %s  (%s, checked %s)\n", v.Name, status, v.Source.URL, v.Source.VerifiedOn)
	}
	fmt.Println()

	// --- what was thrown away, and why -------------------------------------
	fmt.Println("REJECTED")
	fmt.Fprintln(w, "  count\treason")
	fmt.Fprintln(w, "  -----\t------")
	for _, k := range sortedKeys(rejected) {
		fmt.Fprintf(w, "  %d\t%s\n", rejected[k], k)
	}
	w.Flush()
	fmt.Println()

	// --- what survived ------------------------------------------------------
	fmt.Println("PASSED")
	if len(passed) == 0 {
		fmt.Println("  Nothing clears costs at this hold with these filters.")
		fmt.Println("  That is a finding, not a failure: the current funding regime does not")
		fmt.Println("  pay for a round trip on any symbol that is liquid enough to trade.")
		fmt.Printf("  Try -hold 30, or -survey to see the unfiltered universe.\n")
	} else {
		limit := *top
		if limit > len(passed) {
			limit = len(passed)
		}
		fmt.Fprintln(w, "  #\tsymbol\trate/int\tint\tsize/leg\tcost bps\tnet bps\tnet $/30d\tbreak-even\tslip bps\tspot vol\tbasis")
		fmt.Fprintln(w, "  -\t------\t--------\t---\t--------\t--------\t-------\t---------\t----------\t--------\t--------\t-----")
		for i := 0; i < limit; i++ {
			r := passed[i]
			fmt.Fprintf(w, "  %d\t%s\t%+.4f%%\t%.0fh\t$%.0f\t%.2f\t%+.2f\t%+.2f\t%.1f d\t%.2f\t%s\t%+.2f\n",
				i+1, r.o.Symbol, r.o.FundingRatePct, r.o.IntervalHours,
				r.size, r.cost, r.net, r.net/10000*r.size, r.be, r.o.RoundTripSlippageBps(),
				humanUSD(r.o.SpotQuoteVolume24hUSD), r.o.BasisBps())
		}
		w.Flush()

		// Totals across EVERY passing symbol, not just the rows printed above.
		// Sixteen small positions and seven large ones look alike until the
		// dollars are added up.
		var totalNotional, totalNetUSD float64
		for _, pr := range passed {
			totalNotional += pr.size
			totalNetUSD += pr.net / 10000 * pr.size
		}
		if totalNotional > 0 && *hold > 0 {
			capital := totalNotional * 2
			fmt.Println()
			fmt.Printf("  TOTAL across %d passing: $%.0f notional/leg, $%.0f capital deployed (both legs)\n",
				len(passed), totalNotional, capital)
			fmt.Printf("  TOTAL net over %.0f days: $%+.2f  =  %+.3f%% on deployed capital  =  %+.2f%% annualised\n",
				*hold, totalNetUSD, totalNetUSD/capital*100, totalNetUSD/capital*100*(365/(*hold)))
		}

		if strings.Contains(passed[0].reason, "CAUTION") {
			fmt.Println()
			fmt.Printf("  #1 %s: %s\n", passed[0].o.Symbol, passed[0].reason)
		}
	}
	fmt.Println()

	fmt.Printf("%d observed  |  %d passed  |  %d rejected  |  %d paying negative funding\n",
		len(obs), len(passed), len(obs)-len(passed), negative)
	fmt.Println()
	fmt.Println("Passing means funding exceeds the fully-costed round trip IF the rate holds")
	fmt.Println("for the whole period. Rates mean-revert, and a high rate is often payment")
	fmt.Println("for a risk rather than a gift. This is a screen, not a forecast.")
}

// classify turns a refusal into a countable category. The exact symbol names
// belong in the journal; the summary needs shapes.
func classify(err error, reason string) string {
	switch {
	case err == nil:
		return "did not clear costs at this hold"
	case strings.Contains(reason, "no spot pair"):
		return "no spot market -- position cannot be constructed"
	case strings.Contains(reason, "SPOT 24h volume"):
		return "spot volume too thin -- long leg cannot be bought"
	case strings.Contains(reason, "24h volume"):
		return "perp volume too thin for this size"
	case strings.Contains(reason, "at the touch"):
		return "book too shallow at the touch for this size"
	case strings.Contains(reason, "book could not be read"):
		return "book unreadable -- execution cost unknown"
	case strings.Contains(reason, "UNVERIFIED"):
		return "venue fees unverified"
	case strings.Contains(reason, "funding is negative"):
		return "negative funding -- the short leg pays"
	case strings.Contains(reason, "funding is zero"):
		return "zero funding"
	default:
		return "did not clear costs at this hold"
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return m[out[i]] > m[out[j]] })
	return out
}

func humanUSD(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.1fbn", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.0fm", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.0fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}
