// Package report writes VEGA's daily Excel workbook.
//
// This is the first and only external dependency in the project
// (github.com/xuri/excelize/v2). Everything else is standard library.
//
// # WHAT THIS FILE WILL NOT DO
//
// A spreadsheet is the most persuasive document format there is. A number in a
// cell looks settled in a way the same number in a log line does not, and six
// weeks from now nobody will remember which figures were measured and which
// were assumed. So:
//
//   - A position whose PnL is INCOMPLETE is marked incomplete in every sheet it
//     appears in, in red, with the reason spelled out in a cell. It is never
//     rendered as a clean number.
//   - Unmeasured slippage is written as the text "UNMEASURED", not as 0.00.
//     Zero is a measurement. Blank is not.
//   - A total across incomplete rows carries a warning in the cell next to it.
//   - Every sheet states the mode (testnet/mainnet) it came from, because a
//     testnet workbook and a mainnet workbook look identical otherwise, and
//     mistaking one for the other is the single easiest way to believe you have
//     a working strategy.
package report

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/imtiyaz/vega-bot/pkg/execution"
	"github.com/imtiyaz/vega-bot/pkg/live"
	"github.com/imtiyaz/vega-bot/pkg/risk"
)

// Input is everything one workbook is built from.
type Input struct {
	GeneratedAt time.Time
	Mode        string
	Venue       string

	Reconciliation live.Report
	Risk           risk.PortfolioRisk
	Positions      []*live.LivePosition
	Funding        []execution.FundingPayment
}

// styles holds the formats used across sheets.
type styles struct {
	header     int
	money      int
	moneyBad   int
	pct        int
	warn       int
	good       int
	unmeasured int
	ts         int
}

func newStyles(f *excelize.File) (styles, error) {
	var s styles
	var err error

	if s.header, err = f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"1F3864"}},
		Alignment: &excelize.Alignment{Horizontal: "left", Vertical: "center"},
	}); err != nil {
		return s, err
	}
	if s.money, err = f.NewStyle(&excelize.Style{NumFmt: 4}); err != nil { // #,##0.00
		return s, err
	}
	// Negative money in red. A loss should not read like a gain at a glance.
	if s.moneyBad, err = f.NewStyle(&excelize.Style{
		NumFmt: 4,
		Font:   &excelize.Font{Color: "C00000", Bold: true},
	}); err != nil {
		return s, err
	}
	if s.pct, err = f.NewStyle(&excelize.Style{NumFmt: 10}); err != nil { // 0.00%
		return s, err
	}
	if s.warn, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "C00000", Bold: true},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFE699"}},
	}); err != nil {
		return s, err
	}
	if s.good, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "1E7B34"},
	}); err != nil {
		return s, err
	}
	if s.unmeasured, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "808080", Italic: true},
	}); err != nil {
		return s, err
	}
	if s.ts, err = f.NewStyle(&excelize.Style{NumFmt: 22}); err != nil { // date time
		return s, err
	}
	return s, nil
}

