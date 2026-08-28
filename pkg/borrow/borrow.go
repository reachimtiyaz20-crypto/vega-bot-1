// Package borrow reads what it costs to borrow on each venue.
//
// # WHY THIS EXISTS
//
// Leverage on a cash-and-carry position multiplies (funding − borrow), not
// funding. VEGA has always measured funding and never measured borrow, so it
// has never been able to say whether leverage helps or hurts. Measured
// 2026-08-15: USDT borrows at 2.80% on OKX and 3.93% on Bybit, against net
// funding near 7% on the best symbols and 2.37% on BTC. At those rates
// levering BTC cash-and-carry LOSES money and nothing in the system knew.
//
// # THE UNIT TRAP, AGAIN
//
// Bybit publishes an HOURLY rate. OKX and Binance publish a DAILY one. The
// same number means twenty-four different things depending on which venue
// sent it, and a venue whose period is not known is REFUSED rather than
// assumed -- the identical discipline that the funding-interval bug earned the
// hard way on 2026-08-13.
//
// A stablecoin borrowing above 100% a year is not a market condition, it is a
// period mix-up. That check is here because reading OKX's daily rate as
// hourly produces exactly 67%, which looks almost plausible.
package borrow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Period is the interval a venue quotes its borrow rate over.
type Period int

const (
	// PeriodUnknown must never be assumed away.
	PeriodUnknown Period = iota
	PeriodHourly
	PeriodDaily
)

func (p Period) String() string {
	switch p {
	case PeriodHourly:
		return "hourly"
	case PeriodDaily:
		return "daily"
	}
	return "UNKNOWN"
}

// hoursIn is how many hours one quoted period covers.
func (p Period) hoursIn() (float64, bool) {
	switch p {
	case PeriodHourly:
		return 1, true
	case PeriodDaily:
		return 24, true
	}
	return 0, false
}

// maxStablecoinAPR is a unit-error guard, not a market judgement.
//
// SET TO 50, NOT 100, AND THE DIFFERENCE MATTERS. Reading OKX's daily USDT
// rate as hourly produces 67.28% -- which would have sailed under a 100%
// bound. A guard that does not catch the specific mistake it was written for
// is decoration.
//
// The cost is that a genuine stablecoin squeeze past 50% would be refused. That
// is the right trade: a refusal is visible and gets investigated, a 24x error
// is invisible and makes leverage look free.
const maxStablecoinAPR = 50.0

// Rate is one currency's borrow cost on one venue.
type Rate struct {
	Venue    string `json:"venue"`
	Currency string `json:"currency"`

	// RawRate is exactly what the venue published, untouched, with the period
	// it was published in. Everything else is derived from these two.
	RawRate float64 `json:"raw_rate"`
	Period  Period  `json:"-"`
	PeriodS string  `json:"period"`

	AnnualPct float64 `json:"annual_pct"`
	HourlyPct float64 `json:"hourly_pct"`

	MaxBorrowUSD    float64 `json:"max_borrow_usd,omitempty"`
	CollateralRatio float64 `json:"collateral_ratio,omitempty"`
	Borrowable      bool    `json:"borrowable"`

	At     time.Time `json:"at"`
	Source string    `json:"source"`
	Ok     bool      `json:"ok"`
}

// normalise fills the derived fields, refusing anything whose period is
// unknown or whose result is arithmetically impossible.
func (r *Rate) normalise(stable bool) error {
	hours, ok := r.Period.hoursIn()
	if !ok {
		return fmt.Errorf("borrow: %s %s has an UNKNOWN quoting period; the same "+
			"number means 24 different things depending on it", r.Venue, r.Currency)
	}
	if r.RawRate < 0 {
		return fmt.Errorf("borrow: %s %s published a negative rate %v", r.Venue, r.Currency, r.RawRate)
	}

	r.PeriodS = r.Period.String()
	r.HourlyPct = r.RawRate / hours * 100
	r.AnnualPct = r.RawRate / hours * 24 * 365 * 100

	if stable && r.AnnualPct > maxStablecoinAPR {
		return fmt.Errorf(
			"borrow: %s %s works out to %.1f%% a year, past the %.0f%% bound. A "+
				"stablecoin does not borrow at that rate -- this is a %s rate being read "+
				"as something else, and getting it wrong is a 24x error in the direction "+
				"that makes leverage look free",
			r.Venue, r.Currency, r.AnnualPct, maxStablecoinAPR, r.Period)
	}

	r.Ok = true
	return nil
}

