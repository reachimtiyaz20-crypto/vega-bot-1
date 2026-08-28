// Package orderbook is one order book and one way of costing it.
//
// # WHY THIS IS SHARED RATHER THAN PER-VENUE
//
// A cross-venue position is long a perp on one venue and short a perp on
// another. Its cost is the sum of what each leg pays to cross its own book. If
// those two numbers come from two different implementations -- one walking
// levels, one trusting a venue-supplied "impact price", one charging both sides
// and one charging half -- then the total is not a cost, it is two
// incomparable numbers added together.
//
// So there is exactly one Sweep here, and every venue reader in this project
// produces a Book that it consumes.
//
// # THE RULE THIS PACKAGE ENFORCES
//
// An unread book is a REFUSAL, never a default. Measured is false until real
// levels have been parsed, and every method returns ok=false while it is false.
// "I could not read the book" and "the book is deep" must never produce the
// same answer, because only one of them is safe to trade on.
package orderbook

import (
	"sort"
	"strconv"
	"time"
)

// Level is one price level of resting size.
type Level struct {
	Px float64
	Sz float64 // base units
	N  int     // number of orders, where the venue reports it
}

// USD is the notional resting at this level.
func (l Level) USD() float64 { return l.Px * l.Sz }

// Book is both sides of one market at a moment.
type Book struct {
	Venue  string
	Symbol string
	At     time.Time

	// Kind is "spot", "linear", "swap" or empty. Recorded so a spot book
	// cannot be silently used where a perp book was meant -- they differ by
	// the basis, which is a quantity this project measures deliberately.
	Kind string

	Bids []Level // descending price
	Asks []Level // ascending price

	// Measured is false if the venue returned an empty or unparseable book.
	// Nothing downstream may treat an unmeasured book as a deep one.
	Measured bool
}

// ParseNum parses a venue's string-encoded number.
//
// ok=false rather than 0, because zero is a legitimate funding rate and a
// catastrophic price. A silently-zeroed price sizes a position against nothing.
func ParseNum(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Finalise sorts both sides and sets Measured.
//
// Venues generally return sorted books. Sorting anyway is cheap, and a book
// that arrived out of order would make every sweep silently optimistic --
// the sweep would fill at the best prices it happened to see first.
func (b *Book) Finalise() {
	sort.Slice(b.Bids, func(i, j int) bool { return b.Bids[i].Px > b.Bids[j].Px })
	sort.Slice(b.Asks, func(i, j int) bool { return b.Asks[i].Px < b.Asks[j].Px })
	b.Measured = len(b.Bids) > 0 && len(b.Asks) > 0
	if b.At.IsZero() {
		b.At = time.Now().UTC()
	}
}

// Mid is the midpoint. ok is false on an unmeasured book.
func (b Book) Mid() (mid float64, ok bool) {
	if !b.Measured {
		return 0, false
	}
	return (b.Bids[0].Px + b.Asks[0].Px) / 2, true
}

// SpreadBps is touch to touch. Half of it is the minimum cost of crossing,
// before any size is considered.
func (b Book) SpreadBps() (float64, bool) {
	mid, ok := b.Mid()
	if !ok || mid <= 0 {
		return 0, false
	}
	return (b.Asks[0].Px - b.Bids[0].Px) / mid * 10000, true
}

// TopOfBookUSD is the notional at the best bid and best ask.
//
// The SMALLER of the two bounds the round trip: a position must be both opened
// and closed, so the thinner side is the constraint.
func (b Book) TopOfBookUSD() (bid, ask, min float64, ok bool) {
	if !b.Measured {
		return 0, 0, 0, false
	}
	bid = b.Bids[0].USD()
	ask = b.Asks[0].USD()
	min = bid
	if ask < min {
		min = ask
	}
	return bid, ask, min, true
}

// Sweep is what a market order of a given size would actually pay.
type Sweep struct {
	// Filled is the USD the book could absorb. Less than requested when the
	// book runs out.
	Filled float64

	// VWAP is the size-weighted average fill price.
	VWAP float64

	// SlippageBps is VWAP against the mid, ALWAYS POSITIVE: the cost of
	// crossing at this size. This replaces every venue-supplied depth proxy.
	SlippageBps float64

	// Exhausted means the book could not fill the whole request. Callers must
	// treat this as a refusal, not a partial fill -- a second leg that only
	// half-fills leaves the first leg naked.
	Exhausted bool

	LevelsUsed int
	Ok         bool
}

// SweepCost walks the resting orders and computes what notionalUSD would pay.
//
// buy=true crosses the asks, buy=false crosses the bids.
func (b Book) SweepCost(notionalUSD float64, buy bool) Sweep {
	mid, ok := b.Mid()
	if !ok || mid <= 0 || notionalUSD <= 0 {
		return Sweep{}
	}

	levels := b.Asks
	if !buy {
		levels = b.Bids
	}

	remaining := notionalUSD
	var baseFilled, quoteSpent float64
	used := 0

	for _, lv := range levels {
		avail := lv.USD()
		if avail <= 0 {
			continue
		}
		used++

		take := avail
		if take > remaining {
			take = remaining
		}
		baseFilled += take / lv.Px
		quoteSpent += take
		remaining -= take

		if remaining <= 1e-9 {
			break
		}
	}

	if baseFilled <= 0 {
		return Sweep{}
	}

	s := Sweep{
		Filled:     quoteSpent,
		VWAP:       quoteSpent / baseFilled,
		Exhausted:  remaining > 1e-9,
		LevelsUsed: used,
		Ok:         true,
	}

	// Buying above the mid and selling below it both COST.
	if buy {
		s.SlippageBps = (s.VWAP - mid) / mid * 10000
	} else {
		s.SlippageBps = (mid - s.VWAP) / mid * 10000
	}
	if s.SlippageBps < 0 {
		s.SlippageBps = 0
	}
	return s
}

// RoundTripSlippageBps is opening AND closing one leg, in slippage alone.
//
// A short perp sells to open and buys to close, so both sides are crossed.
// Charging one side is the error that reported CASHCAT at 20 bps when the true
// figure was 119.5.
//
// ok is false if either direction cannot fill: an unfillable exit is a
// refusal, not a cost.
func (b Book) RoundTripSlippageBps(notionalUSD float64) (bps float64, ok bool) {
	buy := b.SweepCost(notionalUSD, true)
	sell := b.SweepCost(notionalUSD, false)
	if !buy.Ok || !sell.Ok || buy.Exhausted || sell.Exhausted {
		return 0, false
	}
	return buy.SlippageBps + sell.SlippageBps, true
}

// DepthWithinBps is the USD resting within a given distance of the mid --
// how much could move without paying more than that. This is the question
// adaptive sizing actually asks.
func (b Book) DepthWithinBps(bps float64) (bidUSD, askUSD float64, ok bool) {
	mid, ok := b.Mid()
	if !ok || mid <= 0 || bps <= 0 {
		return 0, 0, false
	}
	floor := mid * (1 - bps/10000)
	ceil := mid * (1 + bps/10000)

	for _, lv := range b.Bids {
		if lv.Px < floor {
			break
		}
		bidUSD += lv.USD()
	}
	for _, lv := range b.Asks {
		if lv.Px > ceil {
			break
		}
		askUSD += lv.USD()
	}
	return bidUSD, askUSD, true
}
