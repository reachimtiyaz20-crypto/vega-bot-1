// Package risk answers one question: how close is a position to being
// liquidated, and what should be done about it.
//
// # THE THING THIS PACKAGE EXISTS FOR
//
// A cash-and-carry is long spot and SHORT the perpetual. That is delta neutral
// in economic terms: if price doubles, the spot leg gains exactly what the
// perp leg loses. So it feels safe.
//
// It is not, because the two legs sit in DIFFERENT MARGIN POOLS. When price
// rallies, the short perp loses money and burns futures margin. The spot leg
// is gaining at exactly the same rate -- but that gain is in the spot wallet,
// and the exchange will not use it to save the futures position. The perp gets
// liquidated while the hedge that was supposed to protect it sits, profitable,
// in an account the liquidation engine cannot see.
//
// The moment that happens you are no longer market neutral. You are long spot,
// unhedged, having just realised a loss on the perp -- and the usual next move
// is a retrace, which is now pure loss.
//
// The brief names this as risk 2 and calls it "the big one". During the March
// 2024 BTC run from $60K to $73K, multiple delta-neutral traders were
// liquidated on precisely this mechanism.
//
// SO: a RALLY is the danger for this strategy, not a crash. That is
// counterintuitive enough that it is worth saying twice.
package risk

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/execution"
)

// Severity is how worried to be.
type Severity int

const (
	// Unknown means the venue did not report a liquidation price.
	//
	// It sorts as MORE severe than Watch, not less. A position whose distance
	// to liquidation cannot be measured is not a safe position -- it is an
	// unmeasured one, and unmeasured risk has to be treated as present.
	Unknown Severity = iota
	Safe
	Watch
	Danger
	Critical
)

