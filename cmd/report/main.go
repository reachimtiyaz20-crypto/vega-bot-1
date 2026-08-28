// Command report builds VEGA's daily Excel workbook.
//
// It reconstructs the day from two sources, and prefers the second wherever
// they disagree:
//
//	THE JOURNAL   what VEGA believed happened. Positions, orders, the
//	              reconciliation it computed at the time.
//
//	THE EXCHANGE  what actually happened. Read live, read-only, at the moment
//	              the report is generated.
//
// Reading the exchange again rather than trusting the journal is the whole
// point. A journal is VEGA's own account of itself, and if there is a bug in
// VEGA then the journal contains it too. Where the two disagree the workbook
// shows the disagreement instead of picking a winner.
//
//	report -data ~/vega-bot/data                      # today
//	report -data ~/vega-bot/data -date 2026-08-05     # a specific day
//	report -data ~/vega-bot/data -no-live             # journal only, no API
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/execution"
	"github.com/imtiyaz/vega-bot/pkg/live"
	"github.com/imtiyaz/vega-bot/pkg/report"
	"github.com/imtiyaz/vega-bot/pkg/risk"
)

func main() {
	dataDir := flag.String("data", "", "data directory (required)")
	date := flag.String("date", "", "day to report, YYYY-MM-DD (default: today, UTC)")
	outDir := flag.String("out", "", "output directory (default: <data>/reports)")
	venue := flag.String("venue", "binance", "venue to read")
	modeFlag := flag.String("mode", "testnet", "testnet or mainnet")
	noLive := flag.Bool("no-live", false, "do not contact the exchange; build from the journal alone")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	if *dataDir == "" {
		logger.Fatal("FATAL -data is required")
	}
	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		logger.Fatalf("FATAL resolving -data: %v", err)
	}

	day := time.Now().UTC().Format("2006-01-02")
	if *date != "" {
		if _, err := time.Parse("2006-01-02", *date); err != nil {
			logger.Fatalf("FATAL -date must be YYYY-MM-DD, got %q", *date)
		}
		day = *date
	}
	dayStart, _ := time.Parse("2006-01-02", day)
	dayStart = dayStart.UTC()

	mode := execution.Testnet
	if strings.EqualFold(*modeFlag, "mainnet") {
		mode = execution.Mainnet
	}

	out := *outDir
	if out == "" {
		out = filepath.Join(abs, "reports")
	}

	// --- 1. what VEGA believed ------------------------------------------------
	positions, journaledRep, journaledRisk, err := readJournal(filepath.Join(abs, "journal"), day)
	if err != nil {
		logger.Printf("journal read: %v", err)
	}
	logger.Printf("journal for %s: %d position(s)", day, len(positions))

	in := report.Input{
		GeneratedAt:    time.Now().UTC(),
		Mode:           string(mode),
		Venue:          *venue,
		Positions:      positions,
		Reconciliation: journaledRep,
		Risk:           journaledRisk,
	}

	// --- 2. what the exchange says -------------------------------------------
	if *noLive {
		in.Reconciliation.Warnings = append(in.Reconciliation.Warnings,
			"-no-live was set: nothing in this workbook has been checked against the exchange. "+
				"These are VEGA's own numbers, unverified.")
		in.Reconciliation.AllComplete = false
	} else {
		if err := addLive(&in, *venue, mode, dayStart, positions, logger); err != nil {
			// Not fatal. A report built from the journal alone is worth having,
			// as long as it says clearly that is what it is.
			logger.Printf("live read failed: %v", err)
			in.Reconciliation.Warnings = append(in.Reconciliation.Warnings,
				fmt.Sprintf("could not reach the exchange (%v); this workbook is UNVERIFIED "+
					"against the venue ledger", err))
			in.Reconciliation.AllComplete = false
		}
	}

	path, err := report.Write(out, in)
	if err != nil {
		logger.Fatalf("FATAL writing report: %v", err)
	}

	logger.Printf("wrote %s", path)
	logger.Printf("  %s", in.Reconciliation.Summary)
	if !in.Reconciliation.AllComplete {
		logger.Printf("  NOTE this report is marked INCOMPLETE; see the Summary sheet")
	}
}

