package economics

import (
	"errors"
	"math"
	"testing"
)

// binanceExact is the brief's fee model with no slippage allowance: exactly
// 30 bps round trip. Used to reproduce the post-mortem arithmetic to the
// decimal place.
var binanceExact = FeeSchedule{SpotTakerBps: 10, FuturesTakerBps: 5, SlippageBpsPerLeg: 0}

func approx(t *testing.T, got, want, tol float64, label string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (tol %.6f)", label, got, want, tol)
	}
}

// --- Bug 1: no fee model at all -------------------------------------------

func TestRoundTripIsThirtyBps(t *testing.T) {
	approx(t, binanceExact.RoundTripBps(), 30.0, 1e-9, "RoundTripBps")
	approx(t, binanceExact.EntryBps(), 15.0, 1e-9, "EntryBps")
}

// A zero-value FeeSchedule must not be usable. The previous bot's effective
// fee model WAS the zero value -- it just never existed as a type. If this
// test ever passes with Viable=true, Bug 1 has come back.
func TestZeroFeeScheduleIsRejected(t *testing.T) {
	var none FeeSchedule

	if err := none.Validate(); !errors.Is(err, ErrNoFeeModel) {
		t.Fatalf("zero FeeSchedule.Validate() = %v, want ErrNoFeeModel", err)
	}

	a, err := Assess(Opportunity{Symbol: "BTCUSDT", FundingRatePct: 1.0, NotionalUSD: 10000}, none, 30)
	if err == nil {
		t.Fatal("Assess with zero fees returned nil error")
	}
	if a.Viable {
		t.Fatal("Assess with zero fees returned Viable=true; this is exactly how the last bot lost money")
	}
}

func TestNegativeFeeComponentRejected(t *testing.T) {
	bad := FeeSchedule{SpotTakerBps: -10, FuturesTakerBps: 5}
	if err := bad.Validate(); err == nil {
		t.Fatal("negative fee component accepted")
	}
}

// --- Bug 2: entry threshold incompatible with hold time --------------------

// The exact post-mortem number. MIN_FUNDING_RATE=0.005 with
// MAX_POSITION_HOURS=72 produced -25.5 bps per cycle, deterministically.
func TestPreviousBotConfigLosesTwentyFivePointFiveBps(t *testing.T) {
	a, err := Assess(Opportunity{
		Symbol:         "BTCUSDT",
		FundingRatePct: 0.005,
		NotionalUSD:    10000,
	}, binanceExact, 3)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}

	approx(t, a.ExpectedFundingBps, 4.5, 1e-9, "revenue over 3d")
	approx(t, a.CostBps, 30.0, 1e-9, "cost")
	approx(t, a.NetBps, -25.5, 1e-9, "net")
	approx(t, a.NetUSD, -25.5, 1e-9, "net USD on 10k")
	approx(t, a.BreakEvenDays, 20.0, 1e-9, "break-even days")

	if a.Viable {
		t.Fatal("the configuration that lost money for a year assessed as viable")
	}
	t.Logf("old config rejected: %s", a.Reason)
}

// The break-even table from Part 3 of the brief.
func TestBreakEvenTable(t *testing.T) {
	cases := []struct {
		ratePct float64
		days    float64
	}{
		{0.005, 20.0},
		{0.010, 10.0},
		{0.030, 3.3333333},
		{0.100, 1.0},
	}
	for _, c := range cases {
		got := BreakEvenDays(c.ratePct, binanceExact, IntervalsPerDay)
		approx(t, got, c.days, 1e-6, "BreakEvenDays")
	}
}

// The minimum-viable-rate table from Part 3.
func TestMinViableRateTable(t *testing.T) {
	cases := []struct {
		holdDays float64
		ratePct  float64
	}{
		{3, 0.0333333},
		{7, 0.0142857},
		{14, 0.0071428},
		{30, 0.0033333},
	}
	for _, c := range cases {
		got := MinViableRatePct(binanceExact, c.holdDays)
		approx(t, got, c.ratePct, 1e-6, "MinViableRatePct")
	}
}

// The entry gate must sit at MinViableRatePct.
//
// Deliberately NOT asserting viability at the exact boundary: MinViableRatePct
// divides and Assess multiplies back, so the net lands within ~1e-14 bps of
// zero and which side it falls on is down to IEEE-754 rounding, not policy.
// Asserting on that would be a test that fails on a different CPU. What
// matters is that the gate is in the right PLACE, so probe either side of it.
func TestGateSitsAtMinViableRate(t *testing.T) {
	min := MinViableRatePct(binanceExact, 7)

	at, err := Assess(Opportunity{Symbol: "X", FundingRatePct: min, NotionalUSD: 1000}, binanceExact, 7)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	approx(t, at.NetBps, 0.0, 1e-9, "net at exact minimum")

	below, _ := Assess(Opportunity{Symbol: "X", FundingRatePct: min * 0.999, NotionalUSD: 1000}, binanceExact, 7)
	if below.Viable {
		t.Fatalf("rate below minimum assessed as viable: %s", below.Reason)
	}
	if below.NetBps >= 0 {
		t.Errorf("net below minimum = %.6f bps, want negative", below.NetBps)
	}

	above, _ := Assess(Opportunity{Symbol: "X", FundingRatePct: min * 1.001, NotionalUSD: 1000}, binanceExact, 7)
	if !above.Viable {
		t.Fatalf("rate above minimum assessed as non-viable: %s", above.Reason)
	}
}

