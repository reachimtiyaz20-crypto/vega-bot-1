package live

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/execution"
)

// RECONCILIATION IS WHERE THE FOUR ORIGINAL BUGS ARE MADE IMPOSSIBLE
//
// The predecessor bot's PnL was a single float that only ever went up:
//
//	totalFundingEarned += payment    // never -=, never fees, never exit cost
//
// Three separate lies in one line. Funding can be negative and it never
// subtracted. Fees existed and it never counted them. The round trip was paid
// on every rotation and it never appeared at all. The number could not go
// down, so it always looked like a winning strategy.
//
// The rules here:
//
//  1. NOTHING PERSISTS. PnL is recomputed from scratch on every pass, from the
//     venue's own settled records. There is no stored running total, so there
//     is nothing that can drift, and nothing a bug can inflate over time. The
//     only sums here are local, over a ledger that was just read.
//
//  2. EVERY COMPONENT IS SIGNED. Funding is summed as reported, including
//     negative payments. Fees are always subtracted. A position that lost
//     money reports a negative number.
//
//  3. AN UNKNOWN INPUT MAKES THE ANSWER UNKNOWN. If a fee could not be read,
//     or a fee arrived in BNB and cannot be converted, the PnL is marked
//     INCOMPLETE and the gap is named. It is not computed as though the
//     missing piece were zero. A fee of unknown size is not a fee of zero.
//
//  4. THE VENUE'S NUMBER AND OURS ARE BOTH KEPT. Where they disagree, that is
//     journaled as a divergence rather than resolved by preferring one. A
//     disagreement means one of them has a bug, and that is worth knowing.

// DefaultTolerancePct is how far VEGA's arithmetic may sit from the venue's
// before the gap is called material. Small differences are expected: rounding,
// a settlement landing between two reads, a fee credited a moment later.
const DefaultTolerancePct = 0.5

// PositionPnL is one position's profit and loss, built only from settled
// numbers.
//
// All monetary fields are quote currency (USDT). All are signed. A negative
// NetUSD is a loss and is reported as such.
type PositionPnL struct {
	PositionID string    `json:"position_id"`
	Venue      string    `json:"venue"`
	Symbol     string    `json:"symbol"`
	At         time.Time `json:"at"`

	OpenedAt time.Time `json:"opened_at"`
	ClosedAt time.Time `json:"closed_at,omitempty"`
	HoldDays float64   `json:"hold_days"`
	IsOpen   bool      `json:"is_open"`

	// --- entry, from the fills ---
	SpotEntryUSD float64 `json:"spot_entry_usd"`
	PerpEntryUSD float64 `json:"perp_entry_usd"`
	Quantity     float64 `json:"quantity"`

	// DeployedUSD is what the position actually ties up: BOTH legs. The
	// return figures in the brief are quoted on notional; on deployed capital
	// they are roughly half, and this is the denominator that matters to
	// someone deciding whether to fund the account.
	DeployedUSD float64 `json:"deployed_usd"`

	// --- the three components, all signed ---

	// FundingUSD is the sum of SETTLED funding payments from the venue's income
	// ledger, filtered to this symbol and this position's lifetime. Negative
	// entries are included. This is the only revenue line.
	FundingUSD      float64 `json:"funding_usd"`
	FundingCount    int     `json:"funding_count"`
	FundingWorst    float64 `json:"funding_worst_payment"`
	FundingNegative int     `json:"funding_negative_count"`

	// FeesUSD is every fee the venue charged, as a NEGATIVE number. Four legs
	// on a completed round trip.
	FeesUSD float64 `json:"fees_usd"`

	// SlippageUSD is the measured cost of crossing the spread on the legs that
	// have executed, as a NEGATIVE number. Measured against the reference
	// prices captured at decision time, not assumed.
	SlippageUSD float64 `json:"slippage_usd"`

	// PriceDriftUSD is what the hedge failed to cancel: the residual from the
	// two legs not being exactly equal in size. In a perfect hedge it is zero.
	// It is separated out because a large value here means the hedge is not
	// doing its job, which no amount of funding income makes acceptable.
	PriceDriftUSD float64 `json:"price_drift_usd"`

	// --- the answer ---
	NetUSD float64 `json:"net_usd"`
	NetBps float64 `json:"net_bps"`

	// AnnualizedPctOnDeployed extrapolates the realised rate. Only meaningful
	// once a position has run long enough to have collected several
	// settlements; below that it is noise multiplied by 365.
	AnnualizedPctOnDeployed float64 `json:"annualized_pct_on_deployed"`

	// Complete is false when any input could not be read. When false, NetUSD
	// is a LOWER BOUND on cost and an UPPER BOUND on profit -- never a result.
	Complete bool     `json:"complete"`
	Gaps     []string `json:"gaps,omitempty"`
}

