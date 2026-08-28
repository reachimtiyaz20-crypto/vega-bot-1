package funding

import "testing"

// TestConfigReachesTheEntryGate.
//
// Until 2026-08-11 Book.Update built its constraints from hardcoded literals,
// so -notional, -min-vol, -min-depth and -max-slip changed the scanner's
// output and nothing else. The book kept entering on $10,000 against a $50M
// floor while the scanner reported sixteen candidates at $400.
//
// A knob that does not reach the code it names produces confident wrong
// beliefs, which is the failure this whole project exists to prevent.
func TestConfigReachesTheEntryGate(t *testing.T) {
	cfg := DefaultPaperConfig()
	cfg.NotionalUSD = 400
	cfg.MinQuoteVolume24hUSD = 10_000_000
	cfg.MinTopOfBookFraction = 0.5
	cfg.MaxRoundTripSlippageBps = 6

	c := cfg.constraints()

	if c.NotionalUSD != 400 {
		t.Fatalf("notional %v did not reach the gate", c.NotionalUSD)
	}
	if c.MinQuoteVolume24hUSD != 10_000_000 {
		t.Fatalf("volume floor %v did not reach the gate", c.MinQuoteVolume24hUSD)
	}
	if c.MinTopOfBookFraction != 0.5 {
		t.Fatalf("depth floor %v did not reach the gate", c.MinTopOfBookFraction)
	}
	if c.MaxRoundTripSlippageBps != 6 {
		t.Fatalf("slippage ceiling %v did not reach the gate", c.MaxRoundTripSlippageBps)
	}
	if !c.RequireMeasuredLiquidity {
		t.Fatal("measured-liquidity requirement was dropped")
	}
}

// TestZeroConfigDoesNotDisableFilters.
//
// A positions.json written before these fields existed loads with zeros. A
// zero volume floor would mean NO volume floor, which is the opposite of the
// conservative reading and would let the book enter anything.
func TestZeroConfigDoesNotDisableFilters(t *testing.T) {
	c := PaperConfig{}.constraints()

	if c.NotionalUSD <= 0 {
		t.Fatal("zero config produced a zero notional")
	}
	if c.MinQuoteVolume24hUSD <= 0 {
		t.Fatal("zero config disabled the volume floor")
	}
	if c.MinTopOfBookFraction <= 0 {
		t.Fatal("zero config disabled the depth floor")
	}
	if c.MaxRoundTripSlippageBps <= 0 {
		t.Fatal("zero config disabled the slippage ceiling")
	}
	if !c.RequireMeasuredLiquidity {
		t.Fatal("zero config allowed entry against an unmeasured book")
	}
}

// TestDefaultsAreNonZero, so nothing downstream has to guess.
func TestDefaultsAreNonZero(t *testing.T) {
	d := DefaultPaperConfig()
	if d.NotionalUSD <= 0 || d.MinQuoteVolume24hUSD <= 0 ||
		d.MinTopOfBookFraction <= 0 || d.MaxRoundTripSlippageBps <= 0 {
		t.Fatalf("DefaultPaperConfig has a zero filter: %+v", d)
	}
}
