package replay

import (
	"fmt"
	"math"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/margin"
)

// THE REPLAY ENGINE
//
// Walk a recorded position through real one-minute mark-price history and ask,
// at every minute: was it dead yet?
//
// FOUR DECISIONS THAT DETERMINE WHETHER THE ANSWER IS HONEST
//
//  1. EACH LEG IS TESTED AT ITS OWN WORST EXTREME. A long dies on the candle's
//     LOW, a short on its HIGH. Both extremes occurred inside that minute, and
//     testing either leg on the close would miss the moment it died.
//
//  2. ISOLATED AND PORTFOLIO ARE DIFFERENT QUESTIONS. Cross-venue, the legs are
//     independent -- each exchange sees a naked position, so each is tested
//     separately at its own worst. Single-venue under portfolio margin the legs
//     share an account, so the price can only be ONE thing at a time: the
//     account is evaluated at the low and again at the high, and the worse of
//     the two is taken.
//
//  3. LIQUIDATION IS TERMINAL. Once a leg dies the walk stops. It does not
//     recover when the price comes back, because the position no longer exists
//     -- the venue closed it and kept the margin. Continuing to accrue funding
//     past that point is how a backtest invents money.
//
//  4. ONE PRICE SERIES PER SYMBOL. Cross-venue legs sit on venues whose prices
//     differ by the basis -- 10 to 200 bps on what we measured. Liquidation
//     distances run from 900 to 3300 bps, so pricing both legs off one series
//     is an approximation an order of magnitude smaller than the thing being
//     measured. It is stated here rather than buried.

// Mode says whether the legs share a margin account.
type Mode int

const (
	// ModeIsolated is the cross-venue world: two exchanges, neither aware of
	// the other, each leg standing alone.
	ModeIsolated Mode = iota

	// ModePortfolio is the single-venue world: one account, legs offset.
	ModePortfolio
)

func (m Mode) String() string {
	if m == ModePortfolio {
		return "portfolio (one venue)"
	}
	return "isolated (two venues)"
}

// LegSpec identifies one side of a recorded position.
type LegSpec struct {
	Venue      string
	Symbol     string
	EntryPrice float64
}

// Subject is a position to replay.
//
// FundingBps and CostBps are what the paper book ACTUALLY recorded, not a
// model. The replay only decides whether the position survived long enough to
// keep them.
type Subject struct {
	Label string

	Long  LegSpec
	Short LegSpec

	NotionalUSD float64
	OpenedAt    time.Time
	ClosedAt    time.Time // zero means still open; the replay runs to the end of the data

	FundingBps float64
	CostBps    float64

	// LongIsSpot marks the long leg as a fully-funded holding rather than a
	// levered position.
	//
	// In cash-and-carry you OWN the spot. It cannot be liquidated and it
	// carries no maintenance margin -- it IS the collateral. Modelling it as a
	// levered leg invents a liquidation that cannot happen and doubles the
	// margin requirement, which understates the strategy twice over.
	LongIsSpot bool
}

// Result is one replay at one leverage.
type Result struct {
	Label    string
	Leverage float64
	Mode     Mode

	Survived   bool
	DiedLeg    string
	DiedAt     time.Time
	DiedPrice  float64
	LiqLongAt  float64
	LiqShortAt float64

	// WorstRatio is the highest margin ratio reached. 1.0 is death; anything
	// above roughly 0.5 is a position that was in real trouble.
	WorstRatio float64
	WorstAt    time.Time
	WorstPrice float64

	CapitalUSD float64
	NetUSD     float64
	ReturnPct  float64

	Candles int
	Ok      bool
	Err     string
}

// Replay walks one subject at one leverage.
//
// schedules is keyed by "venue|symbol".
// Replay walks a subject where both legs track the same price series.
func Replay(s Subject, series Series, schedules map[string]margin.Schedule,
	leverage float64, mode Mode) Result {
	return ReplayPair(s, series, series, schedules, leverage, mode)
}

