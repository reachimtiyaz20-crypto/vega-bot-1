// Command listbacktest replays the new-listing funding strategy over history.
//
// # THE STRATEGY BEING TESTED
//
// A perp lists. Retail piles in, there is no spot market yet, and funding sits
// dislocated for days before normalising. Take the paying side on the listing
// venue, hedge on whichever other venue also lists it, hold while the spread
// is wide, exit when it closes.
//
// WHAT IS MEASURED EXACTLY
//
//	listing dates      Binance exchangeInfo onboardDate
//	funding, binance   /fapi/v1/fundingRate, full history
//	funding, bybit     /v5/market/funding/history
//	intervals          per symbol, from each venue
//
// WHAT IS ASSUMED, AND THIS IS THE HONEST WEAKNESS
//
//	SLIPPAGE. No venue publishes historical order books, so the cost of
//	crossing a brand-new listing's spread cannot be measured after the fact.
//	It is a flag. New listings have thin books -- measured 2026-08-16, Bybit
//	held as little as $924 on a recent listing -- so the true cost is probably
//	worse than whatever is passed in.
//
//	FILLS. Every entry and exit is assumed to fill at the modelled price. In a
//	market hours old that is generous.
//
//	FEES. Today's taker fees applied retroactively.
//
// So this is the OPTIMISTIC case. If it does not clear here, it does not clear.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"
)

const ua = "vega-research/1.0"

