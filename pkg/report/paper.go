package report

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/imtiyaz/vega-bot/pkg/funding"
)

// THE PAPER WORKBOOK
//
// Rewritten after the first version showed a breakdown that contradicted its
// own total: it printed funding 0.59 and cost -109.76 against a net of
// -186.24, because funding.Stats does not count the same costs that
// Position.NetBps does.
//
// A report whose numbers do not add up is worse than no report. Someone will
// eventually trust one of the two figures, and there is no way to know which.
//
// So every total here is computed from the POSITIONS THEMSELVES, in this file,
// with the arithmetic laid out line by line so it can be checked by eye:
//
//	funding collected  -  entry costs  -  exit costs  =  NET
//
// funding.Stats is still used for counts, never for money.

// GateCount is how many symbol-observations a refusal reason accounted for.
type GateCount struct {
	Code  string
	Count int
}

// PaperInput is one paper workbook's contents.
type PaperInput struct {
	GeneratedAt time.Time
	Day         string

	Stats  funding.Stats
	Open   []funding.Position
	Closed []funding.Position

	Observations int
	HealthChecks int
	Gates        []GateCount
	Notes        []string
}

// ledger is the reconciled money view, computed here so it always adds up.
type ledger struct {
	FundingBps float64
	EntryBps   float64
	ExitBps    float64
	NetBps     float64

	FundingUSD float64
	EntryUSD   float64
	ExitUSD    float64
	NetUSD     float64

	RealizedUSD   float64
	UnrealizedUSD float64

	Count       int
	OpenCount   int
	ClosedCount int
	Profitable  int
	Losing      int

	MeanHoldDays      float64
	MeanRoundTripBps  float64
	MeanFundingPerDay float64
	BreakEvenDays     float64
}

// posCost returns the entry and exit cost for one position. An open position
// has not paid its exit yet, but it is unavoidable, so it is carried at the
// entry cost -- the same assumption Position.NetBps makes.
func posCost(p funding.Position) (entry, exit float64) {
	entry = p.EntryCostBps
	exit = p.ExitCostBps
	if !p.Closed() {
		exit = p.EntryCostBps
	}
	return entry, exit
}

// fundingPerDayBps is what this position earns per day at its entry rate.
func fundingPerDayBps(p funding.Position) float64 {
	if p.IntervalHours <= 0 {
		return 0
	}
	return p.EntryRatePct * 100 * (24 / p.IntervalHours)
}

func buildLedger(open, closed []funding.Position) ledger {
	var l ledger
	now := time.Now().UTC()

	var holdSum, rtSum, fpdSum float64
	var fpdCount int

	all := make([]funding.Position, 0, len(open)+len(closed))
	all = append(all, open...)
	all = append(all, closed...)

	for _, p := range all {
		entry, exit := posCost(p)
		net := p.FundingCollectedBps - entry - exit
		usd := net / 10000 * p.NotionalUSD

		l.FundingBps += p.FundingCollectedBps
		l.EntryBps += entry
		l.ExitBps += exit
		l.NetBps += net

		l.FundingUSD += p.FundingCollectedBps / 10000 * p.NotionalUSD
		l.EntryUSD += entry / 10000 * p.NotionalUSD
		l.ExitUSD += exit / 10000 * p.NotionalUSD
		l.NetUSD += usd

		if p.Closed() {
			l.RealizedUSD += usd
			l.ClosedCount++
		} else {
			l.UnrealizedUSD += usd
			l.OpenCount++
		}
		if net >= 0 {
			l.Profitable++
		} else {
			l.Losing++
		}

		holdSum += p.HeldDays(now)
		rtSum += entry + exit
		if f := fundingPerDayBps(p); f > 0 {
			fpdSum += f
			fpdCount++
		}
		l.Count++
	}

	if l.Count > 0 {
		l.MeanHoldDays = holdSum / float64(l.Count)
		l.MeanRoundTripBps = rtSum / float64(l.Count)
	}
	if fpdCount > 0 {
		l.MeanFundingPerDay = fpdSum / float64(fpdCount)
	}
	if l.MeanFundingPerDay > 0 {
		l.BreakEvenDays = l.MeanRoundTripBps / l.MeanFundingPerDay
	}
	return l
}

// periodRow is one line of a Daily / Weekly / Monthly sheet.
type periodRow struct {
	Label      string
	Closed     int
	Realized   float64
	Cumulative float64
}