func TestClearlyViableCase(t *testing.T) {
	a, err := Assess(Opportunity{Symbol: "X", FundingRatePct: 0.10, NotionalUSD: 10000}, binanceExact, 3)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	approx(t, a.ExpectedFundingBps, 90.0, 1e-9, "revenue")
	approx(t, a.NetBps, 60.0, 1e-9, "net")
	if !a.Viable {
		t.Fatalf("0.10 pct per 8h over 3d should be viable, got: %s", a.Reason)
	}
}

// --- Bug 4: the PnL counter could not show a loss --------------------------

// Nothing in this package may clamp at zero. Sweep a range of rates and assert
// the net actually goes negative and stays ordered.
func TestNetIsSignedAndMonotonic(t *testing.T) {
	var prev = math.Inf(-1)
	sawNegative := false
	for _, rate := range []float64{-0.05, -0.01, 0.0, 0.001, 0.005, 0.01, 0.05, 0.2} {
		a, _ := Assess(Opportunity{Symbol: "X", FundingRatePct: rate, NotionalUSD: 10000}, binanceExact, 7)
		if a.NetBps < 0 {
			sawNegative = true
		}
		if a.NetBps < prev {
			t.Fatalf("net not monotonic in rate: %.4f -> %.4f", prev, a.NetBps)
		}
		prev = a.NetBps
	}
	if !sawNegative {
		t.Fatal("no negative net across the sweep; something is clamping")
	}
}

// Risk 1 from the brief: funding flips negative and the short leg begins
// paying. This must be rejected, and the loss must be visible, not floored.
func TestNegativeFundingIsRejectedAndVisible(t *testing.T) {
	a, err := Assess(Opportunity{Symbol: "X", FundingRatePct: -0.02, NotionalUSD: 10000}, binanceExact, 7)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Viable {
		t.Fatal("negative funding assessed as viable")
	}
	approx(t, a.ExpectedFundingBps, -42.0, 1e-9, "revenue when paying funding")
	approx(t, a.NetBps, -72.0, 1e-9, "net when paying funding")
	if !math.IsInf(a.BreakEvenDays, 1) {
		t.Errorf("BreakEvenDays = %v, want +Inf for negative funding", a.BreakEvenDays)
	}
}

// --- Return honesty --------------------------------------------------------

// Both legs need capital, so return on deployed capital is roughly HALF the
// annualised-on-notional figure. This is the detail retail sources omit and
// the reason a "20 percent strategy" pays 10.
func TestReturnOnCapitalIsHalfOfNotional(t *testing.T) {
	a, err := Assess(Opportunity{Symbol: "X", FundingRatePct: 0.01, NotionalUSD: 10000}, binanceExact, 365)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	approx(t, a.AnnualizedPct, 10.95, 1e-9, "annualised on notional")
	approx(t, a.CapitalUSD, 20000, 1e-9, "capital deployed")
	approx(t, a.NetAnnualOnCapital, 5.325, 1e-6, "net annual on capital")

	if a.NetAnnualOnCapital > a.AnnualizedPct/2 {
		t.Fatal("net on capital exceeds half the notional figure; fees vanished somewhere")
	}
}

// The founder has been quoted 12-15 percent monthly. Assert that no plausible
// sustained rate produces it, so the number is falsified by the model rather
// than by argument.
func TestTargetedReturnIsUnreachableAtSustainableRates(t *testing.T) {
	a, _ := Assess(Opportunity{Symbol: "X", FundingRatePct: 0.03, NotionalUSD: 10000}, binanceExact, 365)
	monthly := a.NetAnnualOnCapital / 12
	t.Logf("0.03 pct per 8h sustained all year: %.2f pct annual on capital = %.2f pct monthly",
		a.NetAnnualOnCapital, monthly)
	if monthly >= 12 {
		t.Fatalf("model produces %.2f pct monthly at 0.03 pct per 8h; check the arithmetic", monthly)
	}
}

// --- Input hygiene ---------------------------------------------------------

func TestBadHoldRejected(t *testing.T) {
	for _, hold := range []float64{0, -1} {
		if _, err := Assess(Opportunity{Symbol: "X", FundingRatePct: 0.05}, binanceExact, hold); !errors.Is(err, ErrBadHold) {
			t.Errorf("hold %v: err = %v, want ErrBadHold", hold, err)
		}
	}
}

func TestNonFiniteRateRejected(t *testing.T) {
	for _, r := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := Assess(Opportunity{Symbol: "X", FundingRatePct: r}, binanceExact, 7); err == nil {
			t.Errorf("rate %v accepted", r)
		}
	}
}

// Symbols on a 4-hour funding schedule settle 6x/day, not 3x. Break-even
// halves. If the monitor hardcodes 3 for these, it under-reports viability.
func TestFourHourScheduleHalvesBreakEven(t *testing.T) {
	a, err := Assess(Opportunity{
		Symbol: "X", FundingRatePct: 0.01, NotionalUSD: 10000,
		IntervalsPerDayOverride: 6,
	}, binanceExact, 10)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	approx(t, a.BreakEvenDays, 5.0, 1e-9, "break-even on 4h schedule")
	approx(t, a.IntervalsHeld, 60, 1e-9, "intervals over 10d")
}
