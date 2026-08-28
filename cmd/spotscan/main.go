// Command spotscan records which coins can actually be bought on spot, per
// venue, and which of those can be borrowed.
//
// It exists because VEGA chose cross-venue by default -- not because that
// structure was better, but because nothing could tell it whether the better
// one was available. Single-venue cash-and-carry captures the FULL funding
// rate; cross-venue captures only the difference between two rates. On ONG that
// was 141 bps/hr against 63.
//
// The only thing exchange.Venue knew was SpotAvailable, a per-VENUE boolean
// meaning "Binance has spot markets". True, and useless for deciding whether
// Binance has a spot market for the alt currently paying 30%. Most perp-listed
// alts do not.
//
// No orders are placed and nothing is written outside data/spot/.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/spot"
)

func main() {
	dataDir := flag.String("data", "data", "data directory")
	quiet := flag.Bool("q", false, "write only, no report")
	timeout := flag.Duration("timeout", 60*time.Second, "total time for all venues")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := &http.Client{Timeout: 20 * time.Second}
	now := time.Now().UTC()

	table := spot.Scan(ctx, client, now)

	// Attach borrow where cmd/borrow already has it. Borrow currently covers
	// four currencies on two venues, so almost every alt will come back
	// unchecked -- which the table records as unchecked rather than as
	// "cannot borrow". Widening that list is the next step, not this one.
	borrowed, berr := applyBorrow(*dataDir, table)
	if berr != nil && !*quiet {
		fmt.Fprintf(os.Stderr, "borrow join skipped: %v\n", berr)
	}

	path := spot.Path(*dataDir)
	if err := table.Save(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *quiet {
		return
	}

	fmt.Printf("%s  spot universe -> %s\n\n", now.Format(time.RFC3339), path)
	for _, line := range table.Summary() {
		fmt.Println("  " + line)
	}
	fmt.Printf("\n  %d coins with a spot market on at least one venue\n", len(table.Coins()))
	fmt.Printf("  %d borrow records attached\n", borrowed)

	var failed int
	for _, r := range table.Results {
		if !r.OK {
			failed++
		}
	}
	if failed > 0 {
		// Loud, because a partial table is the dangerous state: it looks
		// complete and every coin on the failed venue reads as unknown.
		fmt.Printf("\n  WARNING: %d venue(s) failed. Coins there are recorded UNKNOWN,\n"+
			"  not absent -- the book will not treat them as lacking spot.\n", failed)
		os.Exit(2)
	}
}

// borrowSnapshot mirrors one line of data/borrow/rates.jsonl.
type borrowSnapshot struct {
	At    time.Time `json:"at"`
	Rates []struct {
		Venue        string  `json:"venue"`
		Currency     string  `json:"currency"`
		AnnualPct    float64 `json:"annual_pct"`
		MaxBorrowUSD float64 `json:"max_borrow_usd"`
		Borrowable   bool    `json:"borrowable"`
		OK           bool    `json:"ok"`
	} `json:"rates"`
}

// applyBorrow reads the LAST snapshot in the borrow journal and attaches it.
//
// Only the last: the journal is a time series, and an older row saying a coin
// was borrowable last week is not evidence that it is now.
func applyBorrow(dataDir string, t *spot.Table) (int, error) {
	path := filepath.Join(dataDir, "borrow", "rates.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()

	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			last = line
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("scanning %s: %w", path, err)
	}
	if last == "" {
		return 0, fmt.Errorf("%s is empty", path)
	}

	var snap borrowSnapshot
	if err := json.Unmarshal([]byte(last), &snap); err != nil {
		return 0, fmt.Errorf("parsing last row of %s: %w", path, err)
	}

	n := 0
	for _, r := range snap.Rates {
		if !r.OK {
			// A failed read is not "cannot borrow". Leaving it unchecked keeps
			// CanReverseCarry closed without recording a false fact.
			continue
		}
		if r.Currency == "USDT" || r.Currency == "USDC" {
			continue // quote currencies, not tradeable legs
		}
		if t.ApplyBorrow(r.Venue, r.Currency, r.Borrowable, r.AnnualPct, r.MaxBorrowUSD) {
			n++
		}
	}
	return n, nil
}
