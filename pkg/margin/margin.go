// Package margin computes how close a leveraged position is to liquidation.
//
// WHY THIS IS SEPARATE FROM pkg/risk
//
// pkg/risk classifies a position's severity for a human reading a dashboard.
// This package answers one arithmetic question: at price P, with leverage L,
// is this leg dead? It has no opinions and no severity levels.
//
// THREE THINGS THAT MUST NOT BE GUESSED
//
//  1. THE MAINTENANCE MARGIN RATE. It is set per SYMBOL and per NOTIONAL TIER
//     by each venue, and it is the number that decides when you die. A guessed
//     MMR produces a confident liquidation price that is wrong, which is worse
//     than no answer. Schedules carry Verified and a source; an unverified one
//     REFUSES rather than defaults.
//
//  2. WHICH PRICE. Venues liquidate on the MARK price, not the last trade.
//     Mark is index-anchored precisely so a thin-book wick cannot liquidate
//     anyone. Replaying against traded prices over-counts deaths; replaying
//     against a 5-minute sample of anything under-counts them.
//
//  3. WHETHER THE LEGS NET. On one venue under portfolio margin, a long spot
//     and a short perp offset and the account barely moves. Across two venues
//     they do not offset AT ALL -- each exchange sees a naked leg. Same two
//     positions, completely different survival. Portfolio() and Isolated()
//     exist so a caller must state which world it is in.
package margin

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Side is the direction of a leg.
type Side int

const (
	Long Side = iota
	Short
)

func (s Side) String() string {
	if s == Short {
		return "short"
	}
	return "long"
}

// Tier is one risk-limit bracket.
//
// Venues raise the maintenance margin rate as position size grows, so a
// $100,000 position on the same symbol is liquidated earlier than a $400 one.
// Taking the smallest tier's rate for every size is a quiet way to understate
// risk at exactly the size where it matters.
type Tier struct {
	// MaxNotionalUSD is the top of this bracket.
	MaxNotionalUSD float64

	// MaintenanceMarginRate is the fraction of notional that must remain as
	// equity. Below it, the venue closes the position.
	MaintenanceMarginRate float64

	// MaxLeverage is the highest leverage this bracket permits.
	MaxLeverage float64
}

// Schedule is one symbol's risk-limit table on one venue.
type Schedule struct {
	Venue  string
	Symbol string
	Tiers  []Tier

	// Source and VerifiedAt record WHERE this came from and WHEN.
	//
	// Same discipline as the fee schedule. A maintenance margin rate nobody
	// has checked produces a liquidation price nobody should act on.
	Source     string
	VerifiedAt time.Time
	Verified   bool
}

// TierFor returns the bracket governing a given notional.
//
// ok is false when the schedule is unverified, empty, or the notional exceeds
// every bracket -- a position larger than the venue's top tier is not
// permitted at all, which is a refusal, not a clamp.
func (s Schedule) TierFor(notionalUSD float64) (Tier, bool) {
	if !s.Verified || len(s.Tiers) == 0 || notionalUSD <= 0 {
		return Tier{}, false
	}
	tiers := make([]Tier, len(s.Tiers))
	copy(tiers, s.Tiers)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].MaxNotionalUSD < tiers[j].MaxNotionalUSD })

	for _, t := range tiers {
		if notionalUSD <= t.MaxNotionalUSD {
			if t.MaintenanceMarginRate <= 0 {
				return Tier{}, false
			}
			return t, true
		}
	}
	return Tier{}, false
}

// Leg is one position on one venue.
type Leg struct {
	Venue  string
	Symbol string
	Side   Side

	// EntryPrice and QtyBase define the position. Notional is derived, so it
	// moves with price exactly as the venue's does.
	EntryPrice float64
	QtyBase    float64

	// Leverage determines the margin posted: notional at entry / Leverage.
	Leverage float64

	// CollateralUSD overrides the derived margin when set, for cases where
	// extra was posted as a buffer. Zero means "derive it from Leverage".
	CollateralUSD float64
}