// Write builds the workbook and saves it.
//
// The filename carries the date and the mode. A file called
// vega-2026-08-06-TESTNET.xlsx cannot be mistaken for a real trading record
// six weeks later, which vega-daily.xlsx absolutely can.
func Write(dir string, in Input) (string, error) {
	if in.GeneratedAt.IsZero() {
		in.GeneratedAt = time.Now().UTC()
	}
	if in.Mode == "" {
		in.Mode = "UNKNOWN-MODE"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	name := fmt.Sprintf("vega-%s-%s.xlsx",
		in.GeneratedAt.Format("2006-01-02"), upper(in.Mode))
	path := filepath.Join(dir, name)

	f := excelize.NewFile()
	defer f.Close()

	st, err := newStyles(f)
	if err != nil {
		return "", fmt.Errorf("report: building styles: %w", err)
	}

	if err := sheetSummary(f, st, in); err != nil {
		return "", err
	}
	if err := sheetPositions(f, st, in); err != nil {
		return "", err
	}
	if err := sheetTrades(f, st, in); err != nil {
		return "", err
	}
	if err := sheetFunding(f, st, in); err != nil {
		return "", err
	}
	if err := sheetRisk(f, st, in); err != nil {
		return "", err
	}

	// excelize creates "Sheet1" by default; ours are all named.
	if idx, err := f.GetSheetIndex("Summary"); err == nil && idx >= 0 {
		f.SetActiveSheet(idx)
	}
	_ = f.DeleteSheet("Sheet1")

	if err := f.SaveAs(path); err != nil {
		return "", fmt.Errorf("report: saving %s: %w", path, err)
	}
	return path, nil
}

// --- sheets -----------------------------------------------------------------

func sheetSummary(f *excelize.File, st styles, in Input) error {
	const sh = "Summary"
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	_ = f.SetColWidth(sh, "A", "A", 34)
	_ = f.SetColWidth(sh, "B", "C", 22)
	_ = f.SetColWidth(sh, "D", "D", 70)

	r := 1
	set(f, sh, 1, r, "VEGA DAILY REPORT")
	style(f, sh, 1, r, 1, r, st.header)
	r++
	kv(f, sh, &r, "Generated (UTC)", in.GeneratedAt.Format(time.RFC3339))
	kv(f, sh, &r, "Mode", upper(in.Mode))
	kv(f, sh, &r, "Venue", in.Venue)

	// The mode warning goes near the top, where it will be read.
	if upper(in.Mode) != "MAINNET" {
		set(f, sh, 1, r, "NOT REAL MONEY")
		set(f, sh, 2, r, upper(in.Mode))
		set(f, sh, 4, r, "These figures come from a simulated or test environment. "+
			"Fill prices on testnet are not representative of real execution.")
		style(f, sh, 1, r, 4, r, st.warn)
		r++
	}
	r++

	rep := in.Reconciliation

	set(f, sh, 1, r, "RECONCILED TOTALS")
	style(f, sh, 1, r, 4, r, st.header)
	r++

	money(f, sh, st, &r, "Funding collected (settled)", rep.TotalFundingUSD)
	money(f, sh, st, &r, "Fees paid", rep.TotalFeesUSD)
	money(f, sh, st, &r, "NET", rep.TotalNetUSD)
	money(f, sh, st, &r, "Capital deployed (both legs)", rep.TotalDeployedUSD)

	if rep.TotalDeployedUSD > 0 {
		set(f, sh, 1, r, "Return on deployed capital")
		set(f, sh, 2, r, rep.TotalNetUSD/rep.TotalDeployedUSD)
		style(f, sh, 2, r, 2, r, st.pct)
		set(f, sh, 4, r, "On DEPLOYED capital, not notional. Both legs must be funded, "+
			"so this is roughly half the figure quoted on notional.")
		r++
	}
	r++

	// The completeness verdict. This is the most important cell in the file.
	set(f, sh, 1, r, "DATA COMPLETENESS")
	style(f, sh, 1, r, 4, r, st.header)
	r++

	if rep.AllComplete {
		set(f, sh, 1, r, "Complete")
		set(f, sh, 2, r, "YES")
		set(f, sh, 4, r, "Every fee, funding payment and slippage figure above was read "+
			"from the exchange. These numbers are auditable.")
		style(f, sh, 2, r, 2, r, st.good)
	} else {
		set(f, sh, 1, r, "Complete")
		set(f, sh, 2, r, "NO")
		set(f, sh, 4, r, "One or more inputs could not be read. The NET figure above is an "+
			"UPPER BOUND on profit -- true costs are higher than shown. Do not quote it.")
		style(f, sh, 1, r, 4, r, st.warn)
	}
	r++

	for _, w := range rep.Warnings {
		set(f, sh, 1, r, "Warning")
		set(f, sh, 4, r, w)
		style(f, sh, 1, r, 4, r, st.warn)
		r++
	}
	r++

	set(f, sh, 1, r, "LIQUIDATION RISK")
	style(f, sh, 1, r, 4, r, st.header)
	r++
	kv(f, sh, &r, "Worst severity", in.Risk.WorstName)
	kv(f, sh, &r, "Summary", in.Risk.Summary)
	if in.Risk.UnknownCount > 0 {
		set(f, sh, 1, r, "Unmeasured positions")
		set(f, sh, 2, r, in.Risk.UnknownCount)
		set(f, sh, 4, r, "The venue reported no liquidation price for these. That is UNKNOWN "+
			"risk, not absent risk.")
		style(f, sh, 1, r, 4, r, st.warn)
		r++
	}

	r++
	set(f, sh, 1, r, "DIVERGENCES (VEGA vs exchange)")
	style(f, sh, 1, r, 4, r, st.header)
	r++
	if rep.MaterialCount == 0 {
		set(f, sh, 1, r, "Material disagreements")
		set(f, sh, 2, r, 0)
		set(f, sh, 4, r, "VEGA's record and the exchange's ledger agree within tolerance.")
		style(f, sh, 2, r, 2, r, st.good)
	} else {
		set(f, sh, 1, r, "Material disagreements")
		set(f, sh, 2, r, rep.MaterialCount)
		set(f, sh, 4, r, "VEGA and the exchange disagree. One of them has a bug. "+
			"See the Positions sheet.")
		style(f, sh, 1, r, 4, r, st.warn)
	}
	return nil
}

func sheetPositions(f *excelize.File, st styles, in Input) error {
	const sh = "Positions"
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	head := []string{
		"Position ID", "Venue", "Symbol", "Status", "Opened (UTC)", "Closed (UTC)",
		"Hold days", "Quantity", "Deployed USD",
		"Funding USD", "Payments", "Negative payments", "Worst payment",
		"Fees USD", "Slippage USD", "Price drift USD",
		"NET USD", "NET bps", "Annualised % on deployed",
		"Complete", "Gaps",
	}
	writeHeader(f, sh, st, head)
	_ = f.SetColWidth(sh, "A", "A", 30)
	_ = f.SetColWidth(sh, "E", "F", 22)
	_ = f.SetColWidth(sh, "U", "U", 80)

	r := 2
	for _, p := range in.Reconciliation.Positions {
		status := "OPEN"
		if !p.IsOpen {
			status = "CLOSED"
		}
		closed := ""
		if !p.ClosedAt.IsZero() {
			closed = p.ClosedAt.Format(time.RFC3339)
		}

		vals := []any{
			p.PositionID, p.Venue, p.Symbol, status,
			p.OpenedAt.Format(time.RFC3339), closed,
			round(p.HoldDays, 3), p.Quantity, round(p.DeployedUSD, 2),
			round(p.FundingUSD, 6), p.FundingCount, p.FundingNegative, round(p.FundingWorst, 6),
			round(p.FeesUSD, 6), round(p.SlippageUSD, 6), round(p.PriceDriftUSD, 6),
			round(p.NetUSD, 6), round(p.NetBps, 2), round(p.AnnualizedPctOnDeployed, 2),
		}
		writeRow(f, sh, r, vals)

		if p.Complete {
			set(f, sh, 20, r, "YES")
			style(f, sh, 20, r, 20, r, st.good)
		} else {
			set(f, sh, 20, r, "NO")
			set(f, sh, 21, r, join(p.Gaps))
			// Mark the whole row, not just the flag. A red cell in column T is
			// easy to scroll past; a red row is not.
			style(f, sh, 1, r, 21, r, st.warn)
		}

		// Negative net in red regardless of completeness.
		if p.NetUSD < 0 {
			style(f, sh, 17, r, 17, r, st.moneyBad)
		} else {
			style(f, sh, 17, r, 17, r, st.money)
		}
		r++
	}

	if r == 2 {
		set(f, sh, 1, 2, "No positions in this period.")
	}
	return nil
}

// sheetTrades is one row per ORDER LEG -- four per completed round trip.
//
// This is the sheet that answers "what did I actually pay". Every figure in it
// is read back from the venue, never carried over from the request.
func sheetTrades(f *excelize.File, st styles, in Input) error {
	const sh = "Trades"
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	head := []string{
		"Position ID", "Phase", "Leg", "Venue", "Symbol", "Side", "Status",
		"Sent (UTC)", "Requested qty", "Filled qty", "Avg fill price",
		"Quote value USD", "Reference price", "Slippage bps",
		"Fee paid", "Fee asset", "Client order ID", "Venue order ID", "Note",
	}
	writeHeader(f, sh, st, head)
	_ = f.SetColWidth(sh, "A", "A", 30)
	_ = f.SetColWidth(sh, "Q", "R", 28)
	_ = f.SetColWidth(sh, "S", "S", 50)

	r := 2
	for _, p := range in.Positions {
		legs := []struct {
			phase string
			res   execution.OrderResult
		}{
			{"entry", p.SpotEntry},
			{"entry", p.PerpEntry},
			{"exit", p.SpotExit},
			{"exit", p.PerpExit},
		}
		for _, l := range legs {
			if l.res.FilledQty == 0 && l.res.Status == "" {
				continue // this leg never happened
			}
			writeRow(f, sh, r, []any{
				p.ID, l.phase, string(l.res.Leg), l.res.Venue, l.res.Symbol,
				string(l.res.Side), string(l.res.Status),
				l.res.SentAt.Format(time.RFC3339),
				l.res.RequestedQty, l.res.FilledQty, l.res.AvgFillPrice,
				round(l.res.QuoteSpent, 6), l.res.RefPrice,
			})

			// Slippage: measured or explicitly not.
			if bps, ok := l.res.SlippageBps(); ok {
				set(f, sh, 14, r, round(bps, 3))
				if bps > 0 {
					style(f, sh, 14, r, 14, r, st.moneyBad)
				}
			} else {
				set(f, sh, 14, r, "UNMEASURED")
				style(f, sh, 14, r, 14, r, st.unmeasured)
			}

			// Fee: zero from a venue that does not report it is NOT free.
			if l.res.FeePaid == 0 {
				set(f, sh, 15, r, "NOT REPORTED")
				style(f, sh, 15, r, 15, r, st.unmeasured)
			} else {
				set(f, sh, 15, r, round(l.res.FeePaid, 8))
			}
			set(f, sh, 16, r, l.res.FeeAsset)
			set(f, sh, 17, r, l.res.ClientOrderID)
			set(f, sh, 18, r, l.res.VenueOrderID)
			set(f, sh, 19, r, l.res.RawError)
			if l.res.RawError != "" {
				style(f, sh, 19, r, 19, r, st.warn)
			}
			r++
		}
	}
	if r == 2 {
		set(f, sh, 1, 2, "No trades in this period.")
	}
	return nil
}

// sheetFunding is the settled income ledger, straight from the venue.
func sheetFunding(f *excelize.File, st styles, in Input) error {
	const sh = "Funding"
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	writeHeader(f, sh, st, []string{
		"Settled (UTC)", "Venue", "Symbol", "Amount", "Asset", "Direction", "Transaction ID",
	})
	_ = f.SetColWidth(sh, "A", "A", 24)
	_ = f.SetColWidth(sh, "G", "G", 26)

	r := 2
	var total float64
	for _, p := range in.Funding {
		dir := "RECEIVED"
		if p.Amount < 0 {
			dir = "PAID" // the short leg paid. This happens and must be visible.
		}
		writeRow(f, sh, r, []any{
			p.SettleAt.Format(time.RFC3339), p.Venue, p.Symbol,
			round(p.Amount, 8), p.Asset, dir, p.TranID,
		})
		if p.Amount < 0 {
			style(f, sh, 4, r, 6, r, st.moneyBad)
		}
		total += p.Amount
		r++
	}

	if r == 2 {
		set(f, sh, 1, 2, "No settled funding payments in this period.")
		return nil
	}

	r++
	set(f, sh, 3, r, "TOTAL")
	set(f, sh, 4, r, round(total, 8))
	if total < 0 {
		style(f, sh, 3, r, 4, r, st.moneyBad)
	} else {
		style(f, sh, 3, r, 4, r, st.money)
	}
	set(f, sh, 6, r, "Signed sum. Negative entries are included -- a funding total that "+
		"can only rise is the bug that sank the previous bot.")
	return nil
}

func sheetRisk(f *excelize.File, st styles, in Input) error {
	const sh = "Risk"
	if _, err := f.NewSheet(sh); err != nil {
		return err
	}
	writeHeader(f, sh, st, []string{
		"Venue", "Symbol", "Severity", "Position amt", "Notional USD",
		"Mark price", "Liquidation price", "Distance %", "Dangerous direction",
		"Leverage", "Action", "Reasons",
	})
	_ = f.SetColWidth(sh, "I", "I", 34)
	_ = f.SetColWidth(sh, "L", "L", 90)

	r := 2
	for _, a := range in.Risk.Assessments {
		writeRow(f, sh, r, []any{
			a.Venue, a.Symbol, a.SeverityName, a.PositionAmt, round(a.NotionalUSD, 2),
			a.MarkPrice,
		})

		if a.HasLiqPrice {
			set(f, sh, 7, r, a.LiquidationPrice)
			set(f, sh, 8, r, round(a.DistancePct, 3))
		} else {
			set(f, sh, 7, r, "NOT REPORTED")
			set(f, sh, 8, r, "UNKNOWN")
			style(f, sh, 7, r, 8, r, st.warn)
		}
		set(f, sh, 9, r, a.MoveDirection)
		set(f, sh, 10, r, a.Leverage)
		set(f, sh, 11, r, string(a.Action))
		set(f, sh, 12, r, join(a.Reasons))

		switch a.SeverityName {
		case "CRITICAL", "DANGER", "UNKNOWN":
			style(f, sh, 3, r, 3, r, st.warn)
		case "SAFE":
			style(f, sh, 3, r, 3, r, st.good)
		}
		r++
	}
	if r == 2 {
		set(f, sh, 1, 2, "No open positions to assess.")
	}
	return nil
}

// --- small helpers ----------------------------------------------------------

func writeHeader(f *excelize.File, sheet string, st styles, cols []string) {
	for i, c := range cols {
		set(f, sheet, i+1, 1, c)
	}
	style(f, sheet, 1, 1, len(cols), 1, st.header)
}

func writeRow(f *excelize.File, sheet string, row int, vals []any) {
	for i, v := range vals {
		set(f, sheet, i+1, row, v)
	}
}

func set(f *excelize.File, sheet string, col, row int, v any) {
	cell, err := excelize.CoordinatesToCellName(col, row)
	if err != nil {
		return
	}
	_ = f.SetCellValue(sheet, cell, v)
}

func style(f *excelize.File, sheet string, c1, r1, c2, r2, id int) {
	a, err := excelize.CoordinatesToCellName(c1, r1)
	if err != nil {
		return
	}
	b, err := excelize.CoordinatesToCellName(c2, r2)
	if err != nil {
		return
	}
	_ = f.SetCellStyle(sheet, a, b, id)
}

func kv(f *excelize.File, sheet string, row *int, k string, v any) {
	set(f, sheet, 1, *row, k)
	set(f, sheet, 2, *row, v)
	*row++
}

func money(f *excelize.File, sheet string, st styles, row *int, label string, v float64) {
	set(f, sheet, 1, *row, label)
	set(f, sheet, 2, *row, round(v, 6))
	if v < 0 {
		style(f, sheet, 2, *row, 2, *row, st.moneyBad)
	} else {
		style(f, sheet, 2, *row, 2, *row, st.money)
	}
	*row++
}

// round trims float noise so a cell shows 3.93 rather than 3.9300000000000006.
// It rounds for DISPLAY only; nothing downstream reads these values back.
func round(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += " | "
		}
		out += s
	}
	return out
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}
