package replay

import (
	"math"
	"testing"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/margin"
)

var base = time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

// sched is a verified 1% maintenance schedule.
func sched(venue, symbol string) margin.Schedule {
	return margin.Schedule{
		Venue: venue, Symbol: symbol,
		Tiers:      []margin.Tier{{MaxNotionalUSD: 1_000_000, MaintenanceMarginRate: 0.01, MaxLeverage: 50}},
		Source:     "test",
		VerifiedAt: base,
		Verified:   true,
	}
}

func schedules(venues ...string) map[string]margin.Schedule {
	m := map[string]margin.Schedule{}
	for _, v := range venues {
		m[v+"|TESTUSDT"] = sched(v, "TESTUSDT")
	}
	return m
}

// series builds a minute-by-minute path from (low, high) pairs.
func series(lowHigh ...[2]float64) Series {
	s := Series{Venue: "bybit", Symbol: "TESTUSDT"}
	for i, lh := range lowHigh {
		s.Candles = append(s.Candles, Candle{
			TsMs: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			Open: lh[1], High: lh[1], Low: lh[0], Close: lh[1],
		})
	}
	return s
}

// subject is a $400 hedged pair entered at 100 on two venues.
func subject(minutes int) Subject {
	return Subject{
		Label:       "TEST long binance / short bybit",
		Long:        LegSpec{Venue: "binance", Symbol: "TESTUSDT", EntryPrice: 100},
		Short:       LegSpec{Venue: "bybit", Symbol: "TESTUSDT", EntryPrice: 100},
		NotionalUSD: 400,
		OpenedAt:    base,
		ClosedAt:    base.Add(time.Duration(minutes) * time.Minute),
		FundingBps:  100,
		CostBps:     30,
	}
}

// --- survival -----------------------------------------------------------------

// TestQuietPathSurvives.
//
// Entry 100 at 10x. Long dies at 90.91, short at 108.91. A path between 98 and
// 102 comes nowhere near either.
func TestQuietPathSurvives(t *testing.T) {
	s := series([2]float64{98, 102}, [2]float64{99, 101}, [2]float64{98, 102})
	r := Replay(subject(3), s, schedules("binance", "bybit"), 10, ModeIsolated)

	if !r.Ok {
		t.Fatalf("skipped: %s", r.Err)
	}
	if !r.Survived {
		t.Fatalf("liquidated on a quiet path: %s at %v", r.DiedLeg, r.DiedPrice)
	}
	if r.WorstRatio > 0.20 {
		t.Fatalf("worst margin ratio %.1f%%, expected well under 20%%", r.WorstRatio*100)
	}
	// Survivors keep what the book recorded: (100 − 30) bps on $400.
	if math.Abs(r.NetUSD-2.80) > 1e-6 {
		t.Fatalf("net $%.4f, want 2.80", r.NetUSD)
	}
}

// --- death --------------------------------------------------------------------

// TestLongDiesOnTheLowNotTheClose.
//
// The candle opens and closes at 100 and touches 89 in between. At 10x the
// long is liquidated at 90.91, so it died inside that minute -- and a replay
// that read the close would report a survivor.
func TestLongDiesOnTheLowNotTheClose(t *testing.T) {
	s := series([2]float64{89, 100})
	r := Replay(subject(1), s, schedules("binance", "bybit"), 10, ModeIsolated)

	if r.Survived {
		t.Fatal("survived a wick straight through the liquidation price; " +
			"the replay is reading closes, not lows")
	}
	if r.DiedLeg != "long binance" {
		t.Fatalf("died as %q, want the long leg", r.DiedLeg)
	}
	if math.Abs(r.DiedPrice-89) > 1e-9 {
		t.Fatalf("died at %.4f, want 89 (the low)", r.DiedPrice)
	}
}

// TestShortDiesOnTheHigh. Mirror image: short liquidates at 108.91.
func TestShortDiesOnTheHigh(t *testing.T) {
	s := series([2]float64{100, 110})
	r := Replay(subject(1), s, schedules("binance", "bybit"), 10, ModeIsolated)

	if r.Survived {
		t.Fatal("survived a spike past the short's liquidation price")
	}
	if r.DiedLeg != "short bybit" {
		t.Fatalf("died as %q, want the short leg", r.DiedLeg)
	}
}