// groupClosed buckets realised PnL by a period key.
func groupClosed(closed []funding.Position, key func(time.Time) string) []periodRow {
	sums := map[string]float64{}
	counts := map[string]int{}
	for _, p := range closed {
		if !p.Closed() {
			continue
		}
		k := key(p.ClosedAt)
		_, exit := posCost(p)
		net := p.FundingCollectedBps - p.EntryCostBps - exit
		sums[k] += net / 10000 * p.NotionalUSD
		counts[k]++
	}

	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]periodRow, 0, len(keys))
	var cum float64
	for _, k := range keys {
		cum += sums[k]
		out = append(out, periodRow{Label: k, Closed: counts[k], Realized: sums[k], Cumulative: cum})
	}
	return out
}

func isoWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// WritePaper builds the paper workbook.
func WritePaper(dir string, in PaperInput) (string, error) {
	if in.GeneratedAt.IsZero() {
		in.GeneratedAt = time.Now().UTC()
	}
	if in.Day == "" {
		in.Day = in.GeneratedAt.Format("2006-01-02")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("vega-paper-%s.xlsx", in.Day))

	f := excelize.NewFile()
	defer f.Close()

	st, err := newStyles(f)
	if err != nil {
		return "", fmt.Errorf("report: building styles: %w", err)
	}

	l := buildLedger(in.Open, in.Closed)

	if err := paperSummary(f, st, in, l); err != nil {
		return "", err
	}
	if err := periodSheet(f, st, "Daily P&L", "Date",
		groupClosed(in.Closed, func(t time.Time) string { return t.Format("2006-01-02") }), l); err != nil {
		return "", err
	}
	if err := periodSheet(f, st, "Weekly P&L", "ISO week",
		groupClosed(in.Closed, isoWeek), l); err != nil {
		return "", err
	}
	if err := periodSheet(f, st, "Monthly P&L", "Month",
		groupClosed(in.Closed, func(t time.Time) string { return t.Format("2006-01") }), l); err != nil {
		return "", err
	}
	if err := paperPositions(f, st, "Open Positions", in.Open, false); err != nil {
		return "", err
	}
	if err := paperPositions(f, st, "Closed Positions", in.Closed, true); err != nil {
		return "", err
	}
	if err := paperMeasurement(f, st, in); err != nil {
		return "", err
	}

	if idx, err := f.GetSheetIndex("Summary"); err == nil && idx >= 0 {
		f.SetActiveSheet(idx)
	}
	_ = f.DeleteSheet("Sheet1")

	if err := f.SaveAs(path); err != nil {
		return "", fmt.Errorf("report: saving %s: %w", path, err)
	}
	return path, nil
}

// --- Summary ----------------------------------------------------------------

