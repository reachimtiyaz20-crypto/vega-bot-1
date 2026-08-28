package margin

import (
	"math"
	"strings"
	"testing"
	"time"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// sched is a verified two-tier schedule: 1% maintenance up to $50k, 2.5% above.
func sched(venue, symbol string) Schedule {
	return Schedule{
		Venue:  venue,
		Symbol: symbol,
		Tiers: []Tier{
			{MaxNotionalUSD: 50_000, MaintenanceMarginRate: 0.01, MaxLeverage: 50},
			{MaxNotionalUSD: 500_000, MaintenanceMarginRate: 0.025, MaxLeverage: 20},
		},
		Source:     "test",
		VerifiedAt: time.Now(),
		Verified:   true,
	}
}

// --- the liquidation formulas, by hand ---------------------------------------

// TestLongLiquidationPrice.
//
//	P = E · (1 − 1/L) / (1 − mmr)
//	  = 100 · 0.9 / 0.99  =  90.9091
//
// A 9.09% drop kills a 10x long. The rough form (1/L − mmr) says 9%; this is
// the figure the venue actually uses.
func TestLongLiquidationPrice(t *testing.T) {
	l := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long,
		EntryPrice: 100, QtyBase: 1, Leverage: 10}

	liq, err := l.LiquidationPrice(sched("binance", "TESTUSDT"))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if !near(liq, 90.9091, 1e-3) {
		t.Fatalf("liquidation at %.4f, want 90.9091", liq)
	}

	// And the state at that price must show a margin ratio of 1.
	st, err := l.Isolated(liq, sched("binance", "TESTUSDT"))
	if err != nil {
		t.Fatalf("Isolated: %v", err)
	}
	if !near(st.MarginRatio, 1, 1e-4) {
		t.Fatalf("margin ratio %.6f at the liquidation price, want 1.0", st.MarginRatio)
	}
	if !st.Liquidated {
		t.Fatal("not flagged liquidated at its own liquidation price")
	}
}

// TestShortLiquidationPrice.
//
//	P = E · (1 + 1/L) / (1 + mmr)  =  100 · 1.1 / 1.01  =  108.9109
func TestShortLiquidationPrice(t *testing.T) {
	l := Leg{Venue: "bybit", Symbol: "TESTUSDT", Side: Short,
		EntryPrice: 100, QtyBase: 1, Leverage: 10}

	liq, err := l.LiquidationPrice(sched("bybit", "TESTUSDT"))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if !near(liq, 108.9109, 1e-3) {
		t.Fatalf("liquidation at %.4f, want 108.9109", liq)
	}

	st, _ := l.Isolated(liq, sched("bybit", "TESTUSDT"))
	if !near(st.MarginRatio, 1, 1e-4) {
		t.Fatalf("margin ratio %.6f, want 1.0", st.MarginRatio)
	}
}

// TestLeverageMovesTheDeathLine.
//
// The table that decides whether this whole strategy is survivable.
func TestLeverageMovesTheDeathLine(t *testing.T) {
	// Entry 100, maintenance 1%. The right column is what actually matters:
	// how far the asset can fall before the leg is closed.
	//
	//	leverage   liquidation price   survives a drop of
	//	   2x            50.51               49.5%
	//	   3x            67.34               32.7%
	//	   5x            80.81               19.2%
	//	  10x            90.91                9.1%   <- an ordinary alt day
	//	  20x            95.96                4.0%
	want := map[float64]float64{
		2:  50.51, // 100 × 0.5    / 0.99
		3:  67.34, // 100 × 0.6667 / 0.99
		5:  80.81, // 100 × 0.8    / 0.99
		10: 90.91, // 100 × 0.9    / 0.99
		20: 95.96, // 100 × 0.95   / 0.99
	}
	for lev, wantPx := range want {
		l := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long,
			EntryPrice: 100, QtyBase: 1, Leverage: lev}
		liq, err := l.LiquidationPrice(sched("binance", "TESTUSDT"))
		if err != nil {
			t.Fatalf("%.0fx refused: %v", lev, err)
		}
		if !near(liq, wantPx, 0.02) {
			t.Fatalf("%.0fx liquidates at %.2f, want %.2f", lev, liq, wantPx)
		}
	}
}

// --- refusals -----------------------------------------------------------------