// Snapshot is one reading across venues.
type Snapshot struct {
	At    time.Time `json:"at"`
	Rates []Rate    `json:"rates"`
	Errs  []string  `json:"errors,omitempty"`
}

// Cheapest returns the lowest annual rate for a currency across venues.
func (s Snapshot) Cheapest(currency string) (Rate, bool) {
	var best Rate
	found := false
	for _, r := range s.Rates {
		if !r.Ok || r.Currency != currency || !r.Borrowable {
			continue
		}
		if !found || r.AnnualPct < best.AnnualPct {
			best, found = r, true
		}
	}
	return best, found
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		snip := raw
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snip)
	}
	return json.Unmarshal(raw, out)
}

func num(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

var stablecoins = map[string]bool{"USDT": true, "USDC": true, "DAI": true, "FDUSD": true}

// --- Bybit --------------------------------------------------------------------

// Bybit reads spot-margin borrow rates. PUBLIC, no key.
//
// Publishes hourlyBorrowRate as a fraction: 0.0000044842550 per hour is
// 3.93% a year.
type Bybit struct {
	HTTP    *http.Client
	BaseURL string
	VIPTier string
}

func NewBybit() *Bybit {
	return &Bybit{
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://api.bybit.com",
		VIPTier: "No VIP",
	}
}

func (b *Bybit) Rates(ctx context.Context, currencies []string) ([]Rate, []error) {
	var out []Rate
	var errs []error

	for _, ccy := range currencies {
		u := fmt.Sprintf("%s/v5/spot-margin-trade/data?vipLevel=%s&currency=%s",
			b.BaseURL, url.QueryEscape(b.VIPTier), url.QueryEscape(ccy))

		var env struct {
			RetCode int    `json:"retCode"`
			RetMsg  string `json:"retMsg"`
			Result  struct {
				VipCoinList []struct {
					VipLevel string `json:"vipLevel"`
					List     []struct {
						Currency         string `json:"currency"`
						HourlyBorrowRate string `json:"hourlyBorrowRate"`
						MaxBorrowing     string `json:"maxBorrowingAmount"`
						CollateralRatio  string `json:"collateralRatio"`
						Borrowable       bool   `json:"borrowable"`
					} `json:"list"`
				} `json:"vipCoinList"`
			} `json:"result"`
		}
		if err := getJSON(ctx, b.HTTP, u, &env); err != nil {
			errs = append(errs, fmt.Errorf("bybit %s: %w", ccy, err))
			continue
		}
		// retCode is where Bybit's failures live; the HTTP status is 200 either way.
		if env.RetCode != 0 {
			errs = append(errs, fmt.Errorf("bybit %s: retCode %d: %s", ccy, env.RetCode, env.RetMsg))
			continue
		}

		found := false
		for _, tier := range env.Result.VipCoinList {
			for _, c := range tier.List {
				v, ok := num(c.HourlyBorrowRate)
				if !ok {
					continue
				}
				r := Rate{
					Venue: "bybit", Currency: c.Currency,
					RawRate: v, Period: PeriodHourly,
					Borrowable: c.Borrowable,
					At:         time.Now().UTC(),
					Source:     u,
				}
				r.MaxBorrowUSD, _ = num(c.MaxBorrowing)
				r.CollateralRatio, _ = num(c.CollateralRatio)

				if err := r.normalise(stablecoins[c.Currency]); err != nil {
					errs = append(errs, err)
					continue
				}
				out = append(out, r)
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("bybit %s: no usable rate in the response", ccy))
		}
		time.Sleep(80 * time.Millisecond)
	}
	return out, errs
}

// --- OKX ----------------------------------------------------------------------

// OKX reads margin borrow rates. PUBLIC, no key.
//
// Publishes a DAILY rate: 0.0000768 per day is 2.80% a year. Reading it as
// hourly gives 67%, which is exactly the mistake maxStablecoinAPR catches.
type OKX struct {
	HTTP    *http.Client
	BaseURL string
}

func NewOKX() *OKX {
	return &OKX{
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://www.okx.com",
	}
}

func (o *OKX) Rates(ctx context.Context, currencies []string) ([]Rate, []error) {
	want := map[string]bool{}
	for _, c := range currencies {
		want[c] = true
	}

	u := o.BaseURL + "/api/v5/public/interest-rate-loan-quota"
	var env struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Basic []struct {
				Ccy   string `json:"ccy"`
				Rate  string `json:"rate"`
				Quota string `json:"quota"`
			} `json:"basic"`
		} `json:"data"`
	}
	if err := getJSON(ctx, o.HTTP, u, &env); err != nil {
		return nil, []error{fmt.Errorf("okx: %w", err)}
	}
	if env.Code != "0" {
		return nil, []error{fmt.Errorf("okx: code %s: %s", env.Code, env.Msg)}
	}

	var out []Rate
	var errs []error
	for _, d := range env.Data {
		for _, b := range d.Basic {
			if len(want) > 0 && !want[b.Ccy] {
				continue
			}
			v, ok := num(b.Rate)
			if !ok {
				continue
			}
			r := Rate{
				Venue: "okx", Currency: b.Ccy,
				RawRate: v, Period: PeriodDaily,
				Borrowable: true,
				At:         time.Now().UTC(),
				Source:     u,
			}
			r.MaxBorrowUSD, _ = num(b.Quota)

			if err := r.normalise(stablecoins[b.Ccy]); err != nil {
				errs = append(errs, err)
				continue
			}
			out = append(out, r)
		}
	}
	if len(out) == 0 && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("okx: no matching currencies in the response"))
	}
	return out, errs
}