// ReplayPair walks a subject whose legs have SEPARATE price series.
//
// This is the honest form. One series makes the two legs cancel exactly, so a
// netted hedge can never lose equity and "never liquidated" follows from the
// arithmetic rather than from the market. Two series carry the real basis.
func ReplayPair(s Subject, longSeries, shortSeries Series, schedules map[string]margin.Schedule,
	leverage float64, mode Mode) Result {

	r := Result{Label: s.Label, Leverage: leverage, Mode: mode}

	if leverage <= 0 || s.NotionalUSD <= 0 || s.Long.EntryPrice <= 0 || s.Short.EntryPrice <= 0 {
		r.Err = "subject is not fully specified"
		return r
	}

	longSched, okL := schedules[s.Long.Venue+"|"+s.Long.Symbol]
	shortSched, okS := schedules[s.Short.Venue+"|"+s.Short.Symbol]
	if !okL || !okS {
		r.Err = "no verified risk schedule for one or both legs"
		return r
	}

	longLev := leverage
	if s.LongIsSpot {
		// Owned outright. No leverage, no liquidation, no maintenance.
		longLev = 1
	}
	long := margin.Leg{
		Venue: s.Long.Venue, Symbol: s.Long.Symbol, Side: margin.Long,
		EntryPrice: s.Long.EntryPrice, QtyBase: s.NotionalUSD / s.Long.EntryPrice,
		Leverage: longLev,
	}
	short := margin.Leg{
		Venue: s.Short.Venue, Symbol: s.Short.Symbol, Side: margin.Short,
		EntryPrice: s.Short.EntryPrice, QtyBase: s.NotionalUSD / s.Short.EntryPrice,
		Leverage: leverage,
	}

	// Capital: both legs' margin. In portfolio mode the venue would require
	// far less, but posting the same amount in both modes is what makes the
	// comparison mean something -- identical money, different netting.
	r.CapitalUSD = long.Posted() + short.Posted()

	if p, err := long.LiquidationPrice(longSched); err == nil {
		r.LiqLongAt = p
	}
	if p, err := short.LiquidationPrice(shortSched); err == nil {
		r.LiqShortAt = p
	}

	end := s.ClosedAt
	if end.IsZero() {
		if _, to, ok := longSeries.Span(); ok {
			end = to
		} else {
			r.Err = "no price history"
			return r
		}
	}
	candles := longSeries.Between(s.OpenedAt, end)
	shortByTs := map[int64]Candle{}
	for _, c := range shortSeries.Between(s.OpenedAt, end) {
		shortByTs[c.TsMs] = c
	}
	if len(candles) == 0 {
		r.Err = fmt.Sprintf("no mark price between %s and %s",
			s.OpenedAt.Format("01-02 15:04"), end.Format("01-02 15:04"))
		return r
	}
	r.Survived = true

	for _, c := range candles {
		// Counted as walked, not as available. A position that died in minute
		// three did not live through the other four hundred.
		r.Candles++

		var ratio float64
		var at float64
		var died string

		switch mode {
		case ModePortfolio:
			// One account: the price is one thing at a time, so evaluate the
			// whole account at the low and again at the high and keep the
			// worse. The legs offset within each scenario.
			sc, hasShort := shortByTs[c.TsMs]
			if !hasShort {
				continue
			}
			// Evaluate at both extremes of the minute. Within each scenario
			// the legs move together, offset by their own basis.
			for _, sc2 := range [][2]float64{{c.Low, sc.Low}, {c.High, sc.High}} {
				ls, e1 := long.Isolated(sc2[0], longSched)
				ss, e2 := short.Isolated(sc2[1], shortSched)
				if e1 != nil || e2 != nil {
					continue
				}
				px := sc2[1]
				equity := ls.CollateralUSD + ls.UnrealizedUSD + ss.CollateralUSD + ss.UnrealizedUSD
				maint := ss.MaintenanceUSD
				if !s.LongIsSpot {
					// A levered long carries its own maintenance. Owned spot
					// does not -- it is the collateral.
					maint += ls.MaintenanceUSD
				}
				var rr float64
				if equity <= 0 {
					rr = math.Inf(1)
				} else {
					rr = maint / equity
				}
				if rr > ratio {
					ratio, at = rr, px
				}
			}
			if ratio >= 1 {
				died = "account"
			}

		default:
			// Two venues: each leg stands alone, and each faces its own worst
			// extreme within this minute.
			sc, hasShort := shortByTs[c.TsMs]
			if !hasShort {
				// No matching minute on the other series. Skip rather than
				// reuse this leg's price, which would zero the basis.
				continue
			}
			ls, e1 := long.Isolated(c.Low, longSched)
			ss, e2 := short.Isolated(sc.High, shortSched)
			if e1 != nil || e2 != nil {
				continue
			}
			if ls.MarginRatio > ratio {
				ratio, at = ls.MarginRatio, c.Low
			}
			if ss.MarginRatio > ratio {
				ratio, at = ss.MarginRatio, sc.High
			}
			switch {
			case ls.Liquidated && ss.Liquidated:
				died = "both"
			case ls.Liquidated:
				died = "long " + s.Long.Venue
			case ss.Liquidated:
				died = "short " + s.Short.Venue
			}
		}

		if ratio > r.WorstRatio {
			r.WorstRatio = ratio
			r.WorstAt = c.Time()
			r.WorstPrice = at
		}

		if died != "" {
			// Terminal. The venue closed it and kept the margin; nothing that
			// happens afterwards belongs to this position.
			r.Survived = false
			r.DiedLeg = died
			r.DiedAt = c.Time()
			r.DiedPrice = at
			break
		}
	}

	// Economics. A survivor keeps what the book recorded. A casualty loses the
	// margin on the dead leg -- and in the isolated case is left holding a
	// naked position whose further losses are not modelled here, so this
	// figure is the OPTIMISTIC end of what a liquidation costs.
	if r.Survived {
		r.NetUSD = (s.FundingBps - s.CostBps) / 10000 * s.NotionalUSD
	} else if mode == ModePortfolio {
		r.NetUSD = -r.CapitalUSD
	} else if r.DiedLeg == "both" {
		r.NetUSD = -r.CapitalUSD
	} else {
		r.NetUSD = -r.CapitalUSD / 2
	}
	if r.CapitalUSD > 0 {
		r.ReturnPct = r.NetUSD / r.CapitalUSD * 100
	}
	r.Ok = true
	return r
}