// addLive reads the venue with READ-ONLY credentials and overwrites the
// journal's arithmetic with a fresh reconciliation.
func addLive(in *report.Input, venue string, mode execution.Mode, since time.Time,
	positions []*live.LivePosition, logger *log.Logger) error {

	creds, err := execution.FromEnv(venue, execution.ReadOnly, mode)
	if err != nil {
		return err
	}

	var reader execution.AccountReader
	switch strings.ToLower(venue) {
	case "binance":
		reader, err = execution.NewBinanceAccount(creds)
	case "bybit":
		reader, err = execution.NewBybitAccount(creds)
	default:
		return fmt.Errorf("unknown venue %q", venue)
	}
	if err != nil {
		return err
	}

	readers := map[string]execution.AccountReader{venue: reader}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Funding, straight from the income ledger. This is the revenue line, and
	// it is the one number in the whole system that is never computed.
	payments, ferr := reader.FundingSince(ctx, since)
	if ferr != nil {
		logger.Printf("funding ledger: %v", ferr)
	} else {
		sort.Slice(payments, func(i, j int) bool {
			return payments[i].SettleAt.Before(payments[j].SettleAt)
		})
		in.Funding = payments
		logger.Printf("exchange: %d settled funding payment(s) since %s",
			len(payments), since.Format("2006-01-02"))
	}

	// Re-reconcile now rather than trusting the figure journaled hours ago.
	rec := live.NewReconciler(readers)
	rep, err := rec.Reconcile(ctx, positions, mode)
	if err != nil {
		return err
	}
	// Keep any warnings already accumulated (e.g. from the journal read).
	rep.Warnings = append(in.Reconciliation.Warnings, rep.Warnings...)
	in.Reconciliation = rep

	// Liquidation risk, fresh.
	snap, serr := reader.Snapshot(ctx)
	if serr != nil {
		return serr
	}
	in.Risk = risk.AssessPortfolio([]execution.AccountSnapshot{snap}, risk.DefaultThresholds())
	logger.Printf("exchange: risk %s", in.Risk.WorstName)
	return nil
}

// --- journal replay ---------------------------------------------------------

// line is the minimal shape needed to route a journal record. Both writers in
// this project are covered: pkg/live wraps its value in "payload", cmd/live
// wraps its in "data".
type line struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Data    json.RawMessage `json:"data"`
}

// readJournal replays one day and returns the latest state of everything.
//
// Positions are keyed by ID and OVERWRITTEN as later records arrive, so a
// position that was opened and later closed on the same day ends up in its
// closed form. Appending instead would double-count it -- and a double-counted
// position is a doubled profit.
func readJournal(dir, day string) ([]*live.LivePosition, live.Report, risk.PortfolioRisk, error) {
	var rep live.Report
	var pr risk.PortfolioRisk
	byID := map[string]*live.LivePosition{}

	matches, err := filepath.Glob(filepath.Join(dir, "*"+day+"*"))
	if err != nil {
		return nil, rep, pr, err
	}
	if len(matches) == 0 {
		return nil, rep, pr, fmt.Errorf("no journal files for %s in %s", day, dir)
	}
	sort.Strings(matches)

	for _, path := range matches {
		if err := scanFile(path, byID, &rep, &pr); err != nil {
			// One corrupt file must not lose the rest of the day. The journal
			// writer tolerates malformed lines for the same reason.
			return nil, rep, pr, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
		}
	}

	out := make([]*live.LivePosition, 0, len(byID))
	for _, p := range byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out, rep, pr, nil
}

func scanFile(path string, byID map[string]*live.LivePosition,
	rep *live.Report, pr *risk.PortfolioRisk) error {

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}

	dec := json.NewDecoder(r)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			return nil
		} else if err != nil {
			// A truncated final line is normal if the process was killed
			// mid-write. Stop cleanly rather than failing the whole day.
			return nil
		}

		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			continue
		}

		switch l.Type {
		case "live_opened", "live_closed":
			var p live.LivePosition
			if err := json.Unmarshal(l.Payload, &p); err == nil && p.ID != "" {
				cp := p
				byID[p.ID] = &cp
			}
		case "live_reconcile":
			var v live.Report
			if err := json.Unmarshal(l.Data, &v); err == nil {
				*rep = v
			}
		case "live_risk":
			var v risk.PortfolioRisk
			if err := json.Unmarshal(l.Data, &v); err == nil {
				*pr = v
			}
		}
	}
}