// TestLiquidationIsTerminal.
//
// Price dips to 89, then recovers to 100 and stays. The position does NOT come
// back -- the venue closed it and kept the margin. A replay that lets a dead
// position recover is how a backtest invents money.
func TestLiquidationIsTerminal(t *testing.T) {
	s := series(
		[2]float64{100, 100},
		[2]float64{89, 100}, // dies here
		[2]float64{100, 100},
		[2]float64{100, 100},
	)
	r := Replay(subject(4), s, schedules("binance", "bybit"), 10, ModeIsolated)

	if r.Survived {
		t.Fatal("a liquidated position recovered when the price did")
	}
	if r.Candles != 2 {
		t.Fatalf("walked %d candles, want 2 -- the walk continued past the death", r.Candles)
	}
	if r.NetUSD >= 0 {
		t.Fatalf("net $%+.2f after a liquidation", r.NetUSD)
	}
}

// --- THE COMPARISON THAT MATTERS ----------------------------------------------

// TestIsolatedDiesWherePortfolioSurvives.
//
// Identical legs, identical price path, identical leverage. The ONLY
// difference is whether the two positions share a margin account.
//
// At 88 the long has lost $48 against $40 of margin and is dead on its own
// venue -- while the short has gained exactly $48 somewhere the first exchange
// cannot see. In one account those cancel and nothing happens.
//
// This is the whole case for single-venue over cross-venue, in one test.
func TestIsolatedDiesWherePortfolioSurvives(t *testing.T) {
	s := series([2]float64{88, 100})

	iso := Replay(subject(1), s, schedules("binance", "bybit"), 10, ModeIsolated)
	if iso.Survived {
		t.Fatal("isolated legs survived a 12% move at 10x; they should not")
	}

	// Same path, one account.
	sub := subject(1)
	sub.Short.Venue = "binance" // both legs on one venue
	port := Replay(sub, s, schedules("binance"), 10, ModePortfolio)

	if !port.Ok {
		t.Fatalf("portfolio replay skipped: %s", port.Err)
	}
	if !port.Survived {
		t.Fatalf("a netted hedge was liquidated at %.1f%% margin ratio", port.WorstRatio*100)
	}
	if port.WorstRatio > 0.20 {
		t.Fatalf("netted worst ratio %.1f%%; the legs are not offsetting", port.WorstRatio*100)
	}
}

// TestLowerLeverageSurvivesTheSameCrash.
//
// The 12% move that kills 10x should be survivable at 3x, whose liquidation
// price is 67.34.
func TestLowerLeverageSurvivesTheSameCrash(t *testing.T) {
	s := series([2]float64{88, 100})

	if r := Replay(subject(1), s, schedules("binance", "bybit"), 10, ModeIsolated); r.Survived {
		t.Fatal("10x survived a 12% move")
	}
	r := Replay(subject(1), s, schedules("binance", "bybit"), 3, ModeIsolated)
	if !r.Survived {
		t.Fatalf("3x died on a 12%% move; its liquidation price is 67.34 (worst ratio %.1f%%)",
			r.WorstRatio*100)
	}
}

// --- refusals -----------------------------------------------------------------

func TestMissingScheduleIsSkippedNotGuessed(t *testing.T) {
	s := series([2]float64{100, 100})
	r := Replay(subject(1), s, schedules("binance"), 10, ModeIsolated) // no bybit
	if r.Ok {
		t.Fatal("replayed a leg with no verified risk schedule")
	}
}

func TestNoPriceInWindowIsAnError(t *testing.T) {
	s := series([2]float64{100, 100})
	sub := subject(1)
	sub.OpenedAt = base.Add(48 * time.Hour) // far outside the data
	sub.ClosedAt = base.Add(49 * time.Hour)

	r := Replay(sub, s, schedules("binance", "bybit"), 10, ModeIsolated)
	if r.Ok {
		t.Fatal("replayed a position with no price history")
	}
}

// --- aggregation ---------------------------------------------------------------

func TestSummariseCountsBothOutcomes(t *testing.T) {
	quiet := series([2]float64{99, 101})
	crash := series([2]float64{88, 100})

	rs := []Result{
		Replay(subject(1), quiet, schedules("binance", "bybit"), 10, ModeIsolated),
		Replay(subject(1), crash, schedules("binance", "bybit"), 10, ModeIsolated),
	}
	sum := Summarise(rs, 10, ModeIsolated)

	if sum.Total != 2 || sum.Survived != 1 || sum.Liquidated != 1 {
		t.Fatalf("total %d, survived %d, liquidated %d; want 2/1/1",
			sum.Total, sum.Survived, sum.Liquidated)
	}
	if sum.NetUSD >= 0 {
		t.Fatalf("net $%+.2f across one win and one liquidation", sum.NetUSD)
	}
}
