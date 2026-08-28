// Command dispersion measures CROSS-VENUE funding dispersion for perp-perp
// pairs, and costs it.
//
// # WHY THIS IS A DIFFERENT TRADE FROM THE REST OF VEGA
//
// Everything else here is cash-and-carry: long spot, short perp, same venue.
// Its round trip measured 30 bps on Binance, of which TWENTY were the spot
// taker fee charged twice. Break-even lands near day 19.
//
// Perp-perp deletes the spot leg entirely. Long the perp where funding is
// negative, short the perp where it is positive, on two different venues. Four
// perp legs instead of two perp and two spot, and perp fees are roughly half
// spot fees. The round trip drops to somewhere near 20 bps on CEXs and lower
// on venues that rebate makers.
//
// It is also delta neutral, and it removes the "no spot market" refusal that
// discards 853 of 1517 symbols on every scan.
//
// THE CATCH, STATED UP FRONT
//
//   - Funding here is a SPREAD between two venues, not a rate. It can collapse
//     the moment either venue moves, and both move independently.
//   - Capital must sit on BOTH venues simultaneously. $10k of notional needs
//     $10k on each, and it cannot be rebalanced instantly.
//   - A DEX holds your collateral in a contract. There is no support desk and
//     no recourse. That risk is real and this tool does not price it.
//
// # THE INTERVAL TRAP
//
// Hyperliquid settles funding EVERY HOUR and quotes a per-hour rate. Binance
// and Bybit settle every EIGHT hours and quote a per-8h rate. Comparing the
// raw numbers is an 8x error in whichever direction happens to flatter the
// answer. Every rate here is normalised to bps per hour before anything is
// compared, and a venue whose interval is not known is REFUSED rather than
// assumed.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

const hlInfoURL = "https://api.hyperliquid.xyz/info"

// venueIntervalHours is the settlement interval each venue quotes its rate in.
//
// A venue missing from this map is refused, not guessed. Guessing is the 8x
// error this whole file exists to avoid.
var venueIntervalHours = map[string]float64{
	"HlPerp":    1,
	"BinPerp":   8,
	"BybitPerp": 8,
}

// venueLabel maps Hyperliquid's short names to something readable.
var venueLabel = map[string]string{
	"HlPerp":    "hyperliquid",
	"BinPerp":   "binance",
	"BybitPerp": "bybit",
}

// takerBpsDefault are DELIBERATELY UNVERIFIED starting values.
//
// Nobody in this project has read these off the venues' own fee pages. They
// are printed as UNVERIFIED on every run and every result is marked
// accordingly, exactly as pkg/exchange treats an unverified venue. Override
// with the flags once you have checked.
var takerBpsDefault = map[string]float64{
	"hyperliquid": 4.5,
	"binance":     5.0,
	"bybit":       5.5,
}