// Describe renders one result as a line.
func (r Result) Describe() string {
	if !r.Ok {
		return fmt.Sprintf("%-38s %4.0fx  SKIPPED: %s", r.Label, r.Leverage, r.Err)
	}
	if !r.Survived {
		return fmt.Sprintf(
			"%-38s %4.0fx  LIQUIDATED  %s at %s (mark %.6f)  net $%+.2f (%.1f%%)",
			r.Label, r.Leverage, r.DiedLeg, r.DiedAt.Format("01-02 15:04"),
			r.DiedPrice, r.NetUSD, r.ReturnPct)
	}
	return fmt.Sprintf(
		"%-38s %4.0fx  survived    worst margin %5.1f%% at %s  net $%+.2f (%.1f%%)",
		r.Label, r.Leverage, r.WorstRatio*100, r.WorstAt.Format("01-02 15:04"),
		r.NetUSD, r.ReturnPct)
}

// Summary aggregates many results at one leverage.
type Summary struct {
	Leverage   float64
	Mode       Mode
	Total      int
	Survived   int
	Liquidated int
	NetUSD     float64
	CapitalUSD float64
	ReturnPct  float64
	WorstRatio float64
	// AnyLiquidated is true when at least one position died, so WorstRatio
	// describes the survivors and not the worst case.
	AnyLiquidated bool
}

// Summarise rolls results up.
func Summarise(rs []Result, leverage float64, mode Mode) Summary {
	s := Summary{Leverage: leverage, Mode: mode}
	for _, r := range rs {
		if !r.Ok || r.Leverage != leverage || r.Mode != mode {
			continue
		}
		s.Total++
		if r.Survived {
			s.Survived++
		} else {
			s.Liquidated++
		}
		s.NetUSD += r.NetUSD
		s.CapitalUSD += r.CapitalUSD
		// WorstRatio describes SURVIVORS ONLY.
		//
		// A liquidated position's final ratio is at or above 1.0 by
		// definition, so including it produced figures like "331.8%
		// (survivors)" -- which is a contradiction, since anything past 100%
		// is a death. What a reader needs is how close the survivors came.
		if !r.Survived {
			s.AnyLiquidated = true
			continue
		}
		if !math.IsInf(r.WorstRatio, 1) && r.WorstRatio > s.WorstRatio {
			s.WorstRatio = r.WorstRatio
		}
	}
	if s.CapitalUSD > 0 {
		s.ReturnPct = s.NetUSD / s.CapitalUSD * 100
	}
	return s
}
