package exchange

import "fmt"

// ADAPTIVE SIZING
//
// Until now VEGA sized every position at a fixed notional and then asked
// whether the book could hold it. That is backwards. The book is a fact; the
// notional is a preference. So this inverts it: read the book, then take what
// it can actually give, up to the target.
//
// THE BINDING CONSTRAINT IS THE SMALLER LEG. A cash-and-carry needs both a
// spot fill and a perp fill of the same size, so the position is limited by
// whichever book is thinner. Measured 2026-08-11, B3USDT showed $20.30 resting
// on spot against $1.65 on the perp -- a leg that looks tradeable next to one
// that is not. Sizing against the spot book alone would have produced a
// position that could not be hedged.
//
// WHY THIS IS AN IMPROVEMENT EVEN ON BTC
//
// On a deep symbol nothing changes: the touch holds far more than the target,
// so the target wins. What changes is that the measured half-spread stops
// being a polite fiction on everything else. Today the scanner prints a
// CAUTION when the order exceeds the touch; with adaptive sizing that
// condition cannot arise, because the order is never larger than the touch.
//
// WHAT IT DOES NOT DO
//
// It does not make thin symbols profitable. A $10 position on a symbol paying
// 0.1% per interval earns fractions of a cent, and its exit is still gated by
// the same $10 book. Sizing to depth makes the entry honest; it does not
// create liquidity that is not there.

// SizingPolicy decides how large a position may be.
type SizingPolicy struct {
	// TargetNotionalUSD is the size you want per leg, if the book allows.
	TargetNotionalUSD float64

	// Adaptive turns depth-based sizing on. False reproduces the old
	// behaviour exactly -- always the target -- so this can be switched off
	// to compare.
	Adaptive bool

	// DepthFraction is how much of the resting touch size to consume.
	//
	// 1.0 means take the entire displayed quantity at the best price. That is
	// the theoretical maximum for filling without walking the book, and it is
	// too optimistic in practice: the book moves between the moment it is
	// observed and the moment an order lands, and resting size is frequently
	// cancelled. 0.5 leaves headroom for both.
	DepthFraction float64

	// MinNotionalUSD is the floor below which a position is not worth
	// opening. This is an EXCHANGE constraint, not a preference: Binance
	// USDT-M futures rejects orders under roughly $5 notional (some symbols
	// $20 or $100) and spot has its own minNotional filter. A size below the
	// floor is not a small position, it is a rejected order -- and a rejected
	// second leg is a naked first leg.
	MinNotionalUSD float64
}

// DefaultSizingPolicy sizes to the book with a $10,000 ceiling.
func DefaultSizingPolicy(target float64) SizingPolicy {
	if target <= 0 {
		target = 10000
	}
	return SizingPolicy{
		TargetNotionalUSD: target,
		Adaptive:          true,
		DepthFraction:     0.5,
		MinNotionalUSD:    200,
	}
}

// Sizing is the decision for one symbol.
type Sizing struct {
	// NotionalUSD is the size to use per leg. Zero when Ok is false.
	NotionalUSD float64 `json:"notional_usd"`

	// TargetUSD is what was asked for, kept so the gap is visible.
	TargetUSD float64 `json:"target_usd"`

	// LimitedBy names what set the size: "target", "spot book", "perp book",
	// or "" when the position was refused.
	LimitedBy string `json:"limited_by"`

	// Reduced is true when the book, not the target, decided the size.
	Reduced bool `json:"reduced"`

	// Ok is false when no size is placeable at all.
	Ok bool `json:"ok"`

	// Reason explains a refusal, or describes the reduction.
	Reason string `json:"reason"`

	SpotTopUSD float64 `json:"spot_top_usd"`
	PerpTopUSD float64 `json:"perp_top_usd"`
}

// String renders a sizing decision for a log line.
func (s Sizing) String() string {
	if !s.Ok {
		return fmt.Sprintf("UNSIZEABLE: %s", s.Reason)
	}
	if !s.Reduced {
		return fmt.Sprintf("$%.2f (full target)", s.NotionalUSD)
	}
	return fmt.Sprintf("$%.2f, reduced from $%.2f by the %s (spot $%.2f, perp $%.2f at the touch)",
		s.NotionalUSD, s.TargetUSD, s.LimitedBy, s.SpotTopUSD, s.PerpTopUSD)
}

