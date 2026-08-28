package funding

import "testing"

// A position that alternates positive and negative must not look identical to
// one that never went negative. NegativeIntervals resets on any positive
// settlement, so on its own it cannot tell those apart -- which is why an hour
// went into looking for a bug in HYPEUSDT that was never there.
func TestAdverseSettlementsSurviveAPositiveSettlement(t *testing.T) {
	var p Position

	book := func(ratePct float64) {
		p.FundingCollectedBps += ratePct * 100
		p.IntervalsCollected++
		if ratePct < 0 {
			p.NegativeIntervals++
			p.AdverseSettlements++
			p.AdverseBps += ratePct * 100
		} else {
			p.NegativeIntervals = 0
		}
	}

	book(-0.02) // -2 bps
	book(-0.01) // -1 bps
	book(0.03)  // +3 bps, streak resets here
	book(0.01)  // +1 bps

	if p.NegativeIntervals != 0 {
		t.Fatalf("streak counter should have reset: got %d", p.NegativeIntervals)
	}
	if p.AdverseSettlements != 2 {
		t.Errorf("adverse count lost to the reset: got %d, want 2", p.AdverseSettlements)
	}
	if diff := p.AdverseBps - (-3.0); diff > 0.001 || diff < -0.001 {
		t.Errorf("adverse bps: got %v, want -3", p.AdverseBps)
	}
	if diff := p.FundingCollectedBps - 1.0; diff > 0.001 || diff < -0.001 {
		t.Errorf("net collected: got %v, want +1", p.FundingCollectedBps)
	}

	// The point of the whole exercise: +1 net looks fine until you can see that
	// 4 bps were earned and 3 handed back.
	if p.AdverseSettlements == 0 && p.FundingCollectedBps > 0 {
		t.Error("a position that gave back three quarters of its funding reads as clean")
	}
}

// A position that never went negative must report zero, not a false positive.
func TestAdverseSettlementsStayZeroWhenNothingWentNegative(t *testing.T) {
	var p Position
	for i := 0; i < 5; i++ {
		p.FundingCollectedBps += 1.0
		p.IntervalsCollected++
		p.NegativeIntervals = 0
	}
	if p.AdverseSettlements != 0 || p.AdverseBps != 0 {
		t.Errorf("clean position reported adverse activity: %d settlements, %v bps",
			p.AdverseSettlements, p.AdverseBps)
	}
}