// --- collection ---------------------------------------------------------------

// Collect reads every venue and returns one snapshot.
//
// A venue that fails does NOT abort the others. A borrow rate missing from one
// exchange is a gap; a missing snapshot is a hole in the history that can
// never be filled in afterwards.
func Collect(ctx context.Context, currencies []string) Snapshot {
	s := Snapshot{At: time.Now().UTC()}

	for _, src := range []struct {
		name string
		fn   func(context.Context, []string) ([]Rate, []error)
	}{
		{"bybit", NewBybit().Rates},
		{"okx", NewOKX().Rates},
	} {
		rates, errs := src.fn(ctx, currencies)
		s.Rates = append(s.Rates, rates...)
		for _, e := range errs {
			s.Errs = append(s.Errs, e.Error())
		}
	}
	s.crossCheck()
	return s
}

// crossCheck compares the same currency across venues.
//
// The per-venue quoting period is hardcoded from each venue's documentation,
// so the residual risk is a venue CHANGING its convention without telling
// anyone. Two exchanges pricing the same stablecoin cannot legitimately differ
// by an order of magnitude -- capital would flow between them in minutes. A
// gap that wide is a period mix-up, and this catches it even when the absolute
// number looks plausible.
func (s *Snapshot) crossCheck() {
	byCcy := map[string][]Rate{}
	for _, r := range s.Rates {
		if r.Ok && stablecoins[r.Currency] {
			byCcy[r.Currency] = append(byCcy[r.Currency], r)
		}
	}
	for ccy, rs := range byCcy {
		if len(rs) < 2 {
			continue
		}
		lo, hi := rs[0], rs[0]
		for _, r := range rs {
			if r.AnnualPct < lo.AnnualPct {
				lo = r
			}
			if r.AnnualPct > hi.AnnualPct {
				hi = r
			}
		}
		if lo.AnnualPct <= 0 {
			continue
		}
		if ratio := hi.AnnualPct / lo.AnnualPct; ratio > 8 {
			s.Errs = append(s.Errs, fmt.Sprintf(
				"SUSPECT: %s borrows at %.2f%% on %s but %.2f%% on %s, a %.1fx gap. "+
					"Two venues cannot price the same stablecoin that differently -- this "+
					"reads like one of them changed its quoting period",
				ccy, lo.AnnualPct, lo.Venue, hi.AnnualPct, hi.Venue, ratio))
		}
	}
}