func paperSummary(f *excelize.File, st styles, in PaperInput, l ledger) error {
	const sh = "Summary"
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	_ = f.SetColWidth(sh, "A", "A", 32)
	_ = f.SetColWidth(sh, "B", "B", 16)
	_ = f.SetColWidth(sh, "C", "C", 52)

	r := 1
	set(f, sh, 1, r, "VEGA PAPER REPORT  "+in.Day)
	style(f, sh, 1, r, 3, r, st.header)
	r += 2

	set(f, sh, 1, r, "PAPER -- NO ORDERS WERE PLACED")
	set(f, sh, 3, r, "Fills modelled from observed book + verified fees.")
	style(f, sh, 1, r, 3, r, st.warn)
	r += 2

	sec(f, sh, st, &r, "THE ANSWER")
	money(f, sh, st, &r, "Net result (USD)", round(l.NetUSD, 2))
	money(f, sh, st, &r, "  of which realised", round(l.RealizedUSD, 2))
	money(f, sh, st, &r, "  of which unrealised", round(l.UnrealizedUSD, 2))
	set(f, sh, 3, r-3, "Realised = closed. Unrealised = still open.")

	set(f, sh, 1, r, "Verdict")
	switch {
	case l.Count == 0:
		set(f, sh, 2, r, "NO DATA")
		style(f, sh, 2, r, 2, r, st.unmeasured)
	case l.NetUSD < 0:
		set(f, sh, 2, r, "LOSING MONEY")
		set(f, sh, 3, r, "Do not commit capital against this.")
		style(f, sh, 1, r, 3, r, st.warn)
	default:
		set(f, sh, 2, r, "PROFITABLE")
		set(f, sh, 3, r, "Small sample. A good week proves the week was good.")
		style(f, sh, 2, r, 2, r, st.good)
	}
	r += 2

	sec(f, sh, st, &r, "HOW THAT NUMBER IS MADE (USD)")
	money(f, sh, st, &r, "Funding collected", round(l.FundingUSD, 2))
	set(f, sh, 3, r-1, "Signed. Negative settlements included.")
	money(f, sh, st, &r, "Entry costs paid", round(-l.EntryUSD, 2))
	set(f, sh, 3, r-1, "Buy spot + short perp: fees + measured spread.")
	money(f, sh, st, &r, "Exit costs (paid or committed)", round(-l.ExitUSD, 2))
	set(f, sh, 3, r-1, "Open positions carry this as an unavoidable liability.")

	set(f, sh, 1, r, "= NET")
	set(f, sh, 2, r, round(l.NetUSD, 2))
	style(f, sh, 1, r, 2, r, st.header)
	set(f, sh, 3, r, "These three lines add to this. Check them.")
	r += 2

	sec(f, sh, st, &r, "POSITIONS")
	kv(f, sh, &r, "Open", l.OpenCount)
	kv(f, sh, &r, "Closed", l.ClosedCount)
	kv(f, sh, &r, "Profitable", l.Profitable)
	kv(f, sh, &r, "Losing", l.Losing)
	kv(f, sh, &r, "Average hold (days)", round(l.MeanHoldDays, 2))
	r++

	sec(f, sh, st, &r, "WHY")
	kv(f, sh, &r, "Average round trip cost (bps)", round(l.MeanRoundTripBps, 2))
	set(f, sh, 3, r-1, "Four fee legs plus spread, in and out.")
	kv(f, sh, &r, "Average funding earned (bps/day)", round(l.MeanFundingPerDay, 2))
	kv(f, sh, &r, "Days needed to break even", round(l.BreakEvenDays, 1))
	set(f, sh, 3, r-1, "= round trip / funding per day.")
	kv(f, sh, &r, "Days actually held (average)", round(l.MeanHoldDays, 2))

	if l.BreakEvenDays > 0 && l.MeanHoldDays > 0 && l.MeanHoldDays < l.BreakEvenDays {
		set(f, sh, 1, r, "PROBLEM")
		set(f, sh, 2, r, fmt.Sprintf("%.0fx too early", l.BreakEvenDays/l.MeanHoldDays))
		set(f, sh, 3, r, "Positions close before covering the round trip. "+
			"Every exit locks in the full cost.")
		style(f, sh, 1, r, 3, r, st.warn)
		r++
	}
	r++

	sec(f, sh, st, &r, "MEASUREMENT")
	kv(f, sh, &r, "Observations journaled", in.Observations)
	kv(f, sh, &r, "Health records", in.HealthChecks)

	for _, n := range in.Notes {
		set(f, sh, 1, r, "Note")
		set(f, sh, 3, r, n)
		style(f, sh, 1, r, 3, r, st.warn)
		r++
	}
	return nil
}

// sec writes a section header row.
func sec(f *excelize.File, sh string, st styles, r *int, title string) {
	set(f, sh, 1, *r, title)
	style(f, sh, 1, *r, 3, *r, st.header)
	(*r)++
}

// --- period sheets ----------------------------------------------------------

func periodSheet(f *excelize.File, st styles, sh, label string, rows []periodRow, l ledger) error {
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	_ = f.SetColWidth(sh, "A", "A", 16)
	_ = f.SetColWidth(sh, "B", "D", 20)
	_ = f.SetColWidth(sh, "E", "E", 46)

	writeHeader(f, sh, st, []string{label, "Positions closed", "Realised USD", "Cumulative USD", "Note"})

	r := 2
	for _, p := range rows {
		writeRow(f, sh, r, []any{p.Label, p.Closed, round(p.Realized, 2), round(p.Cumulative, 2)})
		if p.Realized < 0 {
			style(f, sh, 3, r, 3, r, st.moneyBad)
		} else {
			style(f, sh, 3, r, 3, r, st.money)
		}
		if p.Cumulative < 0 {
			style(f, sh, 4, r, 4, r, st.moneyBad)
		} else {
			style(f, sh, 4, r, 4, r, st.money)
		}
		r++
	}

	if r == 2 {
		set(f, sh, 1, 2, "Nothing has closed yet.")
		set(f, sh, 5, 2, "This sheet fills in as positions close. Open positions "+
			"appear on the Summary as unrealised.")
		style(f, sh, 1, 2, 5, 2, st.unmeasured)
		r = 3
	}

	r++
	set(f, sh, 1, r, "REALISED ONLY")
	set(f, sh, 5, r, fmt.Sprintf("Currently open positions are worth %.2f USD "+
		"and are NOT in the rows above.", round(l.UnrealizedUSD, 2)))
	style(f, sh, 1, r, 5, r, st.header)
	return nil
}

// --- position sheets --------------------------------------------------------