func get(url string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func num(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// hourly is a forward-filled series: hour since listing -> bps per hour.
//
// Venues settle on different clocks, so matching settlement instants would
// discard most of the data. Forward-filling each venue's last known rate onto
// an hourly grid makes them comparable without inventing anything -- the rate
// genuinely is in force until the next settlement replaces it.
func hourly(settlements map[int64]float64, ivl float64, onboard int64, hours int) []float64 {
	var ks []int64
	for k := range settlements {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })

	out := make([]float64, hours)
	for i := range out {
		out[i] = math.NaN()
	}
	for _, ts := range ks {
		h := int((ts - onboard) / 3_600_000)
		if h < 0 || h >= hours {
			continue
		}
		out[h] = settlements[ts] * 10000 / ivl // bps per hour
	}
	last := math.NaN()
	for i := range out {
		if !math.IsNaN(out[i]) {
			last = out[i]
		} else {
			out[i] = last
		}
	}
	return out
}

// listing is one perp and the moment it went live.
type listing struct {
	sym     string
	onboard int64
}

type result struct {
	Symbol    string
	Listed    time.Time
	Trades    int
	NetBps    float64
	HeldHours float64
	BestBps   float64
	WorstBps  float64
	MaxSpread float64
	Hedgeable bool
}

func main() {
	days := flag.Int("days", 365, "look back this many days for listings")
	window := flag.Int("window", 30, "days to follow each listing")
	minSpread := flag.Float64("min-spread", 1.0, "enter when |spread| exceeds this, bps/hr")
	exitSpread := flag.Float64("exit-spread", 0.3, "exit when |spread| falls below this")
	maxHold := flag.Float64("max-hold", 168, "maximum hold, hours")
	minHold := flag.Float64("min-hold", 8, "minimum hold before an exit is allowed, hours")
	cooldown := flag.Float64("cooldown", 24, "wait this long after an exit before re-entering the same listing, hours")
	feeBps := flag.Float64("fees", 21.0, "round-trip FEES, four legs (binance 5.0 + bybit 5.5, twice)")
	slipBps := flag.Float64("slippage", 15.0, "round-trip SLIPPAGE, ASSUMED -- no historical books exist")
	notional := flag.Float64("notional", 400, "USD per leg")
	top := flag.Int("top", 30, "rows to print")
	flag.Parse()

	roundTrip := *feeBps + *slipBps
	now := time.Now().UnixMilli()
	cutoff := now - int64(*days)*86400000

	// --- listings ---
	var info struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			Status       string `json:"status"`
			ContractType string `json:"contractType"`
			QuoteAsset   string `json:"quoteAsset"`
			OnboardDate  int64  `json:"onboardDate"`
		} `json:"symbols"`
	}
	if err := get("https://fapi.binance.com/fapi/v1/exchangeInfo", &info); err != nil {
		fmt.Fprintln(os.Stderr, "exchangeInfo:", err)
		os.Exit(1)
	}
	var listings []listing
	for _, s := range info.Symbols {
		if s.Status == "TRADING" && s.ContractType == "PERPETUAL" &&
			s.QuoteAsset == "USDT" && s.OnboardDate > cutoff {
			listings = append(listings, listing{s.Symbol, s.OnboardDate})
		}
	}
	sort.Slice(listings, func(i, j int) bool { return listings[i].onboard > listings[j].onboard })

	// --- intervals ---
	binIvl := map[string]float64{}
	var fi []struct {
		Symbol string `json:"symbol"`
		Hours  int    `json:"fundingIntervalHours"`
	}
	_ = get("https://fapi.binance.com/fapi/v1/fundingInfo", &fi)
	for _, r := range fi {
		if r.Hours > 0 {
			binIvl[r.Symbol] = float64(r.Hours)
		}
	}

	// --- bybit universe + intervals ---
	byIvl := map[string]float64{}
	var bi struct {
		Result struct {
			List []struct {
				Symbol          string `json:"symbol"`
				FundingInterval int    `json:"fundingInterval"`
			} `json:"list"`
		} `json:"result"`
	}
	_ = get("https://api.bybit.com/v5/market/instruments-info?category=linear&limit=1000", &bi)
	for _, i := range bi.Result.List {
		if i.FundingInterval > 0 {
			byIvl[i.Symbol] = float64(i.FundingInterval) / 60
		}
	}

	fmt.Printf("NEW-LISTING FUNDING BACKTEST\n\n")
	fmt.Printf("  listings in the last %d days   %d\n", *days, len(listings))
	fmt.Printf("  hedgeable on bybit             %d\n", countHedgeable(listings, byIvl))
	fmt.Printf("  window per listing             %d days\n", *window)
	fmt.Printf("  enter above                    %.2f bps/hr, exit below %.2f\n", *minSpread, *exitSpread)
	fmt.Printf("  round trip                     %.1f bps  (%.1f fees + %.1f ASSUMED slippage)\n\n",
		roundTrip, *feeBps, *slipBps)

	hours := *window * 24
	var results []result

	for _, l := range listings {
		if _, ok := byIvl[l.sym]; !ok {
			results = append(results, result{Symbol: l.sym,
				Listed: time.UnixMilli(l.onboard).UTC(), Hedgeable: false})
			continue
		}

		binS := binanceFunding(l.sym, l.onboard, hours)
		byS := bybitFunding(l.sym, l.onboard, hours)
		if len(binS) < 5 || len(byS) < 5 {
			continue
		}
		bh := binIvl[l.sym]
		if bh == 0 {
			bh = 8
		}
		a := hourly(binS, bh, l.onboard, hours)
		b := hourly(byS, byIvl[l.sym], l.onboard, hours)

		r := simulate(l.sym, time.UnixMilli(l.onboard).UTC(), a, b,
			*minSpread, *exitSpread, *minHold, *maxHold, *cooldown, roundTrip)
		r.Hedgeable = true
		results = append(results, r)
		time.Sleep(120 * time.Millisecond)
	}

	// --- report ---
	traded := results[:0]
	for _, r := range results {
		if r.Hedgeable && r.Trades > 0 {
			traded = append(traded, r)
		}
	}
	sort.Slice(traded, func(i, j int) bool { return traded[i].NetBps > traded[j].NetBps })

	fmt.Printf("%-16s %-12s %6s %10s %10s %11s %11s\n",
		"SYMBOL", "LISTED", "TRADES", "NET bps", "HELD h", "BEST bps", "MAX bps/hr")
	fmt.Println("---------------------------------------------------------------------------------")
	for i, r := range traded {
		if i >= *top {
			fmt.Printf("  ... %d more\n", len(traded)-*top)
			break
		}
		fmt.Printf("%-16s %-12s %6d %+10.1f %10.0f %+11.1f %11.3f\n",
			r.Symbol, r.Listed.Format("2006-01-02"), r.Trades,
			r.NetBps, r.HeldHours, r.BestBps, r.MaxSpread)
	}

	var totalNet, totalHeld float64
	wins, losses := 0, 0
	trades := 0
	for _, r := range traded {
		totalNet += r.NetBps
		totalHeld += r.HeldHours
		trades += r.Trades
		if r.NetBps > 0 {
			wins++
		} else {
			losses++
		}
	}

	fmt.Printf("\nAGGREGATE\n\n")
	fmt.Printf("  listings that produced a trade   %d of %d\n", len(traded), len(listings))
	fmt.Printf("  trades                           %d\n", trades)
	fmt.Printf("  profitable listings              %d   losing %d\n", wins, losses)
	fmt.Printf("  total net                        %+.1f bps of notional\n", totalNet)
	fmt.Printf("  total capital-hours              %.0f  (at $%.0f/leg, $%.0f capital)\n",
		totalHeld, *notional, *notional*2)

	if totalHeld > 0 {
		netUSD := totalNet / 10000 * *notional
		capHours := totalHeld * *notional * 2
		rateDeployed := netUSD / capHours * 8760 * 100
		fmt.Printf("  net in dollars                   $%+.2f\n", netUSD)
		fmt.Printf("  rate WHILE DEPLOYED              %+.1f%%/yr\n", rateDeployed)

		// The old "utilisation" line was nonsense: 112 independent windows
		// across a year overlap, so summed held-hours exceeded the year and
		// produced 192%. What a book actually earns depends on how many
		// positions it can run at once, so state it that way.
		yearHours := float64(*days) * 24
		fmt.Printf("\n  IF THE BOOK COULD HOLD N POSITIONS AT ONCE:\n")
		for _, n := range []float64{1, 3, 5} {
			cap := yearHours * n
			taken := totalHeld
			if taken > cap {
				taken = cap
			}
			frac := taken / totalHeld
			fmt.Printf("    %.0f concurrent   captures %.0f%% of the opportunity   $%+.2f   %+.1f%%/yr on $%.0f\n",
				n, frac*100, netUSD*frac, netUSD*frac/(*notional*2*n)*100*(8760/yearHours), *notional*2*n)
		}
	}

	fmt.Print(`
READ THE SLIPPAGE FLAG BEFORE THE RESULT. Historical order books do not exist,
so the cost of crossing a days-old listing's spread is an assumption, not a
measurement. Bybit held as little as $924 on a recent listing -- at $400 a leg
that is most of the book. Re-run with -slippage 40 and see whether the answer
survives; if it does not, the strategy is a fee-schedule bet rather than a
funding one.

Fills are assumed perfect and fees are today's, applied backwards. Both flatter
the result.
`)
}