// Size decides how much of an observation's book to take.
func (p SizingPolicy) Size(o Observation) Sizing {
	target := p.TargetNotionalUSD
	if target <= 0 {
		target = 10000
	}

	s := Sizing{
		TargetUSD:  target,
		SpotTopUSD: o.SpotTopOfBookUSD,
		PerpTopUSD: o.PerpTopOfBookUSD,
	}

	// Non-adaptive reproduces the old behaviour byte for byte, so the two can
	// be compared on the same data.
	if !p.Adaptive {
		s.NotionalUSD = target
		s.LimitedBy = "target"
		s.Ok = true
		s.Reason = "adaptive sizing disabled"
		return s
	}

	// A book that could not be read cannot be sized against. Falling back to
	// the target here would be sizing against an assumption, which is the one
	// thing this whole package refuses to do.
	if !o.LiquidityMeasured {
		s.Reason = "book could not be read; size is unknowable, not assumable"
		return s
	}

	frac := p.DepthFraction
	if frac <= 0 {
		frac = 0.5
	}
	if frac > 1 {
		// Above 1.0 the order walks past the touch, and the measured
		// half-spread stops describing the fill. Clamped rather than obeyed.
		frac = 1
	}

	if o.SpotTopOfBookUSD <= 0 || o.PerpTopOfBookUSD <= 0 {
		s.Reason = fmt.Sprintf("one leg has no resting size (spot $%.2f, perp $%.2f); the hedge cannot be built",
			o.SpotTopOfBookUSD, o.PerpTopOfBookUSD)
		return s
	}

	spotCap := o.SpotTopOfBookUSD * frac
	perpCap := o.PerpTopOfBookUSD * frac

	// The smaller leg binds. Both fills must be the same size or the position
	// is not hedged.
	bookCap := spotCap
	limitedBy := "spot book"
	if perpCap < spotCap {
		bookCap = perpCap
		limitedBy = "perp book"
	}

	if bookCap >= target {
		s.NotionalUSD = target
		s.LimitedBy = "target"
		s.Ok = true
		s.Reason = "book holds the full target"
		return s
	}

	// The book is the constraint.
	if bookCap < p.MinNotionalUSD {
		s.Reason = fmt.Sprintf(
			"%s allows only $%.2f at %.0f%% of the touch, below the $%.2f exchange minimum; "+
				"an order this small would be rejected, and a rejected second leg is a naked first leg",
			limitedBy, bookCap, frac*100, p.MinNotionalUSD)
		return s
	}

	s.NotionalUSD = bookCap
	s.LimitedBy = limitedBy
	s.Reduced = true
	s.Ok = true
	s.Reason = fmt.Sprintf("reduced to %.0f%% of the %s", frac*100, limitedBy)
	return s
}

// ForBook returns a copy of these constraints resized to what the book can
// actually hold, plus the sizing decision itself.
//
// Deliberately returns a COPY rather than mutating. Constraints is passed by
// value throughout and a caller that reused a mutated set across symbols would
// silently carry one symbol's book depth into the next symbol's assessment.
//
// Callers use it as:
//
//	sized, s := cons.ForBook(o)
//	if !s.Ok { /* refuse, with s.Reason */ }
//	a, err := sized.Assess(o, venue)
func (c Constraints) ForBook(o Observation) (Constraints, Sizing) {
	pol := c.Sizing
	if pol.TargetNotionalUSD <= 0 {
		// Fall back to the fixed notional already configured, so a Constraints
		// built the old way keeps working unchanged.
		pol.TargetNotionalUSD = c.NotionalUSD
	}
	if pol.MinNotionalUSD <= 0 {
		pol.MinNotionalUSD = 10
	}
	if pol.DepthFraction <= 0 {
		pol.DepthFraction = 0.5
	}

	s := pol.Size(o)
	if !s.Ok {
		return c, s
	}
	c.NotionalUSD = s.NotionalUSD
	return c, s
}