func (s Severity) String() string {
	switch s {
	case Safe:
		return "SAFE"
	case Watch:
		return "WATCH"
	case Danger:
		return "DANGER"
	case Critical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

// Action is what the operator or the bot should do.
type Action string

const (
	ActionNone     Action = "none"
	ActionAlert    Action = "alert"
	ActionTopUp    Action = "top-up-margin-or-reduce"
	ActionCloseNow Action = "close-both-legs-now"
)

// Thresholds are distances to liquidation, as a percentage of mark price,
// below which each severity applies.
//
// These are judgement calls, not measurements, and they should be revisited
// against the paper record rather than trusted because they appear in code.
//
// The defaults assume a HEDGED position on a major. For an unhedged or
// altcoin position they are far too loose: a thin altcoin can move 15% in an
// hour, which would take a position from Watch to liquidated between two
// five-minute polls.
type Thresholds struct {
	CriticalPct float64
	DangerPct   float64
	WatchPct    float64

	// MaxLeverage is the highest leverage permitted on a hedged leg. A
	// cash-and-carry does not need leverage at all -- 1x on both legs is the
	// whole point. Anything above this is a configuration error, and it is
	// the single fastest way to turn a market-neutral position into a
	// liquidation.
	MaxLeverage float64

	// RequireShortPerp asserts the hedge is the right way round. A LONG perp
	// alongside long spot is not a hedge, it is double exposure.
	RequireShortPerp bool
}

// DefaultThresholds is deliberately conservative.
//
// 15% for Watch sounds generous until you remember the poll interval is five
// minutes and a fast market can cover that in one candle.
func DefaultThresholds() Thresholds {
	return Thresholds{
		CriticalPct:      3,
		DangerPct:        7,
		WatchPct:         15,
		MaxLeverage:      2,
		RequireShortPerp: true,
	}
}

// Assessment is one position's liquidation risk.
type Assessment struct {
	Venue  string    `json:"venue"`
	Symbol string    `json:"symbol"`
	At     time.Time `json:"at"`

	PositionAmt      float64 `json:"position_amt"`
	NotionalUSD      float64 `json:"notional_usd"`
	MarkPrice        float64 `json:"mark_price"`
	LiquidationPrice float64 `json:"liquidation_price"`
	HasLiqPrice      bool    `json:"has_liq_price"`

	// DistancePct is how far price must move before liquidation, as a
	// percentage of mark. For a short, that move is UPWARD.
	DistancePct float64 `json:"distance_pct"`

	// MoveDirection says which way price has to go to kill this position.
	// Stated explicitly because "up is dangerous" is the opposite of most
	// people's intuition.
	MoveDirection string `json:"move_direction"`

	Leverage float64 `json:"leverage"`

	Severity     Severity `json:"-"`
	SeverityName string   `json:"severity"`
	Action       Action   `json:"action"`
	Reasons      []string `json:"reasons"`
}

// Assess scores one position.
func Assess(p execution.PerpPosition, t Thresholds) Assessment {
	a := Assessment{
		Venue:            p.Venue,
		Symbol:           p.Symbol,
		At:               time.Now().UTC(),
		PositionAmt:      p.PositionAmt,
		NotionalUSD:      p.NotionalUSD(),
		MarkPrice:        p.MarkPrice,
		LiquidationPrice: p.LiquidationPrice,
		HasLiqPrice:      p.HasLiquidationPrice(),
		Leverage:         p.Leverage,
	}

	if p.IsFlat() {
		a.Severity = Safe
		a.SeverityName = a.Severity.String()
		a.Action = ActionNone
		a.Reasons = []string{"position is flat"}
		return a
	}

	// Hedge integrity comes before liquidation distance. A position on the
	// wrong side is not a risky hedge, it is not a hedge.
	if t.RequireShortPerp && !p.IsShort() {
		a.Severity = Critical
		a.SeverityName = a.Severity.String()
		a.Action = ActionCloseNow
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"perp leg is LONG (%.8f) but a cash-and-carry requires SHORT. "+
				"Alongside long spot this is DOUBLE exposure, not a hedge", p.PositionAmt))
		return a
	}

	if t.MaxLeverage > 0 && p.Leverage > t.MaxLeverage {
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"leverage %.1fx exceeds the %.1fx cap; a cash-and-carry needs none",
			p.Leverage, t.MaxLeverage))
	}

	if p.IsShort() {
		a.MoveDirection = "UP -- a rally liquidates a short perp"
	} else {
		a.MoveDirection = "DOWN"
	}

	if !a.HasLiqPrice {
		// The venue gave us nothing. This is NOT safety.
		a.DistancePct = math.Inf(1)
		a.Severity = Unknown
		a.Action = ActionAlert
		a.SeverityName = a.Severity.String()
		a.Reasons = append(a.Reasons,
			"venue reported NO liquidation price. This is UNKNOWN risk, not absent risk. "+
				"Binance sends \"0\" and Bybit sends \"\" for positions it does not currently "+
				"consider at risk, but neither is a guarantee -- verify manually before relying on it")
		return a
	}

	a.DistancePct = p.DistanceToLiquidationPct()

	switch {
	case a.DistancePct <= t.CriticalPct:
		a.Severity = Critical
		a.Action = ActionCloseNow
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"liquidation is %.2f%% away (mark %.6f, liq %.6f). Close BOTH legs now -- "+
				"closing only the perp leaves naked spot",
			a.DistancePct, p.MarkPrice, p.LiquidationPrice))
	case a.DistancePct <= t.DangerPct:
		a.Severity = Danger
		a.Action = ActionTopUp
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"liquidation is %.2f%% away. Add futures margin or reduce size. "+
				"The spot leg's gain does NOT protect the perp -- different margin pool",
			a.DistancePct))
	case a.DistancePct <= t.WatchPct:
		a.Severity = Watch
		a.Action = ActionAlert
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"liquidation is %.2f%% away; within one fast candle of Danger", a.DistancePct))
	default:
		a.Severity = Safe
		a.Action = ActionNone
		a.Reasons = append(a.Reasons, fmt.Sprintf(
			"liquidation is %.2f%% away", a.DistancePct))
	}

	a.SeverityName = a.Severity.String()
	return a
}

// PortfolioRisk is every position assessed, plus the aggregate picture.
type PortfolioRisk struct {
	At          time.Time    `json:"at"`
	Assessments []Assessment `json:"assessments"`

	Worst       Severity `json:"-"`
	WorstName   string   `json:"worst_severity"`
	WorstSymbol string   `json:"worst_symbol"`

	// ClosestPct is the smallest distance to liquidation across all positions.
	// The portfolio is exactly as safe as its nearest position.
	ClosestPct float64 `json:"closest_pct"`

	TotalNotionalUSD float64 `json:"total_notional_usd"`
	UnknownCount     int     `json:"unknown_count"`
	CriticalCount    int     `json:"critical_count"`
	DangerCount      int     `json:"danger_count"`

	// Action is the most severe action any single position requires.
	Action  Action   `json:"action"`
	Summary string   `json:"summary"`
	Alerts  []string `json:"alerts,omitempty"`
}