func countHedgeable(ls []listing, by map[string]float64) int {
	n := 0
	for _, l := range ls {
		if _, ok := by[l.sym]; ok {
			n++
		}
	}
	return n
}

func binanceFunding(sym string, onboard int64, hours int) map[int64]float64 {
	out := map[int64]float64{}
	start := onboard
	end := onboard + int64(hours)*3_600_000
	for page := 0; page < 4; page++ {
		var rows []struct {
			FundingTime int64  `json:"fundingTime"`
			FundingRate string `json:"fundingRate"`
		}
		u := fmt.Sprintf("https://fapi.binance.com/fapi/v1/fundingRate?symbol=%s&startTime=%d&endTime=%d&limit=1000",
			sym, start, end)
		if err := get(u, &rows); err != nil || len(rows) == 0 {
			break
		}
		for _, r := range rows {
			out[r.FundingTime] = num(r.FundingRate)
		}
		next := rows[len(rows)-1].FundingTime + 1
		if next <= start || next >= end {
			break
		}
		start = next
		time.Sleep(120 * time.Millisecond)
	}
	return out
}

func bybitFunding(sym string, onboard int64, hours int) map[int64]float64 {
	out := map[int64]float64{}
	end := onboard + int64(hours)*3_600_000
	cursorEnd := end
	for page := 0; page < 4; page++ {
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
			sym, onboard, cursorEnd)
		if err := get(u, &env); err != nil || env.RetCode != 0 || len(env.Result.List) == 0 {
			break
		}
		oldest := int64(0)
		for _, r := range env.Result.List {
			ts, _ := strconv.ParseInt(r.FundingRateTimestamp, 10, 64)
			out[ts] = num(r.FundingRate)
			if oldest == 0 || ts < oldest {
				oldest = ts
			}
		}
		if oldest <= onboard {
			break
		}
		cursorEnd = oldest - 1
		time.Sleep(120 * time.Millisecond)
	}
	return out
}

// simulate walks the hourly spread series and trades it.
//
// Long the venue paying less, short the venue paying more, so the spread is
// earned whichever way round it sits. Entry costs the full round trip up front
// -- a position is not profitable until it has covered getting out as well as in.
// ANTI-CHURN, WHICH THE FIRST VERSION LACKED.
//
// Without a minimum hold and a re-entry cooldown the rule flip-flops around
// the threshold: 1,242 entries across 112 listings, 11 each, each paying a full
// round trip. That produced 44,712 bps of costs against 35,441 bps of funding
// -- the strategy lost on re-entry, not on funding.
//
// The live book has both rules already, for exactly this reason. Leaving them
// out of the backtest tested a strategy nobody would run.
func simulate(sym string, listed time.Time, a, b []float64,
	minSpread, exitSpread, minHold, maxHold, cooldown, roundTrip float64) result {

	r := result{Symbol: sym, Listed: listed}
	inPos := false
	var acc, held, sinceExit float64
	sinceExit = cooldown // free to enter at listing

	for i := 0; i < len(a) && i < len(b); i++ {
		if math.IsNaN(a[i]) || math.IsNaN(b[i]) {
			continue
		}
		spread := math.Abs(a[i] - b[i])
		if spread > r.MaxSpread {
			r.MaxSpread = spread
		}

		if !inPos {
			sinceExit++
			if spread >= minSpread && sinceExit >= cooldown {
				inPos = true
				acc = -roundTrip
				held = 0
				r.Trades++
			}
			continue
		}

		acc += spread
		held++

		if (spread < exitSpread && held >= minHold) || held >= maxHold {
			r.NetBps += acc
			r.HeldHours += held
			if acc > r.BestBps {
				r.BestBps = acc
			}
			if acc < r.WorstBps {
				r.WorstBps = acc
			}
			inPos = false
			sinceExit = 0
		}
	}
	if inPos {
		r.NetBps += acc
		r.HeldHours += held
	}
	return r
}
