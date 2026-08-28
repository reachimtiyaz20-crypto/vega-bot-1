package funding

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// REVERSE CARRY: short spot, long perp.
//
// The mirror of cash-and-carry. Cash-and-carry earns when funding is POSITIVE
// -- longs pay shorts, and we are short the perp. Reverse carry earns when
// funding is NEGATIVE, because then shorts pay longs and we are long the perp.
//
// WHY IT IS HERE AT ALL
//
// Measured over 19 days to 2026-08-24, 811k observations: negative funding
// episodes outnumbered positive ones 7,366 to 817. Replayed as positions, cash
// -and-carry lost money at every entry threshold with a usable sample, while
// reverse carry returned +1,744 bps on taker fills alone. The abundant,
// harvestable dispersion is in the direction this project was not built for.
//
// WHAT MAKES IT A DIFFERENT TRADE, NOT A SIGN FLIP
//
// The short spot leg has to be BORROWED, and borrow is the dominant cost.
// Measured the same day: BMT lent at 336%/yr, which over the 4-hour median hold
// costs 15.4 bps against a +10 bps median net -- underwater before it starts.
// KAITO at 46% costs 2.1 bps and is fine. Ignoring borrow overstated the
// strategy by roughly 3x and, on some coins, inverted its sign.
//
// Worse than expensive: about half the coins with the best negative funding
// cannot be borrowed at all. A coin nobody will lend is exactly the coin
// everyone wants to short. Those positions are refused, not priced.
//
// THE RISK IS ALSO DIFFERENT
//
// A borrowed asset can be recalled. Short spot has unbounded loss if the hedge
// breaks. Neither is modelled here, and neither should be forgotten before this
// runs with real money.

// side returns the position's direction as a multiplier, defaulting to +1.
//
// Zero means cash-and-carry, so every position written before this field
// existed reads correctly rather than as a broken reverse.
func (p *Position) side() float64 {
	if p.Side < 0 {
		return -1
	}
	return 1
}

// IsReverse reports whether this is a short-spot position.
func (p *Position) IsReverse() bool { return p.Side < 0 }

// borrowFor returns the hourly borrow cost in bps for a symbol's base asset.
//
// Returns ok=false when no venue offered to lend it. That is a REFUSAL, not a
// zero: an unborrowable coin cannot have a short spot leg at any price, and
// treating a missing rate as free is how a strategy books income it could never
// have collected.
func (b *Book) borrowFor(symbol string) (float64, bool) {
	if b.Borrow == nil {
		return 0, false
	}
	v, ok := b.Borrow[baseAsset(symbol)]
	return v, ok
}

// baseAsset strips the quote currency from a symbol.
func baseAsset(symbol string) string {
	for _, q := range []string{"USDT", "USDC", "USD"} {
		if strings.HasSuffix(symbol, q) && len(symbol) > len(q) {
			return symbol[:len(symbol)-len(q)]
		}
	}
	return symbol
}

// borrowSnapshot mirrors one line of data/borrow/rates.jsonl.
type borrowSnapshot struct {
	Rates []struct {
		Currency   string  `json:"currency"`
		AnnualPct  float64 `json:"annual_pct"`
		Borrowable bool    `json:"borrowable"`
		OK         bool    `json:"ok"`
	} `json:"rates"`
}

// LoadBorrowRates reads the newest borrow snapshot and returns bps per hour per
// currency, taking the CHEAPEST venue where several lend the same coin.
//
// Only the newest line: the journal is a time series, and a rate from last week
// is not evidence about this hour. Borrow rises precisely when shorting demand
// rises, which is precisely when a reverse position wants to open, so a stale
// rate is optimistic in exactly the wrong direction.
//
// A missing file returns an empty map rather than an error. No borrow data
// means no reverse positions, which is the safe state.
func LoadBorrowRates(dataDir string) (map[string]float64, error) {
	path := filepath.Join(dataDir, "borrow", "rates.jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]float64{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			last = line
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if last == "" {
		return map[string]float64{}, nil
	}

	var snap borrowSnapshot
	if err := json.Unmarshal([]byte(last), &snap); err != nil {
		return nil, err
	}

	out := map[string]float64{}
	for _, r := range snap.Rates {
		if !r.OK || !r.Borrowable || r.Currency == "" {
			continue
		}
		// annual percent -> basis points per hour
		bpsHr := r.AnnualPct * 100.0 / 8760.0
		if cur, seen := out[r.Currency]; !seen || bpsHr < cur {
			out[r.Currency] = bpsHr
		}
	}
	return out, nil
}
