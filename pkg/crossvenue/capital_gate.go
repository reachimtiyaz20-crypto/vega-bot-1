package crossvenue

// GateNoCapital refuses a candidate the book cannot fund.
//
// It is deliberately a refusal rather than a smaller position. Silently
// shrinking a trade to fit the remaining budget would change the thing being
// measured without saying so, and every cost figure this project holds is
// measured at a stated size.
const GateNoCapital = "NO_CAPITAL"

// leverage is the perp leverage per leg, defaulting to 1 (unlevered).
//
// At 1 this yields capital = 2 x notional for a cross-venue position, which is
// exactly what this book recorded before the ledger existed. The default is
// therefore behaviour-preserving, and any change to it is a deliberate act.
func (b *Book) leverage() float64 {
	if b.Leverage < 1 {
		return 1
	}
	return b.Leverage
}
