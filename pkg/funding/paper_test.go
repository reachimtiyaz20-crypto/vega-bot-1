package funding

import (
	"strings"
	"testing"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/exchange"
)

// The single most important property in this package: paper PnL must be able
// to go negative. The previous bot's totalFundingEarned used += only and
// showed a rising "earned" figure while the account balance fell.
func TestPaperPnLCanGoNegative(t *testing.T) {
	p := &Position{
		NotionalUSD:  10000,
		CapitalUSD:   20000,
		EntryCostBps: 19,
		ExitCostBps:  19,
		ClosedAt:     time.Now(),
	}

	// Opened and closed having collected nothing: pure cost.
	if got := p.NetBps(); got != -38 {
		t.Fatalf("net with no funding = %v, want -38", got)
	}
	if got := p.NetUSD(); got != -38 {
		t.Fatalf("net USD = %v, want -38", got)
	}

	// Negative funding must make it worse, not be floored at zero.
	p.FundingCollectedBps = -12
	if got := p.NetBps(); got != -50 {
		t.Fatalf("net after negative funding = %v, want -50", got)
	}
	if p.ReturnOnCapitalPct() >= 0 {
		t.Fatal("return on capital is not negative for a losing position")
	}
}

// An OPEN position must carry the exit cost as a liability. Charging only the
// entry would show a profit for a position that closes at a loss.
func TestOpenPositionCarriesExitCost(t *testing.T) {
	p := &Position{
		NotionalUSD:         10000,
		CapitalUSD:          20000,
		EntryCostBps:        19,
		FundingCollectedBps: 25,
	}
	if p.Closed() {
		t.Fatal("position reports closed with no ClosedAt")
	}
	// 25 collected, 19 in, 19 still to pay out = -13.
	if got := p.NetBps(); got != -13 {
		t.Fatalf("open net = %v, want -13 (exit cost must be carried)", got)
	}
}

// Return must be quoted against deployed capital, which is ~2x notional.
func TestReturnIsAgainstCapitalNotNotional(t *testing.T) {
	p := &Position{
		NotionalUSD:         10000,
		CapitalUSD:          20000,
		EntryCostBps:        15,
		ExitCostBps:         15,
		FundingCollectedBps: 130,
		ClosedAt:            time.Now(),
	}
	// net 100 bps of notional = $100 on $20,000 capital = 0.5%.
	if got := p.NetBps(); got != 100 {
		t.Fatalf("net = %v, want 100", got)
	}
	if got := p.ReturnOnCapitalPct(); got != 0.5 {
		t.Fatalf("return on capital = %v%%, want 0.5%% (half the notional figure)", got)
	}
}

// Signed accrual: a negative settlement reduces the accumulated total and
// increments the negative counter that drives the automated exit.
// TestAccrualCreditsTheRateObservedBeforeTheBoundary.
//
// This replaces TestAccrualIsSignedAndCountsNegatives, which asserted that the
// rate observed AT a boundary is credited to the interval that just ended.
// That is the inversion an external code review found on 2026-08-19: the
// venue's published rate is its prediction for the interval about to START.
// The old test locked the defect in place, so the defect survived a rewrite of
// the surrounding code.
func TestAccrualCreditsTheRateObservedBeforeTheBoundary(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	base := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	p := &Position{NextFundingTime: base.Add(8 * time.Hour)}

	// An observation BEFORE the boundary. Nothing settles yet, but this is the
	// rate that will apply when it does.
	b.accrue(p, obsAt(0.01, base.Add(8*time.Hour)), base.Add(7*time.Hour))
	if p.FundingCollectedBps != 0 {
		t.Fatalf("credited before any boundary passed: %v", p.FundingCollectedBps)
	}

	// The boundary passes. The venue now publishes -0.03 for the NEXT interval.
	// The one that just ended was funded at +0.01.
	b.accrue(p, obsAt(-0.03, base.Add(16*time.Hour)), base.Add(8*time.Hour))
	if p.FundingCollectedBps != 1 {
		t.Fatalf("booked the next interval's rate: %v, want 1", p.FundingCollectedBps)
	}
	if p.NegativeIntervals != 0 {
		t.Fatalf("counted a negative interval on a positive settlement: %d", p.NegativeIntervals)
	}

	// Next boundary: now the -0.03 applies, and it must REDUCE the total.
	b.accrue(p, obsAt(0.02, base.Add(24*time.Hour)), base.Add(16*time.Hour))
	if p.FundingCollectedBps != -2 {
		t.Fatalf("negative settlement did not reduce the total: %v, want -2", p.FundingCollectedBps)
	}
	if p.NegativeIntervals != 1 {
		t.Fatalf("negative intervals = %d, want 1", p.NegativeIntervals)
	}

	// A positive settlement resets the streak: one bad print is noise.
	b.accrue(p, obsAt(0.01, base.Add(32*time.Hour)), base.Add(24*time.Hour))
	if p.NegativeIntervals != 0 {
		t.Fatalf("negative streak not reset: %d", p.NegativeIntervals)
	}
	if p.IntervalsCollected != 3 {
		t.Fatalf("intervals = %d, want 3", p.IntervalsCollected)
	}
}

