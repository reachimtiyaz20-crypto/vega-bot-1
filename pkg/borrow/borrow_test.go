package borrow

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// --- THE UNIT TRAP ------------------------------------------------------------

// TestBybitHourlyConvertsCorrectly.
//
// Measured 2026-08-15: 0.0000044842550 per hour = 3.93% a year.
func TestBybitHourlyConvertsCorrectly(t *testing.T) {
	r := Rate{Venue: "bybit", Currency: "USDT", RawRate: 0.000004484255, Period: PeriodHourly}
	if err := r.normalise(true); err != nil {
		t.Fatalf("refused a real rate: %v", err)
	}
	if !near(r.AnnualPct, 3.928, 0.01) {
		t.Fatalf("annual %.4f%%, want 3.928", r.AnnualPct)
	}
}

// TestOKXDailyConvertsCorrectly.
//
// Measured 2026-08-15: 0.0000768 per day = 2.80% a year.
func TestOKXDailyConvertsCorrectly(t *testing.T) {
	r := Rate{Venue: "okx", Currency: "USDT", RawRate: 0.0000768, Period: PeriodDaily}
	if err := r.normalise(true); err != nil {
		t.Fatalf("refused a real rate: %v", err)
	}
	if !near(r.AnnualPct, 2.803, 0.01) {
		t.Fatalf("annual %.4f%%, want 2.803", r.AnnualPct)
	}
}

// TestTheSameNumberMeans24DifferentThings.
//
// This is the whole reason the package refuses an unknown period. One raw
// figure, two conventions, a 24x difference -- and 67% is high enough to look
// like a squeeze rather than a bug.
func TestTheSameNumberMeans24DifferentThings(t *testing.T) {
	const raw = 0.0000768

	daily := Rate{Venue: "x", Currency: "BTC", RawRate: raw, Period: PeriodDaily}
	hourly := Rate{Venue: "x", Currency: "BTC", RawRate: raw, Period: PeriodHourly}

	// BTC, not a stablecoin, so the guard does not fire and both convert.
	if err := daily.normalise(false); err != nil {
		t.Fatal(err)
	}
	if err := hourly.normalise(false); err != nil {
		t.Fatal(err)
	}
	if !near(daily.AnnualPct, 2.803, 0.01) {
		t.Fatalf("daily %.4f%%, want 2.803", daily.AnnualPct)
	}
	if !near(hourly.AnnualPct, 67.28, 0.05) {
		t.Fatalf("hourly %.4f%%, want 67.28", hourly.AnnualPct)
	}
	if ratio := hourly.AnnualPct / daily.AnnualPct; !near(ratio, 24, 1e-6) {
		t.Fatalf("ratio %.4f, want exactly 24", ratio)
	}
}

// TestTheGuardCatchesTheActualMistake.
//
// The bound was set at 100 first. 67.28% passed it. A guard that misses the one
// error it exists for is decoration -- hence 50.
func TestTheGuardCatchesTheActualMistake(t *testing.T) {
	r := Rate{Venue: "okx", Currency: "USDT", RawRate: 0.0000768, Period: PeriodHourly}
	err := r.normalise(true)
	if err == nil {
		t.Fatalf("accepted %.2f%% APR for a stablecoin -- this is the daily rate "+
			"read as hourly and the guard let it through", r.AnnualPct)
	}
	if !strings.Contains(err.Error(), "24x") {
		t.Fatalf("refusal does not name the error: %v", err)
	}
}

// TestUnknownPeriodIsRefused.
func TestUnknownPeriodIsRefused(t *testing.T) {
	r := Rate{Venue: "mystery", Currency: "USDT", RawRate: 0.00001}
	if err := r.normalise(true); err == nil {
		t.Fatal("normalised a rate whose quoting period is unknown")
	}
}

// TestNonStablecoinsMayBeExpensive.
//
// Binance quoted 0G at 0.00212747 daily = 77.65% a year. Legitimate: thin
// borrow markets are genuinely that costly. The guard must not refuse it.
func TestNonStablecoinsMayBeExpensive(t *testing.T) {
	r := Rate{Venue: "binance", Currency: "0G", RawRate: 0.00212747, Period: PeriodDaily}
	if err := r.normalise(false); err != nil {
		t.Fatalf("refused a legitimate 77%% altcoin borrow: %v", err)
	}
	if !near(r.AnnualPct, 77.65, 0.05) {
		t.Fatalf("annual %.4f%%, want 77.65", r.AnnualPct)
	}
}