// TestUnverifiedScheduleRefuses.
//
// A maintenance rate nobody has checked produces a confident, wrong
// liquidation price. That is worse than no answer, and it is the same failure
// as the hardcoded funding interval.
func TestUnverifiedScheduleRefuses(t *testing.T) {
	s := sched("binance", "TESTUSDT")
	s.Verified = false

	l := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long,
		EntryPrice: 100, QtyBase: 1, Leverage: 10}

	if _, err := l.LiquidationPrice(s); err == nil {
		t.Fatal("computed a liquidation price from an unverified schedule")
	}
	if _, err := l.Isolated(100, s); err == nil {
		t.Fatal("computed margin state from an unverified schedule")
	}
}

// TestNotionalAboveEveryTierRefuses. A position larger than the venue's top
// bracket is not permitted; clamping to the last tier would model a position
// the exchange would never have opened.
func TestNotionalAboveEveryTierRefuses(t *testing.T) {
	l := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long,
		EntryPrice: 100, QtyBase: 100_000, Leverage: 10} // $10m

	if _, err := l.Isolated(100, sched("binance", "TESTUSDT")); err == nil {
		t.Fatal("accepted a position above every risk-limit tier")
	}
}

// TestTierChosenOnCurrentNotional.
//
// A position that grows into a higher bracket is liquidated on the HIGHER
// rate. Pricing it on the entry bracket understates risk exactly when the
// position is biggest.
func TestTierChosenOnCurrentNotional(t *testing.T) {
	s := sched("binance", "TESTUSDT")

	// Entry $40k -- tier 1 at 1%.
	l := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long,
		EntryPrice: 100, QtyBase: 400, Leverage: 5}

	st, err := l.Isolated(100, s)
	if err != nil {
		t.Fatalf("at entry: %v", err)
	}
	if !near(st.MMR, 0.01, 1e-9) {
		t.Fatalf("MMR %.4f at $40k, want 0.01", st.MMR)
	}

	// Price doubles -> $80k notional -> tier 2 at 2.5%.
	st, err = l.Isolated(200, s)
	if err != nil {
		t.Fatalf("after the move: %v", err)
	}
	if !near(st.MMR, 0.025, 1e-9) {
		t.Fatalf("MMR %.4f at $80k, want 0.025 -- the tier was taken from ENTRY size", st.MMR)
	}
}

// TestNoCollateralRefuses.
func TestNoCollateralRefuses(t *testing.T) {
	l := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long, EntryPrice: 100, QtyBase: 1}
	if _, err := l.Isolated(100, sched("binance", "TESTUSDT")); err == nil {
		t.Fatal("computed margin with no leverage and no collateral")
	}
}

// --- THE POINT OF THE PACKAGE -------------------------------------------------

// TestCrossVenueHedgeDoesNotSaveTheLeg.
//
// The finding this whole exercise rests on. A perfectly hedged position, at
// 10x, on TWO venues. P&L is flat. The long leg is dead anyway, because
// Binance cannot see the Bybit short.
func TestCrossVenueHedgeDoesNotSaveTheLeg(t *testing.T) {
	long := Leg{Venue: "binance", Symbol: "KAITOUSDT", Side: Long,
		EntryPrice: 100, QtyBase: 4, Leverage: 10} // $400 notional
	short := Leg{Venue: "bybit", Symbol: "KAITOUSDT", Side: Short,
		EntryPrice: 100, QtyBase: 4, Leverage: 10}

	const drop = 85.0 // a 15% fall -- an ordinary day for an alt

	lState, err := long.Isolated(drop, sched("binance", "KAITOUSDT"))
	if err != nil {
		t.Fatalf("long: %v", err)
	}
	sState, err := short.Isolated(drop, sched("bybit", "KAITOUSDT"))
	if err != nil {
		t.Fatalf("short: %v", err)
	}

	// Net P&L across the pair is zero.
	net := lState.UnrealizedUSD + sState.UnrealizedUSD
	if !near(net, 0, 1e-9) {
		t.Fatalf("net P&L %.4f on a perfect hedge, want 0", net)
	}

	// And the long is still dead.
	if !lState.Liquidated {
		t.Fatalf("long survived a 15%% drop at 10x (equity $%.2f); the isolated model "+
			"is not modelling isolation", lState.EquityUSD)
	}
	if sState.Liquidated {
		t.Fatal("the short died too -- only one leg should")
	}
}