// NotionalAtEntry is the position's size when opened.
func (l Leg) NotionalAtEntry() float64 { return l.EntryPrice * l.QtyBase }

// Posted is the margin backing this leg.
func (l Leg) Posted() float64 {
	if l.CollateralUSD > 0 {
		return l.CollateralUSD
	}
	if l.Leverage <= 0 {
		return 0
	}
	return l.NotionalAtEntry() / l.Leverage
}

// UnrealizedAt is the P&L in USD if the mark is at price.
func (l Leg) UnrealizedAt(price float64) float64 {
	if l.Side == Short {
		return (l.EntryPrice - price) * l.QtyBase
	}
	return (price - l.EntryPrice) * l.QtyBase
}

// State is one leg's health at a given mark price.
type State struct {
	Price float64

	NotionalUSD    float64
	CollateralUSD  float64
	UnrealizedUSD  float64
	EquityUSD      float64
	MaintenanceUSD float64

	// MarginRatio is maintenance / equity. At or above 1.0 the venue
	// liquidates. Venues usually warn far earlier.
	MarginRatio float64

	Liquidated bool

	// MMR is the rate actually applied, from the tier the CURRENT notional
	// falls into.
	MMR float64

	Ok bool
}

// Isolated computes a leg's health with NO offsetting position.
//
// This is the cross-venue world: the exchange sees one naked leg and does not
// know the hedge exists. Measured 2026-08-12, a KAITO position held long on
// one venue and short on another was fully hedged in P&L and completely
// unhedged in MARGIN.
func (l Leg) Isolated(price float64, s Schedule) (State, error) {
	st := State{Price: price}

	if price <= 0 || l.QtyBase <= 0 || l.EntryPrice <= 0 {
		return st, fmt.Errorf("margin: leg has non-positive price or size")
	}
	posted := l.Posted()
	if posted <= 0 {
		return st, fmt.Errorf("margin: no collateral -- set Leverage or CollateralUSD")
	}

	st.NotionalUSD = price * l.QtyBase
	st.CollateralUSD = posted
	st.UnrealizedUSD = l.UnrealizedAt(price)
	st.EquityUSD = posted + st.UnrealizedUSD

	// The tier is chosen on CURRENT notional, not entry notional. A position
	// that grew into a higher bracket is liquidated on the higher bracket's
	// rate, and pricing it on the entry bracket understates the risk exactly
	// when the position is largest.
	tier, ok := s.TierFor(st.NotionalUSD)
	if !ok {
		return st, fmt.Errorf(
			"margin: no verified risk-limit tier for %s on %s at $%.2f notional; "+
				"a guessed maintenance rate gives a confident wrong liquidation price",
			l.Symbol, s.Venue, st.NotionalUSD)
	}
	st.MMR = tier.MaintenanceMarginRate
	st.MaintenanceUSD = st.NotionalUSD * st.MMR

	if st.EquityUSD <= 0 {
		st.MarginRatio = math.Inf(1)
		st.Liquidated = true
		st.Ok = true
		return st, nil
	}
	st.MarginRatio = st.MaintenanceUSD / st.EquityUSD
	st.Liquidated = st.MarginRatio >= 1
	st.Ok = true
	return st, nil
}

// LiquidationPrice is the mark at which this leg is closed by the venue.
//
// Derived rather than searched:
//
//	long:   P = E · (1 − 1/L) / (1 − mmr)
//	short:  P = E · (1 + 1/L) / (1 + mmr)
//
// At 10x with a 1% maintenance rate, a long entered at 100 dies at 90.91 --
// a 9.1% move. The rough form (1/L − mmr) gives 9%; this is the exact figure
// and it is the one the venue uses.
func (l Leg) LiquidationPrice(s Schedule) (float64, error) {
	if l.EntryPrice <= 0 || l.QtyBase <= 0 {
		return 0, fmt.Errorf("margin: leg is not fully specified")
	}
	posted := l.Posted()
	if posted <= 0 {
		return 0, fmt.Errorf("margin: no collateral")
	}

	tier, ok := s.TierFor(l.NotionalAtEntry())
	if !ok {
		return 0, fmt.Errorf("margin: no verified tier for %s on %s", l.Symbol, s.Venue)
	}
	mmr := tier.MaintenanceMarginRate

	// Effective leverage from the collateral actually posted, so a buffer is
	// honoured rather than ignored.
	effL := l.NotionalAtEntry() / posted

	if l.Side == Short {
		den := 1 + mmr
		return l.EntryPrice * (1 + 1/effL) / den, nil
	}
	den := 1 - mmr
	if den <= 0 {
		return 0, fmt.Errorf("margin: maintenance rate %.4f is nonsensical", mmr)
	}
	return l.EntryPrice * (1 - 1/effL) / den, nil
}

