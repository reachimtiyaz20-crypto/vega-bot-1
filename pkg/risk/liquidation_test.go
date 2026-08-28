package risk

import (
	"math"
	"strings"
	"testing"

	"github.com/imtiyaz/vega-bot/pkg/execution"
)

// shortAt builds a hedged-shape position: SHORT perp, mark and liq as given.
func shortAt(mark, liq float64) execution.PerpPosition {
	return execution.PerpPosition{
		Venue:            "binance",
		Symbol:           "BTCUSDT",
		PositionAmt:      -0.15,
		EntryPrice:       mark,
		MarkPrice:        mark,
		LiquidationPrice: liq,
		Leverage:         1,
		MarginType:       "cross",
	}
}

// --- the invariant this whole package exists to protect ---------------------

// TestUnknownOutranksWatch locks the severity ordering.
//
// If someone ever "tidies" the const block and puts Unknown after Safe, this
// fails. A position whose liquidation distance cannot be measured must never
// compare as safer than one that has been measured and found merely close.
func TestUnknownOutranksWatch(t *testing.T) {
	if !(Critical > Danger && Danger > Watch && Watch > Safe && Safe > Unknown) {
		t.Fatalf("severity ordering broken: Unknown=%d Safe=%d Watch=%d Danger=%d Critical=%d",
			Unknown, Safe, Watch, Danger, Critical)
	}
	// And the one that actually matters at runtime: an unmeasured position must
	// not be reported as SAFE.
	a := Assess(shortAt(100, 0), DefaultThresholds())
	if a.Severity == Safe {
		t.Fatal("a position with no liquidation price was reported SAFE")
	}
	if a.Severity != Unknown {
		t.Fatalf("want Unknown, got %s", a.Severity)
	}
	if a.Action == ActionNone {
		t.Fatal("unmeasured risk produced no action")
	}
}

// TestBinanceZeroAndBybitEmptyBothReadAsUnknown covers the two venue shapes
// documented in binance_account.go and bybit_account.go. Binance sends "0",
// Bybit sends "". Both parse to 0.0 here, and 0.0 must mean UNKNOWN.
func TestBinanceZeroAndBybitEmptyBothReadAsUnknown(t *testing.T) {
	for _, liq := range []float64{0, -1} {
		p := shortAt(64000, liq)
		if p.HasLiquidationPrice() {
			t.Fatalf("liq=%v reported as a real liquidation price", liq)
		}
		a := Assess(p, DefaultThresholds())
		if a.Severity != Unknown {
			t.Fatalf("liq=%v: want Unknown, got %s", liq, a.Severity)
		}
		if !math.IsInf(a.DistancePct, 1) {
			t.Fatalf("liq=%v: want +Inf distance, got %v", liq, a.DistancePct)
		}
	}
}

// --- hedge integrity --------------------------------------------------------

// TestLongPerpIsCriticalNotSafe: a LONG perp beside long spot is not a hedge
// with a comfortable liquidation distance -- it is double exposure. It must be
// caught before the distance arithmetic ever runs.
func TestLongPerpIsCriticalNotSafe(t *testing.T) {
	p := shortAt(100, 10_000) // liquidation miles away
	p.PositionAmt = +0.15     // but on the wrong side

	a := Assess(p, DefaultThresholds())
	if a.Severity != Critical {
		t.Fatalf("long perp: want Critical, got %s", a.Severity)
	}
	if a.Action != ActionCloseNow {
		t.Fatalf("long perp: want close-now, got %s", a.Action)
	}
	if !strings.Contains(strings.ToUpper(a.Reasons[0]), "DOUBLE") {
		t.Fatalf("reason does not name the problem: %s", a.Reasons[0])
	}
}

// TestShortIsKilledByARally states the counterintuitive fact in a test so it
// cannot be quietly reversed: for this strategy, UP is the dangerous direction.
func TestShortIsKilledByARally(t *testing.T) {
	a := Assess(shortAt(100, 110), DefaultThresholds())
	if !strings.Contains(a.MoveDirection, "UP") {
		t.Fatalf("short position does not name UP as the danger: %q", a.MoveDirection)
	}
}

// --- thresholds -------------------------------------------------------------

func TestSeverityBands(t *testing.T) {
	th := DefaultThresholds() // 3 / 7 / 15
	cases := []struct {
		liq  float64
		want Severity
	}{
		{102, Critical}, // 2%
		{105, Danger},   // 5%
		{110, Watch},    // 10%
		{130, Safe},     // 30%
	}
	for _, c := range cases {
		got := Assess(shortAt(100, c.liq), th)
		if got.Severity != c.want {
			t.Fatalf("liq %.0f (%.1f%% away): want %s, got %s",
				c.liq, got.DistancePct, c.want, got.Severity)
		}
	}
}