// TestSingleVenuePortfolioSurvivesTheSameMove.
//
// Identical legs, identical prices, identical leverage -- but ONE margin
// account. The exchange sees the hedge and nothing happens. This is the entire
// argument for single-venue over cross-venue.
func TestSingleVenuePortfolioSurvivesTheSameMove(t *testing.T) {
	legs := []Leg{
		{Venue: "bybit", Symbol: "KAITOUSDT", Side: Long, EntryPrice: 100, QtyBase: 4, Leverage: 10},
		{Venue: "bybit", Symbol: "KAITO-PERP", Side: Short, EntryPrice: 100, QtyBase: 4, Leverage: 10},
	}
	prices := map[string]float64{
		"bybit|KAITOUSDT":  85,
		"bybit|KAITO-PERP": 85,
	}
	schedules := map[string]Schedule{
		"bybit|KAITOUSDT":  sched("bybit", "KAITOUSDT"),
		"bybit|KAITO-PERP": sched("bybit", "KAITO-PERP"),
	}

	ps, err := Portfolio(legs, prices, schedules, 0)
	if err != nil {
		t.Fatalf("Portfolio: %v", err)
	}
	if ps.Liquidated {
		t.Fatalf("a netted hedge was liquidated: equity $%.2f, maintenance $%.2f",
			ps.EquityUSD, ps.MaintenanceUSD)
	}
	if !near(ps.NetDeltaUSD, 0, 1e-6) {
		t.Fatalf("net delta $%.4f, want 0", ps.NetDeltaUSD)
	}
	// Collateral was $80; the legs offset, so equity is untouched.
	if !near(ps.EquityUSD, 80, 1e-6) {
		t.Fatalf("equity $%.4f, want 80", ps.EquityUSD)
	}
	if ps.MarginRatio > 0.10 {
		t.Fatalf("margin ratio %.1f%% on a netted hedge", ps.MarginRatio*100)
	}
}

// TestMissingPriceIsAnErrorNotZero.
//
// A leg priced at zero looks like a total loss and would report a liquidation
// that never happened.
func TestMissingPriceIsAnErrorNotZero(t *testing.T) {
	legs := []Leg{{Venue: "bybit", Symbol: "AAA", Side: Long, EntryPrice: 100, QtyBase: 1, Leverage: 5}}
	_, err := Portfolio(legs, map[string]float64{}, map[string]Schedule{
		"bybit|AAA": sched("bybit", "AAA"),
	}, 0)
	if err == nil {
		t.Fatal("priced a leg with no price")
	}
	if !strings.Contains(err.Error(), "no price") {
		t.Fatalf("wrong error: %v", err)
	}
}

// TestNegativeEquityIsLiquidatedNotDivided.
func TestNegativeEquityIsLiquidatedNotDivided(t *testing.T) {
	l := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long,
		EntryPrice: 100, QtyBase: 1, Leverage: 10}

	st, err := l.Isolated(50, sched("binance", "TESTUSDT")) // wiped out
	if err != nil {
		t.Fatalf("Isolated: %v", err)
	}
	if !st.Liquidated {
		t.Fatal("negative equity not flagged as liquidated")
	}
	if !math.IsInf(st.MarginRatio, 1) {
		t.Fatalf("margin ratio %.4f on negative equity, want +Inf", st.MarginRatio)
	}
}

// TestExtraCollateralIsHonoured. A buffer must lower the effective leverage,
// not be ignored.
func TestExtraCollateralIsHonoured(t *testing.T) {
	base := Leg{Venue: "binance", Symbol: "TESTUSDT", Side: Long,
		EntryPrice: 100, QtyBase: 1, Leverage: 10}
	buffered := base
	buffered.CollateralUSD = 30 // 3x effective rather than 10x

	a, _ := base.LiquidationPrice(sched("binance", "TESTUSDT"))
	b, _ := buffered.LiquidationPrice(sched("binance", "TESTUSDT"))

	if b >= a {
		t.Fatalf("extra collateral did not move the liquidation price: %.4f vs %.4f", b, a)
	}
	if !near(b, 70.707, 1e-2) { // (1 − 0.3)/0.99
		t.Fatalf("buffered liquidation at %.4f, want 70.707", b)
	}
}