// TestMissedSettlementsAreCountedNotCollapsed.
//
// A restart, a venue outage or a rate-limit stall can step over several
// settlements between polls. Booking one is how 19 settlements got recorded
// where 23 had occurred, always on the paying leg.
func TestMissedSettlementsAreCountedNotCollapsed(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	base := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	p := &Position{NextFundingTime: base.Add(8 * time.Hour)}
	b.accrue(p, obsAt(0.01, base.Add(8*time.Hour)), base.Add(7*time.Hour))

	o := obsAt(-0.02, base.Add(32*time.Hour))
	o.IntervalHours = 8
	b.accrue(p, o, base.Add(30*time.Hour))

	if p.IntervalsCollected != 3 {
		t.Fatalf("intervals = %d, want 3", p.IntervalsCollected)
	}
	if p.MissedSettlements != 2 {
		t.Fatalf("missed = %d, want 2 -- the count is what says how much to distrust the total", p.MissedSettlements)
	}
	if p.FundingCollectedBps != 3 {
		t.Fatalf("collected = %v, want 3 (three intervals at +0.01%%)", p.FundingCollectedBps)
	}
}

// No settlement means no accrual. Polling every 5 minutes must not credit
// funding 96 times a day.
func TestNoAccrualWithoutSettlement(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	next := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	p := &Position{NextFundingTime: next}

	for i := 0; i < 20; i++ {
		b.accrue(p, obsAt(0.05, next), next.Add(-time.Hour))
	}
	if p.FundingCollectedBps != 0 {
		t.Fatalf("accrued %v bps without a settlement boundary", p.FundingCollectedBps)
	}
	if p.IntervalsCollected != 0 {
		t.Fatalf("counted %d intervals without a settlement", p.IntervalsCollected)
	}
}

// underwater builds a position that has paid its entry, earned nothing, and is
// therefore net negative but nowhere near the stop loss.
func underwater(now time.Time) *Position {
	return &Position{
		OpenedAt: now.Add(-72 * time.Hour), LastSeenAt: now,
		PlannedHoldDays: 30, NegativeIntervals: 6,
		EntryCostBps: 16, FundingCollectedBps: 0,
		NotionalUSD: 10000, CapitalUSD: 20000,
	}
}

// THE INVARIANT THIS WHOLE EXIT RULE EXISTS FOR.
//
// Six positions closed on 2026-08-06 after an average hold of 0.48 days, each
// paying a ~32 bps round trip to collect a fraction of a basis point. Total
// -186 bps. That is bug 3 of the previous bot -- rotation churn -- arriving
// through the exit rule rather than through rotation.
func TestDoesNotExitWhileUnderwater(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	now := time.Now().UTC()
	p := underwater(now)

	o := obsAt(-0.02, now.Add(8*time.Hour))
	o.SpotSymbolAvailable = true

	if p.NetBps() >= 0 {
		t.Fatalf("fixture is not underwater: %v bps", p.NetBps())
	}
	if reason := b.exitReason(p, o, true, now); reason != "" {
		t.Fatalf("closed an underwater position on negative funding, locking in the round trip: %s", reason)
	}
}

// Once the position has actually covered its costs, negative funding IS a
// reason to leave -- there is a profit to protect.
func TestExitsOnNegativeFundingOnceProfitable(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	now := time.Now().UTC()
	p := underwater(now)
	p.FundingCollectedBps = 40 // net = 40 - 16 - 16 = +8

	o := obsAt(-0.02, now.Add(8*time.Hour))
	o.SpotSymbolAvailable = true

	if p.NetBps() <= 0 {
		t.Fatalf("fixture is not profitable: %v bps", p.NetBps())
	}
	if reason := b.exitReason(p, o, true, now); reason == "" {
		t.Fatal("held a profitable position through sustained negative funding")
	}
}

// The stop loss is what makes holding an underwater position defensible. It
// must fire regardless of MinHoldDays or the underwater rule.
func TestStopLossOverridesTheHold(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	now := time.Now().UTC()
	p := underwater(now)
	p.OpenedAt = now.Add(-1 * time.Hour) // inside MinHoldDays
	p.FundingCollectedBps = -30          // net = -30 - 32 = -62, past the -60 limit

	o := obsAt(-0.02, now.Add(8*time.Hour))
	o.SpotSymbolAvailable = true

	if p.NetBps() > b.cfg.StopLossBps {
		t.Fatalf("fixture does not breach the stop: %v vs %v", p.NetBps(), b.cfg.StopLossBps)
	}
	reason := b.exitReason(p, o, true, now)
	if reason == "" {
		t.Fatal("stop loss did not fire; an underwater position could now run unbounded")
	}
	if !strings.Contains(reason, "stop loss") {
		t.Fatalf("exited for the wrong reason: %s", reason)
	}
}

// A single negative settlement is still noise and must not close anything.
func TestSingleNegativeSettlementDoesNotExit(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	now := time.Now().UTC()
	p := underwater(now)
	p.NegativeIntervals = 1

	o := obsAt(-0.02, now.Add(8*time.Hour))
	o.SpotSymbolAvailable = true

	if reason := b.exitReason(p, o, true, now); reason != "" {
		t.Fatalf("exited after a single negative settlement: %s", reason)
	}
}

// The planned hold is the hard cap. Nothing above may extend it.
func TestPlannedHoldAlwaysExits(t *testing.T) {
	b := &Book{cfg: DefaultPaperConfig()}
	now := time.Now().UTC()
	p := underwater(now)
	p.OpenedAt = now.Add(-31 * 24 * time.Hour)
	p.NegativeIntervals = 0

	o := obsAt(0.05, now.Add(8*time.Hour))
	o.SpotSymbolAvailable = true

	if reason := b.exitReason(p, o, true, now); reason == "" {
		t.Fatal("position ran past its 30-day planned hold")
	}
}

func obsAt(ratePct float64, next time.Time) exchange.Observation {
	return exchange.Observation{
		Venue:               "test",
		Symbol:              "TESTUSDT",
		FundingRatePct:      ratePct,
		IntervalHours:       8,
		NextFundingTime:     next,
		SpotSymbolAvailable: true,
		LiquidityMeasured:   true,
	}
}
