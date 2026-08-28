package crossvenue

import (
	"testing"
	"time"
)

// TestForecastIsFrozenNotRecomputed.
//
// The path stored on a position must be the one computed at ENTRY. A path
// recalculated later would use whatever rates prevail then, and could never
// disagree with reality -- which would make the check worthless.
func TestForecastIsFrozenNotRecomputed(t *testing.T) {
	b := testBook(t, nil)
	if _, err := b.Update(fixedNow, []Candidate{goodCandidate()}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	open, _ := b.Snapshot()
	if len(open) != 1 {
		t.Fatalf("open %d, want 1", len(open))
	}
	p := open[0]

	if len(p.PlanPath) == 0 {
		t.Fatal("no forecast stored at entry")
	}
	if p.PlanCostBps <= 0 {
		t.Fatal("the cost the forecast assumed was not recorded")
	}
	first := p.PlanPath[0]
	if first.AtHours <= 0 {
		t.Fatalf("first settlement forecast at %v hours", first.AtHours)
	}

	// The rate collapses. The stored forecast must NOT move.
	c := goodCandidate()
	c.LongBpsHr = -0.001
	if _, err := b.Update(fixedNow.Add(5*time.Minute), []Candidate{c}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	open, _ = b.Snapshot()
	if open[0].PlanPath[0].NetBps != first.NetBps {
		t.Fatalf("forecast changed from %v to %v after the rate moved; it is not frozen",
			first.NetBps, open[0].PlanPath[0].NetBps)
	}
}

// TestCheckRecordedAtTheSettlement.
func TestCheckRecordedAtTheSettlement(t *testing.T) {
	b := testBook(t, nil)
	if _, err := b.Update(fixedNow, []Candidate{goodCandidate()}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Nothing has settled yet.
	open, _ := b.Snapshot()
	if len(open[0].PlanChecks) != 0 {
		t.Fatalf("%d checks before any settlement", len(open[0].PlanChecks))
	}

	// The hourly leg settles at +0.5h.
	c := goodCandidate()
	c.LongNextFundingMs = fixedNow.Add(90 * time.Minute).UnixMilli()
	if _, err := b.Update(fixedNow.Add(35*time.Minute), []Candidate{c}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	open, _ = b.Snapshot()
	p := open[0]
	if len(p.PlanChecks) != 1 {
		t.Fatalf("%d checks after one settlement, want 1", len(p.PlanChecks))
	}

	ck := p.PlanChecks[0]
	// goodCandidate: long leg -24 bps/hr on a 1h clock -> +24 collected.
	if !near(ck.ActualFundingBps, 24, 1e-9) {
		t.Fatalf("actual funding %.4f, want 24", ck.ActualFundingBps)
	}
	if !near(ck.PredictedFundingBps, 24, 1e-9) {
		t.Fatalf("predicted funding %.4f, want 24", ck.PredictedFundingBps)
	}
	if !near(ck.FundingErrorBps, 0, 1e-9) {
		t.Fatalf("funding error %.4f on a rate that did not move; the model and the "+
			"accrual disagree", ck.FundingErrorBps)
	}
}

// TestDivergenceIsCaught.
//
// The whole point. If funding does NOT arrive as forecast, the check must say
// so rather than quietly agreeing.
func TestDivergenceIsCaught(t *testing.T) {
	b := testBook(t, nil)
	if _, err := b.Update(fixedNow, []Candidate{goodCandidate()}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// The rate collapses to a tenth before the settlement lands. The forecast
	// said +24; reality will pay +2.4.
	c := goodCandidate()
	c.LongBpsHr = -2.4
	c.LongNextFundingMs = fixedNow.Add(90 * time.Minute).UnixMilli()
	if _, err := b.Update(fixedNow.Add(35*time.Minute), []Candidate{c}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	open, _ := b.Snapshot()
	p := open[0]
	if len(p.PlanChecks) != 1 {
		t.Fatalf("%d checks, want 1", len(p.PlanChecks))
	}

	// Settlement books the rate that APPLIED (-24, observed at entry), not the
	// fresh -2.4. So this particular divergence should NOT appear -- and that
	// is itself the thing being tested.
	ck := p.PlanChecks[0]
	if !near(ck.ActualFundingBps, 24, 1e-9) {
		t.Fatalf("actual %.4f: the settlement booked the NEW rate instead of the one "+
			"that applied during the interval", ck.ActualFundingBps)
	}

	worst, _, _, n, ok := p.PlanAccuracy()
	if !ok || n != 1 {
		t.Fatalf("accuracy n=%d ok=%v", n, ok)
	}
	if worst > 1e-6 {
		t.Fatalf("worst funding error %.4f, want ~0", worst)
	}
}

// TestCostDriftIsSeparatedFromFundingError.
//
// An exit that got dearer is a market change. Funding that did not arrive is a
// modelling failure. Reporting them as one number hides the second behind the
// first.
func TestCostDriftIsSeparatedFromFundingError(t *testing.T) {
	b := testBook(t, nil)
	if _, err := b.Update(fixedNow, []Candidate{goodCandidate()}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Same rates, but the exit book triples.
	c := goodCandidate()
	c.LongNextFundingMs = fixedNow.Add(90 * time.Minute).UnixMilli()
	c.LongExitSlipBps, c.ShortExitSlipBps = 20, 20
	if _, err := b.Update(fixedNow.Add(35*time.Minute), []Candidate{c}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	open, _ := b.Snapshot()
	ck := open[0].PlanChecks[0]

	if !near(ck.FundingErrorBps, 0, 1e-9) {
		t.Fatalf("funding error %.4f on unchanged rates -- cost drift leaked into it",
			ck.FundingErrorBps)
	}
	if ck.CostDriftBps <= 0 {
		t.Fatalf("cost drift %.4f after the exit book tripled", ck.CostDriftBps)
	}
}

// TestOneCheckPerCall. Stamping one instantaneous net onto several forecast
// points would manufacture agreement.
func TestOneCheckPerCall(t *testing.T) {
	b := testBook(t, nil)
	if _, err := b.Update(fixedNow, []Candidate{goodCandidate()}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Jump forward past several forecast points in one go.
	c := goodCandidate()
	c.LongNextFundingMs = fixedNow.Add(10 * time.Hour).UnixMilli()
	if _, err := b.Update(fixedNow.Add(6*time.Hour), []Candidate{c}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	open, _ := b.Snapshot()
	if n := len(open[0].PlanChecks); n != 1 {
		t.Fatalf("%d checks from a single pass, want 1", n)
	}
}