func TestNegativeRateIsRefused(t *testing.T) {
	r := Rate{Venue: "x", Currency: "USDT", RawRate: -0.001, Period: PeriodDaily}
	if err := r.normalise(true); err == nil {
		t.Fatal("accepted a negative borrow rate")
	}
}

// --- cross-venue consistency --------------------------------------------------

// TestCrossVenueGapIsFlagged.
//
// Belt and braces for the case the magnitude guard cannot see: a venue that
// quietly changes its quoting convention. Two exchanges cannot price the same
// stablecoin 24x apart -- capital would move between them within minutes.
func TestCrossVenueGapIsFlagged(t *testing.T) {
	s := Snapshot{Rates: []Rate{
		{Venue: "okx", Currency: "USDT", AnnualPct: 2.80, Ok: true},
		{Venue: "bybit", Currency: "USDT", AnnualPct: 67.28, Ok: true},
	}}
	s.crossCheck()

	if len(s.Errs) == 0 {
		t.Fatal("a 24x gap between venues went unflagged")
	}
	if !strings.Contains(s.Errs[0], "quoting period") {
		t.Fatalf("warning does not name the likely cause: %s", s.Errs[0])
	}
}

func TestNormalGapIsNotFlagged(t *testing.T) {
	s := Snapshot{Rates: []Rate{
		{Venue: "okx", Currency: "USDT", AnnualPct: 2.80, Ok: true},
		{Venue: "bybit", Currency: "USDT", AnnualPct: 3.93, Ok: true},
	}}
	s.crossCheck()
	if len(s.Errs) != 0 {
		t.Fatalf("flagged a normal 1.4x spread: %v", s.Errs)
	}
}

// TestCheapestPicksTheLowest.
func TestCheapestPicksTheLowest(t *testing.T) {
	s := Snapshot{Rates: []Rate{
		{Venue: "bybit", Currency: "USDT", AnnualPct: 3.93, Ok: true, Borrowable: true},
		{Venue: "okx", Currency: "USDT", AnnualPct: 2.80, Ok: true, Borrowable: true},
		{Venue: "x", Currency: "USDT", AnnualPct: 0.10, Ok: true, Borrowable: false},
	}}
	best, ok := s.Cheapest("USDT")
	if !ok {
		t.Fatal("no cheapest found")
	}
	if best.Venue != "okx" {
		t.Fatalf("picked %s at %.2f%%; the 0.10%% one is NOT borrowable and must be ignored",
			best.Venue, best.AnnualPct)
	}
}

// --- transport ----------------------------------------------------------------

func TestBybitRetCodeFailureIsNotARate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"retCode":10001,"retMsg":"bad param","result":{}}`))
	}))
	defer srv.Close()

	b := &Bybit{HTTP: srv.Client(), BaseURL: srv.URL, VIPTier: "No VIP"}
	rates, errs := b.Rates(context.Background(), []string{"USDT"})
	if len(rates) != 0 {
		t.Fatal("a retCode failure produced a rate")
	}
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "10001") {
		t.Fatalf("error does not carry the venue's code: %v", errs)
	}
}

func TestOKXGoodResponseParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","msg":"","data":[{"basic":[
		  {"ccy":"USDT","rate":"0.0000768","quota":"5000000"},
		  {"ccy":"BTC","rate":"0.00001392","quota":"175"}
		]}]}`))
	}))
	defer srv.Close()

	o := &OKX{HTTP: srv.Client(), BaseURL: srv.URL}
	rates, errs := o.Rates(context.Background(), []string{"USDT", "BTC"})
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(rates) != 2 {
		t.Fatalf("%d rates, want 2", len(rates))
	}
	for _, r := range rates {
		switch r.Currency {
		case "USDT":
			if !near(r.AnnualPct, 2.803, 0.01) {
				t.Fatalf("USDT %.4f%%, want 2.803", r.AnnualPct)
			}
			if r.MaxBorrowUSD != 5_000_000 {
				t.Fatalf("quota %v", r.MaxBorrowUSD)
			}
		case "BTC":
			if !near(r.AnnualPct, 0.508, 0.01) {
				t.Fatalf("BTC %.4f%%, want 0.508", r.AnnualPct)
			}
		}
	}
}