func paperPositions(f *excelize.File, st styles, sh string, ps []funding.Position, closed bool) error {
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}

	head := []string{
		"Symbol", "Venue", "Status",
		"NET USD", "NET bps", "Return on capital %",
		"Funding earned bps", "Entry cost bps", "Exit cost bps",
		"Rate at entry %", "Rate now %", "Interval h",
		"Funding per day bps", "Break-even days",
		"Held days", "Settlements", "Negative settlements",
		"Notional USD", "Capital USD", "Opened (UTC)",
	}
	if closed {
		head = append(head, "Closed (UTC)", "Why it closed")
	}
	writeHeader(f, sh, st, head)

	_ = f.SetColWidth(sh, "A", "A", 12)
	_ = f.SetColWidth(sh, "T", "U", 22)
	_ = f.SetColWidth(sh, "V", "V", 64)

	now := time.Now().UTC()
	r := 2
	for i := range ps {
		p := ps[i]
		entry, exit := posCost(p)
		net := p.FundingCollectedBps - entry - exit
		netUSD := net / 10000 * p.NotionalUSD
		fpd := fundingPerDayBps(p)
		be := 0.0
		if fpd > 0 {
			be = (entry + exit) / fpd
		}

		status := "OPEN"
		if closed {
			status = "CLOSED"
		}

		writeRow(f, sh, r, []any{
			p.Symbol, p.Venue, status,
			round(netUSD, 2), round(net, 2), round(p.ReturnOnCapitalPct(), 3),
			round(p.FundingCollectedBps, 3), round(entry, 2), round(exit, 2),
			round(p.EntryRatePct, 6), round(p.LastRatePct, 6), p.IntervalHours,
			round(fpd, 2), round(be, 1),
			round(p.HeldDays(now), 2), p.IntervalsCollected, p.NegativeIntervals,
			round(p.NotionalUSD, 2), round(p.CapitalUSD, 2),
			p.OpenedAt.Format("2006-01-02 15:04"),
		})

		if closed {
			set(f, sh, 21, r, p.ClosedAt.Format("2006-01-02 15:04"))
			set(f, sh, 22, r, p.CloseReason)
		}

		if net < 0 {
			style(f, sh, 4, r, 5, r, st.moneyBad)
		} else {
			style(f, sh, 4, r, 5, r, st.money)
		}
		if p.NegativeIntervals > 0 {
			style(f, sh, 17, r, 17, r, st.warn)
		}
		// Held far less than break-even is the churn signature.
		if be > 0 && p.HeldDays(now) < be/2 {
			style(f, sh, 14, r, 15, r, st.warn)
		}
		r++
	}

	if r == 2 {
		if closed {
			set(f, sh, 1, 2, "No positions have closed yet.")
		} else {
			set(f, sh, 1, 2, "No positions are open.")
		}
		style(f, sh, 1, 2, 1, 2, st.unmeasured)
	}
	return nil
}

// --- refusals ---------------------------------------------------------------

func paperMeasurement(f *excelize.File, st styles, in PaperInput) error {
	const sh = "Refusals"
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	writeHeader(f, sh, st, []string{"Gate", "Count", "What it means"})
	_ = f.SetColWidth(sh, "A", "A", 16)
	_ = f.SetColWidth(sh, "B", "B", 14)
	_ = f.SetColWidth(sh, "C", "C", 92)

	meaning := map[string]string{
		"ok":          "Passed. Funding covers the full round trip.",
		"nospot":      "No spot market -- the hedge cannot be built.",
		"thinperp":    "Perp volume too low to exit at the quoted price.",
		"thinspot":    "Spot volume too low. Same problem, other leg.",
		"shallow":     "Top of book smaller than the position. Fill would sweep levels.",
		"unmeasured":  "Book unreadable, so slippage is unknown. Unknown cost is refused.",
		"unverified":  "Venue fees not checked against its fee page.",
		"negfunding":  "Funding negative -- the short leg PAYS. A cost, not an opportunity.",
		"zerofunding": "Funding exactly zero. No revenue.",
		"notcovering": "Funding positive but below the round trip. The interest-rate floor.",
	}

	r := 2
	for _, g := range in.Gates {
		writeRow(f, sh, r, []any{g.Code, g.Count, meaning[g.Code]})
		if g.Code == "ok" {
			style(f, sh, 1, r, 1, r, st.good)
		}
		r++
	}
	if r == 2 {
		set(f, sh, 1, 2, "No gate data for this day.")
		style(f, sh, 1, 2, 1, 2, st.unmeasured)
		return nil
	}

	r++
	set(f, sh, 1, r, "A LARGE REFUSAL COUNT IS THE SYSTEM WORKING")
	set(f, sh, 3, r, "The predecessor bots had no gates, found opportunities everywhere, and lost money on all of them.")
	style(f, sh, 1, r, 3, r, st.header)
	return nil
}