// String renders a PnL line honestly.
func (p PositionPnL) String() string {
	status := "OPEN"
	if !p.IsOpen {
		status = "CLOSED"
	}
	s := fmt.Sprintf("%s %s %s: funding %+.4f, fees %+.4f, slippage %+.4f, drift %+.4f => NET %+.4f USD (%+.2f bps on $%.2f deployed, %.2f days)",
		p.Venue, p.Symbol, status,
		p.FundingUSD, p.FeesUSD, p.SlippageUSD, p.PriceDriftUSD,
		p.NetUSD, p.NetBps, p.DeployedUSD, p.HoldDays)
	if !p.Complete {
		s += fmt.Sprintf("  [INCOMPLETE: %s]", strings.Join(p.Gaps, "; "))
	}
	return s
}

// Report is one reconciliation pass across every tracked position.
type Report struct {
	At   time.Time `json:"at"`
	Mode string    `json:"mode"`

	Positions []PositionPnL `json:"positions"`

	// Divergences are disagreements between VEGA and the venue. An empty list
	// is the good case and is still worth journaling: it is the evidence that
	// the reconciliation ran and agreed.
	Divergences []execution.Divergence `json:"divergences"`

	TotalNetUSD      float64 `json:"total_net_usd"`
	TotalDeployedUSD float64 `json:"total_deployed_usd"`
	TotalFundingUSD  float64 `json:"total_funding_usd"`
	TotalFeesUSD     float64 `json:"total_fees_usd"`

	// AllComplete is false if ANY position had a gap. The total is then not a
	// number anyone should quote.
	AllComplete   bool     `json:"all_complete"`
	MaterialCount int      `json:"material_divergence_count"`
	Warnings      []string `json:"warnings,omitempty"`
	Summary       string   `json:"summary"`
}

// Reconciler compares VEGA's records against each venue's ledger.
type Reconciler struct {
	readers      map[string]execution.AccountReader
	TolerancePct float64
}

// NewReconciler builds one.
func NewReconciler(readers map[string]execution.AccountReader) *Reconciler {
	return &Reconciler{readers: readers, TolerancePct: DefaultTolerancePct}
}

// Reconcile recomputes PnL for every position from settled venue data.
func (r *Reconciler) Reconcile(ctx context.Context, positions []*LivePosition, mode execution.Mode) (Report, error) {
	rep := Report{At: time.Now().UTC(), Mode: string(mode), AllComplete: true}

	// Group by venue so the income ledger is read once per venue rather than
	// once per position. On a rate-limited API that difference matters.
	byVenue := map[string][]*LivePosition{}
	earliest := map[string]time.Time{}
	for _, p := range positions {
		byVenue[p.Venue] = append(byVenue[p.Venue], p)
		if t, ok := earliest[p.Venue]; !ok || p.OpenedAt.Before(t) {
			earliest[p.Venue] = p.OpenedAt
		}
	}

	for venue, ps := range byVenue {
		reader, ok := r.readers[venue]
		if !ok {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("no account reader for %s: its positions cannot be reconciled at all", venue))
			rep.AllComplete = false
			continue
		}

		// Read funding a little before the earliest open. A settlement can land
		// microseconds before our own timestamp for the open, and missing the
		// first payment of a position understates it permanently.
		since := earliest[venue].Add(-time.Hour)
		payments, ferr := reader.FundingSince(ctx, since)
		if ferr != nil {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("%s funding ledger unreadable (%v): revenue for its positions is UNKNOWN, not zero", venue, ferr))
			rep.AllComplete = false
		}

		snap, serr := reader.Snapshot(ctx)
		if serr != nil {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("%s account snapshot unreadable (%v): hedge sizes cannot be verified", venue, serr))
			rep.AllComplete = false
		}

		for _, p := range ps {
			pnl := r.pnl(p, payments, ferr != nil)
			rep.Positions = append(rep.Positions, pnl)

			if !pnl.Complete {
				rep.AllComplete = false
			}
			rep.TotalNetUSD += pnl.NetUSD
			rep.TotalDeployedUSD += pnl.DeployedUSD
			rep.TotalFundingUSD += pnl.FundingUSD
			rep.TotalFeesUSD += pnl.FeesUSD

			if serr == nil && p.Open() {
				rep.Divergences = append(rep.Divergences, r.checkHedge(p, snap)...)
			}
		}
	}

	for _, d := range rep.Divergences {
		if d.Material {
			rep.MaterialCount++
		}
	}

	sort.Slice(rep.Positions, func(i, j int) bool {
		return rep.Positions[i].OpenedAt.Before(rep.Positions[j].OpenedAt)
	})

	switch {
	case len(rep.Positions) == 0:
		rep.Summary = "no positions to reconcile"
	case !rep.AllComplete:
		rep.Summary = fmt.Sprintf("%d position(s), net %+.4f USD -- INCOMPLETE, do not quote this figure",
			len(rep.Positions), rep.TotalNetUSD)
	case rep.MaterialCount > 0:
		rep.Summary = fmt.Sprintf("%d position(s), net %+.4f USD on $%.2f deployed, but %d MATERIAL divergence(s) from the venue",
			len(rep.Positions), rep.TotalNetUSD, rep.TotalDeployedUSD, rep.MaterialCount)
	default:
		rep.Summary = fmt.Sprintf("%d position(s), net %+.4f USD on $%.2f deployed, reconciled clean",
			len(rep.Positions), rep.TotalNetUSD, rep.TotalDeployedUSD)
	}

	return rep, nil
}