// AssessPortfolio scores every open position on every venue.
func AssessPortfolio(snapshots []execution.AccountSnapshot, t Thresholds) PortfolioRisk {
	pr := PortfolioRisk{
		At:         time.Now().UTC(),
		Worst:      Safe,
		ClosestPct: math.Inf(1),
		Action:     ActionNone,
	}

	for _, s := range snapshots {
		for _, p := range s.OpenPositions() {
			a := Assess(p, t)
			pr.Assessments = append(pr.Assessments, a)
			pr.TotalNotionalUSD += a.NotionalUSD

			switch a.Severity {
			case Unknown:
				pr.UnknownCount++
			case Critical:
				pr.CriticalCount++
			case Danger:
				pr.DangerCount++
			}

			if a.HasLiqPrice && a.DistancePct < pr.ClosestPct {
				pr.ClosestPct = a.DistancePct
				pr.WorstSymbol = a.Venue + ":" + a.Symbol
			}
			if a.Severity > pr.Worst {
				pr.Worst = a.Severity
			}
			if actionRank(a.Action) > actionRank(pr.Action) {
				pr.Action = a.Action
			}
			if a.Severity >= Danger || a.Severity == Unknown {
				pr.Alerts = append(pr.Alerts,
					fmt.Sprintf("%s %s:%s -- %s", a.SeverityName, a.Venue, a.Symbol, a.Reasons[0]))
			}
		}
	}

	// Unknown is not Safe. If anything is unmeasured, the portfolio's headline
	// severity says so rather than reporting the average of what we could see.
	if pr.UnknownCount > 0 && pr.Worst < Danger {
		pr.Worst = Unknown
	}
	pr.WorstName = pr.Worst.String()

	sort.Slice(pr.Assessments, func(i, j int) bool {
		return pr.Assessments[i].DistancePct < pr.Assessments[j].DistancePct
	})

	switch {
	case len(pr.Assessments) == 0:
		pr.Summary = "no open positions"
	case pr.CriticalCount > 0:
		pr.Summary = fmt.Sprintf("%d position(s) CRITICAL, closest liquidation %.2f%% away on %s",
			pr.CriticalCount, pr.ClosestPct, pr.WorstSymbol)
	case pr.DangerCount > 0:
		pr.Summary = fmt.Sprintf("%d position(s) in DANGER, closest liquidation %.2f%% away on %s",
			pr.DangerCount, pr.ClosestPct, pr.WorstSymbol)
	case pr.UnknownCount > 0:
		pr.Summary = fmt.Sprintf("%d position(s) with NO liquidation price reported -- risk unmeasured",
			pr.UnknownCount)
	default:
		pr.Summary = fmt.Sprintf("%d position(s), closest liquidation %.2f%% away on %s",
			len(pr.Assessments), pr.ClosestPct, pr.WorstSymbol)
	}

	return pr
}

func actionRank(a Action) int {
	switch a {
	case ActionCloseNow:
		return 3
	case ActionTopUp:
		return 2
	case ActionAlert:
		return 1
	default:
		return 0
	}
}

// SafeToOpen reports whether a new position should be permitted given current
// portfolio risk.
//
// The rule: never add exposure while anything already open is in Danger or
// worse, and never add while anything is unmeasured. Opening a second position
// during a rally that is already threatening the first is how a single bad
// day becomes a wipeout.
func (pr PortfolioRisk) SafeToOpen() (bool, string) {
	if pr.CriticalCount > 0 {
		return false, fmt.Sprintf("refusing to open: %d position(s) already CRITICAL", pr.CriticalCount)
	}
	if pr.DangerCount > 0 {
		return false, fmt.Sprintf("refusing to open: %d position(s) already in DANGER", pr.DangerCount)
	}
	if pr.UnknownCount > 0 {
		return false, fmt.Sprintf("refusing to open: %d position(s) have NO liquidation price, so portfolio risk is unmeasured", pr.UnknownCount)
	}
	return true, "portfolio risk within thresholds"
}