func main() {
	holdHours := flag.Float64("hold", 720, "planned hold in hours (720 = 30 days)")
	notional := flag.Float64("notional", 10000, "USD per leg; capital deployed is roughly 2x this")
	minVolUSD := flag.Float64("min-vol", 5_000_000, "minimum Hyperliquid 24h notional volume")
	top := flag.Int("top", 25, "rows to print")
	hlTaker := flag.Float64("hl-taker", takerBpsDefault["hyperliquid"], "hyperliquid taker fee in bps")
	binTaker := flag.Float64("bin-taker", takerBpsDefault["binance"], "binance futures taker fee in bps")
	bybitTaker := flag.Float64("bybit-taker", takerBpsDefault["bybit"], "bybit perp taker fee in bps")
	jsonOut := flag.String("json", "", "append top rows to this JSONL file instead of printing a table")
	feesVerified := flag.Bool("fees-verified", false, "assert you have checked all three fee pages today")
	flag.Parse()

	taker := map[string]float64{
		"hyperliquid": *hlTaker,
		"binance":     *binTaker,
		"bybit":       *bybitTaker,
	}

	start := time.Now()

	rates, err := predictedFundings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL reading predicted fundings: %v\n", err)
		os.Exit(1)
	}
	ctxs, err := hlAssetContexts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING hyperliquid asset contexts unavailable (%v); "+
			"volume and impact-price filters are OFF\n", err)
	}
	elapsed := time.Since(start)

	type row struct {
		coin       string
		longVenue  string
		shortVenue string
		longRate   float64
		shortRate  float64
		spreadHr   float64
		roundTrip  float64
		beHours    float64
		netBps     float64
		netUSD     float64
		volUSD     float64
		impactBps  float64
		measured   bool
	}

	var rows []row
	var skippedUnknownVenue int
	var skippedUnmeasured int

	for coin, byVenue := range rates {
		names := make([]string, 0, len(byVenue))
		for v := range byVenue {
			if _, ok := venueIntervalHours[v]; !ok {
				skippedUnknownVenue++
				continue
			}
			names = append(names, v)
		}
		sort.Strings(names)

		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				a, b := names[i], names[j]

				// Normalise to bps per hour. This is the step that makes
				// Hyperliquid's hourly rate comparable with Binance's 8h rate.
				ra := byVenue[a] * 100 * 100 / venueIntervalHours[a]
				rb := byVenue[b] * 100 * 100 / venueIntervalHours[b]

				longV, shortV := a, b
				longR, shortR := ra, rb
				if ra > rb {
					longV, shortV = b, a
					longR, shortR = rb, ra
				}
				spread := shortR - longR
				if spread <= 0 {
					continue
				}

				// Depth must be MEASURED. An unmeasured coin is not a cheap
				// coin -- it is an unknown one, and RequireMeasuredLiquidity
				// has refused those everywhere else in this project since the
				// first day.
				c, measured := ctxs[coin]
				if !measured {
					skippedUnmeasured++
					continue
				}

				// Four legs, each crossing half the impact spread, so the
				// round trip carries 2x the full spread.
				//
				// UNDERSTATED: only Hyperliquid publishes impactPxs. The other
				// venue's spread is not in here at all, so the true cost is
				// higher than this by whatever Binance or Bybit charges in
				// spread. Treat this as a floor.
				slip := 2 * c.impactSpreadBps
				rt := 2*(taker[venueLabel[longV]]+taker[venueLabel[shortV]]) + slip
				r := row{
					coin:       coin,
					longVenue:  venueLabel[longV],
					shortVenue: venueLabel[shortV],
					longRate:   longR,
					shortRate:  shortR,
					spreadHr:   spread,
					roundTrip:  rt,
					beHours:    rt / spread,
					netBps:     spread*(*holdHours) - rt,
				}
				r.netUSD = r.netBps / 10000 * (*notional)
				r.volUSD = c.volUSD
				r.impactBps = c.impactSpreadBps
				r.measured = true
				rows = append(rows, r)
			}
		}
	}

	kept := rows[:0]
	var droppedThin int
	for _, r := range rows {
		if r.measured && r.volUSD < *minVolUSD {
			droppedThin++
			continue
		}
		kept = append(kept, r)
	}
	rows = kept

	sort.Slice(rows, func(i, j int) bool { return rows[i].netBps > rows[j].netBps })

	fmt.Printf("VEGA cross-venue dispersion  %s UTC   (polled in %s)\n",
		time.Now().UTC().Format("2006-01-02 15:04:05"), elapsed.Round(time.Millisecond))
	fmt.Printf("hold=%.0fh (%.1f days)  notional=$%.0f/leg  capital=$%.0f  min-vol=$%.0f\n",
		*holdHours, *holdHours/24, *notional, *notional*2, *minVolUSD)
	fmt.Println()

	if !*feesVerified {
		fmt.Println("!!! FEES UNVERIFIED -- nobody has read these off the venues' fee pages.")
		fmt.Printf("!!! Using hyperliquid %.2f, binance %.2f, bybit %.2f bps taker.\n",
			taker["hyperliquid"], taker["binance"], taker["bybit"])
		fmt.Println("!!! Every net figure below is therefore UNPROVEN. Check the pages, then")
		fmt.Println("!!! pass -hl-taker/-bin-taker/-bybit-taker and -fees-verified.")
		fmt.Println()
	}

	fmt.Println("SOURCE  hyperliquid /info predictedFundings + metaAndAssetCtxs (public, no keys)")
	fmt.Println("        rates normalised to bps/hour: HlPerp quotes hourly, Bin/Bybit quote 8-hourly")
	if skippedUnknownVenue > 0 {
		fmt.Printf("        %d venue quotes skipped: settlement interval unknown, refused rather than assumed\n",
			skippedUnknownVenue)
	}
	if skippedUnmeasured > 0 {
		fmt.Printf("        %d pairs refused: Hyperliquid publishes no depth for them, so cost is UNKNOWN\n",
			skippedUnmeasured)
	}
	if droppedThin > 0 {
		fmt.Printf("        %d pairs dropped below the $%.0f volume floor\n", droppedThin, *minVolUSD)
	}
	fmt.Println()

	if len(rows) == 0 {
		fmt.Println("No pair shows positive funding dispersion right now.")
		fmt.Println("That is a finding: the venues are aligned, and there is nothing to capture.")
		return
	}

	// JSONL mode: one line per poll, for measuring how often and for how long
	// a dislocation actually exists. A table tells you about now; a log tells
	// you about the distribution.
	if *jsonOut != "" {
		f, err := os.OpenFile(*jsonOut, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL opening %s: %v\n", *jsonOut, err)
			os.Exit(1)
		}
		defer f.Close()

		n := *top
		if n > len(rows) {
			n = len(rows)
		}
		for i := 0; i < n; i++ {
			r := rows[i]
			rec := map[string]any{
				"ts_ms":         time.Now().UTC().UnixMilli(),
				"coin":          r.coin,
				"long_venue":    r.longVenue,
				"short_venue":   r.shortVenue,
				"spread_bps_hr": r.spreadHr,
				"cost_bps":      r.roundTrip,
				"be_hours":      r.beHours,
				"vol_usd":       r.volUSD,
				"impact_bps":    r.impactBps,
			}
			line, _ := json.Marshal(rec)
			f.Write(append(line, '\n'))
		}
		fmt.Printf("appended %d rows to %s\n", n, *jsonOut)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  #\tcoin\tLONG (pays you)\tSHORT (pays you)\tspread bps/hr\tbps/day\tround trip\tbreak-even\tnet bps\tnet $\t24h vol\timpact")
	fmt.Fprintln(w, "  -\t----\t---------------\t----------------\t-------------\t-------\t----------\t----------\t-------\t-----\t-------\t------")

	limit := *top
	if limit > len(rows) {
		limit = len(rows)
	}
	for i := 0; i < limit; i++ {
		r := rows[i]
		be := fmt.Sprintf("%.1f h", r.beHours)
		if r.beHours > 48 {
			be = fmt.Sprintf("%.1f d", r.beHours/24)
		}
		vol := "UNKNOWN"
		imp := "UNKNOWN"
		if r.measured {
			vol = humanUSD(r.volUSD)
			imp = fmt.Sprintf("%.2f bps", r.impactBps)
		}
		fmt.Fprintf(w, "  %d\t%s\t%s %+.4f\t%s %+.4f\t%+.4f\t%+.2f\t%.1f\t%s\t%+.1f\t$%+.2f\t%s\t%s\n",
			i+1, r.coin,
			r.longVenue, r.longRate, r.shortVenue, r.shortRate,
			r.spreadHr, r.spreadHr*24, r.roundTrip, be,
			r.netBps, r.netUSD, vol, imp)
	}
	w.Flush()

	var totalNet, totalNotional float64
	for _, r := range rows {
		totalNet += r.netUSD
		totalNotional += *notional
	}
	capital := totalNotional * 2
	fmt.Println()
	fmt.Printf("  %d pairs pass. Taking ALL of them: $%.0f capital, $%+.2f over %.1f days = %+.3f%% on capital = %+.2f%% annualised\n",
		len(rows), capital, totalNet, *holdHours/24,
		totalNet/capital*100, totalNet/capital*100*(365/(*holdHours/24)))

	best := rows[0]
	fmt.Printf("  Best single pair: %s long %s / short %s, %+.4f bps/hr, break-even %.1f h\n",
		best.coin, best.longVenue, best.shortVenue, best.spreadHr, best.beHours)

	fmt.Println()
	fmt.Println("READ THIS BEFORE BELIEVING ANY OF IT")
	fmt.Println("  A spread is not a rate. Both legs move independently and either can flip")
	fmt.Println("  within one settlement. The break-even column assumes today's spread holds")
	fmt.Println("  for the whole hold, which is the same assumption that made MOVEUSDT look")
	fmt.Println("  like 44% annualised while its book held $85k.")
	fmt.Println("  Capital must sit on BOTH venues at once and cannot be moved instantly.")
	fmt.Println("  A DEX holds collateral in a contract with no recourse. Not priced here.")
}