// DistanceToLiquidationPct is how far the mark must move, as a percentage of
// the current price, before this leg dies. Always reported positive.
func (l Leg) DistanceToLiquidationPct(price float64, s Schedule) (float64, error) {
	liq, err := l.LiquidationPrice(s)
	if err != nil {
		return 0, err
	}
	if price <= 0 {
		return 0, fmt.Errorf("margin: non-positive price")
	}
	return math.Abs(price-liq) / price * 100, nil
}

// --- portfolio (single venue) -------------------------------------------------

// PortfolioState is the health of several legs sharing ONE margin account.
//
// This is the single-venue world. Long spot and short perp on the same
// exchange offset inside one account, so a price move that would liquidate
// either leg alone barely moves the account's equity. It is the whole reason a
// unified account is safer than two venues at the same leverage.
type PortfolioState struct {
	EquityUSD      float64
	MaintenanceUSD float64
	MarginRatio    float64
	Liquidated     bool

	NetDeltaUSD float64
	Legs        []State
	Ok          bool
}

// Portfolio computes account-level health for legs in one margin account.
//
// prices is keyed by "venue|symbol". A missing price is an error and not a
// zero: a leg priced at zero would look like a total loss and trigger a
// liquidation that never happened.
func Portfolio(legs []Leg, prices map[string]float64, schedules map[string]Schedule,
	extraCollateralUSD float64) (PortfolioState, error) {

	var ps PortfolioState
	ps.EquityUSD = extraCollateralUSD

	for _, l := range legs {
		key := l.Venue + "|" + l.Symbol
		px, ok := prices[key]
		if !ok || px <= 0 {
			return ps, fmt.Errorf("margin: no price for %s", key)
		}
		sch, ok := schedules[key]
		if !ok {
			return ps, fmt.Errorf("margin: no risk schedule for %s", key)
		}

		st, err := l.Isolated(px, sch)
		if err != nil {
			return ps, err
		}
		ps.Legs = append(ps.Legs, st)

		ps.EquityUSD += st.CollateralUSD + st.UnrealizedUSD
		ps.MaintenanceUSD += st.MaintenanceUSD

		// Net delta: what the account is still exposed to after offsetting.
		// Near zero is the point of the hedge.
		if l.Side == Short {
			ps.NetDeltaUSD -= st.NotionalUSD
		} else {
			ps.NetDeltaUSD += st.NotionalUSD
		}
	}

	if len(ps.Legs) == 0 {
		return ps, fmt.Errorf("margin: no legs")
	}
	if ps.EquityUSD <= 0 {
		ps.MarginRatio = math.Inf(1)
		ps.Liquidated = true
		ps.Ok = true
		return ps, nil
	}
	ps.MarginRatio = ps.MaintenanceUSD / ps.EquityUSD
	ps.Liquidated = ps.MarginRatio >= 1
	ps.Ok = true
	return ps, nil
}

// Describe renders a leg's state for a log line.
func (st State) Describe() string {
	if !st.Ok {
		return "margin UNKNOWN"
	}
	s := fmt.Sprintf("equity $%.2f, maintenance $%.2f, ratio %.1f%%",
		st.EquityUSD, st.MaintenanceUSD, st.MarginRatio*100)
	if st.Liquidated {
		s += "  LIQUIDATED"
	}
	return s
}
