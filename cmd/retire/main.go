// Command retire archives a cross-venue book properly: it closes every open
// position with a stated reason, then moves the file aside and leaves a note.
//
// # WHY THIS EXISTS
//
// Four books were retired between 5 and 20 August by renaming their files.
// None closed its positions first, so 22 of them are still written as "open":
// never marked, never exited, never reconciled. Nothing in those files says the
// writer is gone, so the dashboard rendered a dead book as live for 38 hours.
//
// Renaming a file is not retiring a book. A retired book must say what happened
// to everything it was holding.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/crossvenue"
)

func main() {
	dataDir := flag.String("data", "", "data directory holding cross_positions.json")
	reason := flag.String("reason", "", "why this book is being retired; REQUIRED")
	force := flag.Bool("force", false, "retire even if the file looks freshly written")
	flag.Parse()

	if *dataDir == "" || *reason == "" {
		fmt.Fprintln(os.Stderr, "usage: retire -data <dir> -reason \"<why>\"")
		fmt.Fprintln(os.Stderr, "\nA reason is mandatory. An unexplained archive is the problem this fixes.")
		os.Exit(2)
	}

	path := filepath.Join(*dataDir, "cross_positions.json")
	fi, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no book at %s: %v\n", path, err)
		os.Exit(1)
	}

	// A book being written right now has a live writer. Retiring it would race
	// the service and lose whatever it wrote next.
	if age := time.Since(fi.ModTime()); age < 2*time.Minute && !*force {
		fmt.Fprintf(os.Stderr,
			"%s was written %s ago, so its writer is probably still running.\n"+
				"Stop the service first, or pass -force if you are certain.\n",
			path, age.Truncate(time.Second))
		os.Exit(1)
	}

	book, err := crossvenue.NewBook(*dataDir, crossvenue.DefaultConfig(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading book: %v\n", err)
		os.Exit(1)
	}
	openBefore, closedBefore := book.Snapshot()

	now := time.Now().UTC()
	n, err := book.Retire(*reason, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retiring: %v\n", err)
		os.Exit(1)
	}

	stamp := now.Format("20060102-1504")
	archived := filepath.Join(*dataDir, fmt.Sprintf("cross_positions.retired-%s.json", stamp))
	raw, err := os.ReadFile(path)
	if err == nil {
		err = os.WriteFile(archived, raw, 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "archiving: %v\n", err)
		os.Exit(1)
	}

	note := filepath.Join(*dataDir, fmt.Sprintf("WHY-retired-%s.txt", stamp))
	body := fmt.Sprintf("Retired %s UTC.\n\n%s\n\n"+
		"%d positions were open and have been closed with reason \"book retired\".\n"+
		"Each is dated to its LAST OBSERVATION, not to the retirement time, so held\n"+
		"hours are not inflated by the period after the writer stopped.\n\n"+
		"%d positions were already closed and are unchanged.\n\n"+
		"Archived copy: %s\n",
		now.Format("2006-01-02 15:04"), *reason, n, len(closedBefore), filepath.Base(archived))
	if err := os.WriteFile(note, []byte(body), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "writing note: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("retired %s\n", path)
	fmt.Printf("  %d open positions closed, %d already closed left alone\n", n, len(closedBefore))
	fmt.Printf("  archived to %s\n", filepath.Base(archived))
	fmt.Printf("  note written to %s\n", filepath.Base(note))
	if len(openBefore) != n {
		fmt.Fprintf(os.Stderr, "WARNING: saw %d open before but closed %d\n", len(openBefore), n)
		os.Exit(1)
	}
}