func hlPost(body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, hlInfoURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
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
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return json.Unmarshal(raw, out)
}

// predictedFundings returns coin -> venue -> rate IN THAT VENUE'S OWN INTERVAL.
//
// Shape is nested heterogeneous arrays:
//
//	[["AVAX", [["BinPerp", {"fundingRate":"0.0001", ...}], ...]], ...]
//
// which Go cannot unmarshal into a struct, so it goes through json.RawMessage
// element by element. Any element that does not parse is SKIPPED rather than
// defaulted -- a coin missing from the result is better than a coin with an
// invented rate.
func predictedFundings() (map[string]map[string]float64, error) {
	var top []json.RawMessage
	if err := hlPost(map[string]string{"type": "predictedFundings"}, &top); err != nil {
		return nil, err
	}

	out := map[string]map[string]float64{}
	for _, entry := range top {
		var pair []json.RawMessage
		if json.Unmarshal(entry, &pair) != nil || len(pair) != 2 {
			continue
		}
		var coin string
		if json.Unmarshal(pair[0], &coin) != nil || coin == "" {
			continue
		}

		var venues []json.RawMessage
		if json.Unmarshal(pair[1], &venues) != nil {
			continue
		}

		byVenue := map[string]float64{}
		for _, v := range venues {
			var vp []json.RawMessage
			if json.Unmarshal(v, &vp) != nil || len(vp) != 2 {
				continue
			}
			var name string
			if json.Unmarshal(vp[0], &name) != nil {
				continue
			}
			var payload struct {
				FundingRate string `json:"fundingRate"`
			}
			if json.Unmarshal(vp[1], &payload) != nil || payload.FundingRate == "" {
				continue
			}
			rate, err := strconv.ParseFloat(payload.FundingRate, 64)
			if err != nil {
				continue
			}
			byVenue[name] = rate
		}
		if len(byVenue) >= 2 {
			out[coin] = byVenue
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable coins in predictedFundings response")
	}
	return out, nil
}

type hlCtx struct {
	volUSD          float64
	impactSpreadBps float64
}

// hlAssetContexts returns Hyperliquid's own depth and volume per coin.
//
// The response is [meta, ctxs] where meta.universe[i] pairs with ctxs[i] BY
// INDEX. Two parallel arrays with no key linking them: if either is truncated,
// every coin after that point is silently attributed to the wrong asset. So
// the lengths are checked and the shorter one wins.
func hlAssetContexts() (map[string]hlCtx, error) {
	var top []json.RawMessage
	if err := hlPost(map[string]string{"type": "metaAndAssetCtxs"}, &top); err != nil {
		return nil, err
	}
	if len(top) != 2 {
		return nil, fmt.Errorf("expected [meta, ctxs], got %d elements", len(top))
	}

	var meta struct {
		Universe []struct {
			Name       string `json:"name"`
			IsDelisted bool   `json:"isDelisted"`
		} `json:"universe"`
	}
	if err := json.Unmarshal(top[0], &meta); err != nil {
		return nil, err
	}

	var ctxs []struct {
		DayNtlVlm string   `json:"dayNtlVlm"`
		MarkPx    string   `json:"markPx"`
		MidPx     string   `json:"midPx"`
		ImpactPxs []string `json:"impactPxs"`
	}
	if err := json.Unmarshal(top[1], &ctxs); err != nil {
		return nil, err
	}

	n := len(meta.Universe)
	if len(ctxs) < n {
		n = len(ctxs)
	}

	out := make(map[string]hlCtx, n)
	for i := 0; i < n; i++ {
		if meta.Universe[i].IsDelisted {
			continue
		}
		c := hlCtx{}
		c.volUSD, _ = strconv.ParseFloat(ctxs[i].DayNtlVlm, 64)

		// impactPxs is [bid, ask] at Hyperliquid's impact notional -- a
		// realistic execution price rather than the touch. The gap between
		// them is the closest thing to a measured round-trip spread the
		// public API offers.
		if len(ctxs[i].ImpactPxs) == 2 {
			bid, err1 := strconv.ParseFloat(ctxs[i].ImpactPxs[0], 64)
			ask, err2 := strconv.ParseFloat(ctxs[i].ImpactPxs[1], 64)
			if err1 == nil && err2 == nil && bid > 0 && ask > 0 {
				mid := (bid + ask) / 2
				c.impactSpreadBps = (ask - bid) / mid * 10000
			}
		}
		out[meta.Universe[i].Name] = c
	}
	return out, nil
}

func humanUSD(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.1fb", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.0fm", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.0fk", v/1e3)
	case v > 0:
		return fmt.Sprintf("$%.0f", v)
	default:
		return "-"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