// pnl computes one position's PnL from settled data only.
func (r *Reconciler) pnl(p *LivePosition, payments []execution.FundingPayment, fundingUnreadable bool) PositionPnL {
	out := PositionPnL{
		PositionID: p.ID,
		Venue:      p.Venue,
		Symbol:     p.Symbol,
		At:         time.Now().UTC(),
		OpenedAt:   p.OpenedAt,
		ClosedAt:   p.ClosedAt,
		IsOpen:     p.Open(),
		Quantity:   p.SpotEntry.FilledQty,
		Complete:   true,
	}

	end := p.ClosedAt
	if out.IsOpen {
		end = time.Now().UTC()
	}
	if !p.OpenedAt.IsZero() {
		out.HoldDays = end.Sub(p.OpenedAt).Hours() / 24
	}

	// --- entry sizes, from what the venue said filled ---
	out.SpotEntryUSD = p.SpotEntry.QuoteSpent
	out.PerpEntryUSD = p.PerpEntry.QuoteSpent
	if out.SpotEntryUSD <= 0 && p.SpotEntry.FilledQty > 0 && p.SpotEntry.AvgFillPrice > 0 {
		out.SpotEntryUSD = p.SpotEntry.FilledQty * p.SpotEntry.AvgFillPrice
	}
	if out.PerpEntryUSD <= 0 && p.PerpEntry.FilledQty > 0 && p.PerpEntry.AvgFillPrice > 0 {
		out.PerpEntryUSD = p.PerpEntry.FilledQty * p.PerpEntry.AvgFillPrice
	}
	out.DeployedUSD = out.SpotEntryUSD + out.PerpEntryUSD

	if out.DeployedUSD <= 0 {
		out.Complete = false
		out.Gaps = append(out.Gaps, "entry fills report no value; every ratio below is meaningless")
	}

	// --- funding: SIGNED, from the venue's ledger, filtered to this position ---
	if fundingUnreadable {
		out.Complete = false
		out.Gaps = append(out.Gaps, "funding ledger could not be read; revenue is UNKNOWN, not zero")
	} else {
		seen := make(map[string]bool, len(payments))
		for _, pay := range payments {
			if !strings.EqualFold(pay.Symbol, p.Symbol) {
				continue
			}
			if pay.SettleAt.Before(p.OpenedAt) {
				continue
			}
			if !out.IsOpen && pay.SettleAt.After(p.ClosedAt) {
				continue
			}
			// Dedupe on transaction ID. Paging a ledger by timestamp can return
			// the same settlement twice at a page boundary, and a
			// double-counted payment is a fictional profit.
			if pay.TranID != "" {
				if seen[pay.TranID] {
					continue
				}
				seen[pay.TranID] = true
			}

			out.FundingUSD += pay.Amount
			out.FundingCount++
			if pay.Amount < 0 {
				out.FundingNegative++
			}
			if pay.Amount < out.FundingWorst {
				out.FundingWorst = pay.Amount
			}
		}
	}

	// --- fees: ALWAYS subtracted, and named when unknown ---
	legs := []struct {
		name string
		res  execution.OrderResult
		used bool
	}{
		{"spot entry", p.SpotEntry, p.SpotEntry.FilledQty > 0},
		{"perp entry", p.PerpEntry, p.PerpEntry.FilledQty > 0},
		{"spot exit", p.SpotExit, p.SpotExit.FilledQty > 0},
		{"perp exit", p.PerpExit, p.PerpExit.FilledQty > 0},
	}
	for _, l := range legs {
		if !l.used {
			continue
		}
		switch {
		case l.res.FeePaid == 0:
			// Binance futures does not report a fee on the order response. A
			// zero here means "not reported", and treating it as free is
			// exactly bug 1 from the previous bot.
			out.Complete = false
			out.Gaps = append(out.Gaps,
				fmt.Sprintf("%s fee not reported by venue; true cost is higher than shown", l.name))
		case l.res.FeeAsset == "USDT" || l.res.FeeAsset == "USD" || l.res.FeeAsset == "":
			out.FeesUSD -= math.Abs(l.res.FeePaid)
		default:
			// A fee paid in BNB or the base coin cannot be added to dollars
			// without a conversion rate we do not have here.
			out.Complete = false
			out.Gaps = append(out.Gaps,
				fmt.Sprintf("%s fee of %.8f is in %s, not USDT; not converted, so cost is understated",
					l.name, l.res.FeePaid, l.res.FeeAsset))
		}

		// --- slippage: measured, per leg ---
		if bps, ok := l.res.SlippageBps(); ok {
			notional := l.res.QuoteSpent
			if notional <= 0 {
				notional = l.res.FilledQty * l.res.AvgFillPrice
			}
			// bps is already signed so that positive means it cost us money.
			out.SlippageUSD -= bps / 10_000 * notional
		} else {
			out.Complete = false
			out.Gaps = append(out.Gaps,
				fmt.Sprintf("%s slippage unmeasurable (no reference price)", l.name))
		}
	}

	// --- price drift: what the hedge did not cancel ---
	// The two legs move in opposite directions. If they are the same size, the
	// moves cancel exactly and this is zero. Any residual is unhedged
	// exposure that happened to be profitable or not.
	if !out.IsOpen && p.SpotExit.FilledQty > 0 && p.PerpExit.FilledQty > 0 {
		spotPnL := p.SpotExit.QuoteSpent - p.SpotEntry.QuoteSpent
		// The perp is SHORT, so it profits when the exit cost is lower than
		// the entry proceeds.
		perpPnL := p.PerpEntry.QuoteSpent - p.PerpExit.QuoteSpent
		out.PriceDriftUSD = spotPnL + perpPnL
	}

	// --- the answer. Note every term is added with its own sign. ---
	out.NetUSD = out.FundingUSD + out.FeesUSD + out.SlippageUSD + out.PriceDriftUSD

	if out.DeployedUSD > 0 {
		out.NetBps = out.NetUSD / out.DeployedUSD * 10_000
		if out.HoldDays >= 1 {
			out.AnnualizedPctOnDeployed = out.NetUSD / out.DeployedUSD * (365 / out.HoldDays) * 100
		}
	}
	return out
}

// checkHedge compares our view of an open position against the venue's.
//
// This is the check that would have caught a leg that quietly failed, a
// position closed manually in the web UI, or a size that drifted. VEGA's
// number is never silently replaced -- both are kept and the gap is reported.
func (r *Reconciler) checkHedge(p *LivePosition, snap execution.AccountSnapshot) []execution.Divergence {
	tol := r.TolerancePct
	if tol <= 0 {
		tol = DefaultTolerancePct
	}

	// What we believe: a SHORT perp of the size the entry filled.
	expected := -p.PerpEntry.FilledQty

	var actual float64
	found := false
	for _, vp := range snap.Positions {
		if strings.EqualFold(vp.Symbol, p.Symbol) {
			actual = vp.PositionAmt
			found = true
			break
		}
	}

	d := execution.Compare(p.Venue, p.Symbol, "perp_position_amt", expected, actual, tol)
	if !found {
		// The venue has no such position. If we think one is open, that is the
		// single most important divergence this function can report.
		d.Material = true
	}
	return []execution.Divergence{d}
}
