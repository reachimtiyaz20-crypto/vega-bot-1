package exchange

import (
	"math"
	"strings"
	"testing"
)

// book builds an observation with the given touch depths, measured.
func book(spotUSD, perpUSD float64) Observation {
	return Observation{
		Venue:               "test",
		Symbol:              "TESTUSDT",
		SpotSymbolAvailable: true,
		LiquidityMeasured:   true,
		SpotTopOfBookUSD:    spotUSD,
		PerpTopOfBookUSD:    perpUSD,
	}
}

func policy() SizingPolicy {
	return SizingPolicy{
		TargetNotionalUSD: 10000,
		Adaptive:          true,
		DepthFraction:     0.5,
		MinNotionalUSD:    10,
	}
}

// TestDeepBookTakesTheFullTarget. BTCUSDT holds far more than $10k at the
// touch, so adaptive sizing must be a no-op there. If this fails, the feature
// is quietly shrinking the only positions that actually work.
func TestDeepBookTakesTheFullTarget(t *testing.T) {
	s := policy().Size(book(5_000_000, 4_000_000))
	if !s.Ok {
		t.Fatalf("deep book refused: %s", s.Reason)
	}
	if s.NotionalUSD != 10000 {
		t.Fatalf("want the full 10000, got %v", s.NotionalUSD)
	}
	if s.Reduced {
		t.Fatal("a deep book should not reduce the size")
	}
	if s.LimitedBy != "target" {
		t.Fatalf("want limited by target, got %q", s.LimitedBy)
	}
}

// TestSmallerLegBinds is the B3USDT case, measured 2026-08-11: $20.30 resting
// on spot against $1.65 on the perp. Sizing against spot alone would produce a
// position that cannot be hedged.
func TestSmallerLegBinds(t *testing.T) {
	s := policy().Size(book(20.30, 1.65))
	if s.Ok {
		t.Fatalf("sized %v against a $1.65 perp book; that position cannot be hedged", s.NotionalUSD)
	}
	if !strings.Contains(s.Reason, "perp book") {
		t.Fatalf("refusal does not name the binding leg: %s", s.Reason)
	}
}

// TestPerpLegNamedWhenItBinds, at sizes above the exchange minimum.
func TestPerpLegNamedWhenItBinds(t *testing.T) {
	// spot 0.5*2000 = 1000, perp 0.5*400 = 200 -> perp binds
	s := policy().Size(book(2000, 400))
	if !s.Ok {
		t.Fatalf("refused a sizeable book: %s", s.Reason)
	}
	if math.Abs(s.NotionalUSD-200) > 1e-9 {
		t.Fatalf("want 200, got %v", s.NotionalUSD)
	}
	if s.LimitedBy != "perp book" {
		t.Fatalf("want perp book, got %q", s.LimitedBy)
	}
	if !s.Reduced {
		t.Fatal("size was cut but Reduced is false")
	}
}

func TestSpotLegNamedWhenItBinds(t *testing.T) {
	s := policy().Size(book(400, 2000))
	if !s.Ok || s.LimitedBy != "spot book" {
		t.Fatalf("want spot book binding, got %q (ok=%v)", s.LimitedBy, s.Ok)
	}
	if math.Abs(s.NotionalUSD-200) > 1e-9 {
		t.Fatalf("want 200, got %v", s.NotionalUSD)
	}
}

// TestBelowExchangeMinimumIsRefused.
//
// This is not fastidiousness about small numbers. Binance rejects orders under
// roughly $5 notional. If the SECOND leg is rejected, the first leg is already
// filled and the position is naked -- so a size below the floor must never be
// attempted.
func TestBelowExchangeMinimumIsRefused(t *testing.T) {
	s := policy().Size(book(10, 10)) // 0.5 * 10 = $5, under the $10 floor
	if s.Ok {
		t.Fatalf("accepted $%.2f, below the exchange minimum", s.NotionalUSD)
	}
	if !strings.Contains(s.Reason, "naked first leg") {
		t.Fatalf("refusal does not explain the real danger: %s", s.Reason)
	}
}

// TestUnmeasuredBookIsRefusedNotAssumed. Falling back to the target here would
// be sizing against an assumption.
func TestUnmeasuredBookIsRefusedNotAssumed(t *testing.T) {
	o := book(5_000_000, 4_000_000)
	o.LiquidityMeasured = false

	s := policy().Size(o)
	if s.Ok {
		t.Fatalf("sized $%v against a book that was never read", s.NotionalUSD)
	}
	if s.NotionalUSD != 0 {
		t.Fatalf("refused sizing still returned %v", s.NotionalUSD)
	}
}

// TestEmptyLegIsRefused.
func TestEmptyLegIsRefused(t *testing.T) {
	for _, o := range []Observation{book(0, 1000), book(1000, 0), book(0, 0)} {
		if s := policy().Size(o); s.Ok {
			t.Fatalf("sized against an empty leg: spot %v perp %v", o.SpotTopOfBookUSD, o.PerpTopOfBookUSD)
		}
	}
}

// TestAdaptiveOffReproducesOldBehaviour, so the two can be compared on the
// same data without a second code path.
func TestAdaptiveOffReproducesOldBehaviour(t *testing.T) {
	p := policy()
	p.Adaptive = false

	s := p.Size(book(1, 1)) // absurdly thin
	if !s.Ok || s.NotionalUSD != 10000 {
		t.Fatalf("non-adaptive should always return the target, got %v (ok=%v)", s.NotionalUSD, s.Ok)
	}
}

// TestDepthFractionIsClamped. Above 1.0 the order walks past the touch and the
// measured half-spread stops describing the fill, so it must not be obeyed.
func TestDepthFractionIsClamped(t *testing.T) {
	p := policy()
	p.DepthFraction = 3

	s := p.Size(book(1000, 1000))
	if s.NotionalUSD > 1000 {
		t.Fatalf("took %v from a $1000 touch; the fill would sweep the book", s.NotionalUSD)
	}
}

// TestForBookReturnsACopy. Constraints is passed by value everywhere; a
// mutated original would carry one symbol's depth into the next symbol's
// assessment.
func TestForBookReturnsACopy(t *testing.T) {
	c := Constraints{
		NotionalUSD:              10000,
		HoldDays:                 30,
		RequireMeasuredLiquidity: true,
		Sizing:                   DefaultSizingPolicy(10000),
	}

	sized, s := c.ForBook(book(400, 2000))
	if !s.Ok {
		t.Fatalf("refused: %s", s.Reason)
	}
	if math.Abs(sized.NotionalUSD-200) > 1e-9 {
		t.Fatalf("sized constraints want 200, got %v", sized.NotionalUSD)
	}
	if c.NotionalUSD != 10000 {
		t.Fatalf("ForBook mutated the original: %v", c.NotionalUSD)
	}
}

// TestForBookFallsBackToFixedNotional, so Constraints built before this
// feature existed keep working unchanged.
func TestForBookFallsBackToFixedNotional(t *testing.T) {
	c := Constraints{NotionalUSD: 500, HoldDays: 30} // no Sizing set

	sized, s := c.ForBook(book(5_000_000, 5_000_000))
	if !s.Ok {
		t.Fatalf("refused: %s", s.Reason)
	}
	if sized.NotionalUSD != 500 {
		t.Fatalf("want the configured 500, got %v", sized.NotionalUSD)
	}
}