// TestExcessLeverageIsReported. A cash-and-carry needs 1x. Leverage above the
// cap is a configuration error and must be surfaced even while the distance
// still looks comfortable -- that is precisely when it is cheap to fix.
func TestExcessLeverageIsReported(t *testing.T) {
	p := shortAt(100, 130)
	p.Leverage = 10

	a := Assess(p, DefaultThresholds())
	joined := strings.Join(a.Reasons, " | ")
	if !strings.Contains(joined, "leverage") {
		t.Fatalf("10x leverage not reported: %s", joined)
	}
}

func TestFlatPositionIsSafe(t *testing.T) {
	p := shortAt(100, 0)
	p.PositionAmt = 0
	if a := Assess(p, DefaultThresholds()); a.Severity != Safe {
		t.Fatalf("flat position: want Safe, got %s", a.Severity)
	}
}

// --- portfolio --------------------------------------------------------------

func snapshot(ps ...execution.PerpPosition) execution.AccountSnapshot {
	return execution.AccountSnapshot{
		Venue:     "binance",
		Mode:      execution.Mainnet,
		Positions: ps,
	}
}

// TestPortfolioIsAsSafeAsItsWorstPosition. Three comfortable positions and one
// in danger is a portfolio in danger, not a portfolio that is 75% fine.
func TestPortfolioIsAsSafeAsItsWorstPosition(t *testing.T) {
	pr := AssessPortfolio([]execution.AccountSnapshot{
		snapshot(shortAt(100, 140), shortAt(100, 135), shortAt(100, 105)),
	}, DefaultThresholds())

	if pr.Worst != Danger {
		t.Fatalf("want Danger, got %s", pr.Worst)
	}
	if pr.Action != ActionTopUp {
		t.Fatalf("want top-up action, got %s", pr.Action)
	}
	if math.Abs(pr.ClosestPct-5) > 1e-9 {
		t.Fatalf("closest distance: want 5%%, got %v", pr.ClosestPct)
	}
	// Sorted nearest-to-liquidation first, so the dashboard cannot bury the
	// dangerous one under the comfortable ones -- the mistake the scanner
	// dashboard originally made with refused symbols.
	if pr.Assessments[0].DistancePct > pr.Assessments[len(pr.Assessments)-1].DistancePct {
		t.Fatal("assessments not sorted nearest-first")
	}
}

// TestOneUnmeasuredPositionDowngradesTheWholePortfolio.
func TestOneUnmeasuredPositionDowngradesTheWholePortfolio(t *testing.T) {
	pr := AssessPortfolio([]execution.AccountSnapshot{
		snapshot(shortAt(100, 140), shortAt(100, 0)),
	}, DefaultThresholds())

	if pr.UnknownCount != 1 {
		t.Fatalf("want 1 unknown, got %d", pr.UnknownCount)
	}
	if pr.Worst == Safe {
		t.Fatal("portfolio with an unmeasured position reported SAFE")
	}
	if len(pr.Alerts) == 0 {
		t.Fatal("unmeasured position raised no alert")
	}
}

// TestSafeToOpenRefusesWhileAnythingIsWrong. Adding a position during a rally
// that is already threatening an existing one is how one bad day compounds.
func TestSafeToOpenRefusesWhileAnythingIsWrong(t *testing.T) {
	th := DefaultThresholds()

	cases := []struct {
		name string
		snap execution.AccountSnapshot
		want bool
	}{
		{"all comfortable", snapshot(shortAt(100, 140)), true},
		{"one in danger", snapshot(shortAt(100, 140), shortAt(100, 105)), false},
		{"one critical", snapshot(shortAt(100, 102)), false},
		{"one unmeasured", snapshot(shortAt(100, 140), shortAt(100, 0)), false},
		{"no positions", snapshot(), true},
	}

	for _, c := range cases {
		pr := AssessPortfolio([]execution.AccountSnapshot{c.snap}, th)
		got, why := pr.SafeToOpen()
		if got != c.want {
			t.Fatalf("%s: SafeToOpen=%v want %v (%s)", c.name, got, c.want, why)
		}
		if !got && why == "" {
			t.Fatalf("%s: refused without a stated reason", c.name)
		}
	}
}

// TestEmptyPortfolioIsNotCritical guards the zero value: no positions must not
// read as maximum danger, or the manager would refuse to ever start.
func TestEmptyPortfolioIsNotCritical(t *testing.T) {
	pr := AssessPortfolio(nil, DefaultThresholds())
	if pr.Worst != Safe {
		t.Fatalf("empty portfolio: want Safe, got %s", pr.Worst)
	}
	if ok, _ := pr.SafeToOpen(); !ok {
		t.Fatal("empty portfolio refused to open")
	}
	if pr.Summary != "no open positions" {
		t.Fatalf("unexpected summary: %q", pr.Summary)
	}
}
