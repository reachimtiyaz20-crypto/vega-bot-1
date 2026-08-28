// Command paper-report builds the daily paper workbook.
//
// It reads two things and nothing else:
//
//	data/positions.json          the paper book -- open and closed positions,
//	                             written atomically by the monitor
//	data/journal/<day>.jsonl     that day's observations, for refusal counts
//
// It NEVER writes to either. The monitor owns both files and this process must
// not race it: a report that corrupted the ledger it was reporting on would be
// a spectacular own goal. Everything here is open-for-read.
//
//	paper-report -data ~/vega-bot/data
//	paper-report -data ~/vega-bot/data -date 2026-08-05
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/funding"
	"github.com/imtiyaz/vega-bot/pkg/report"
)

func main() {
	dataDir := flag.String("data", "", "data directory (required)")
	date := flag.String("date", "", "day to report, YYYY-MM-DD (default: today, UTC)")
	outDir := flag.String("out", "", "output directory (default: <data>/reports)")
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

	out := *outDir
	if out == "" {
		out = filepath.Join(abs, "reports")
	}

	// --- the ledger ----------------------------------------------------------
	//
	// NewBook reads positions.json and never writes unless Save is called,
	// which this process does not do. If the monitor is mid-write, the atomic
	// rename in saveLocked means we either see the whole old file or the whole
	// new one -- never a torn read. That rename is doing real work here.
	book, err := funding.NewBook(abs, funding.DefaultPaperConfig())
	if err != nil {
		logger.Fatalf("FATAL reading paper book: %v", err)
	}
	open, closed := book.Snapshot()
	stats := book.Stats(time.Now().UTC())

	in := report.PaperInput{
		GeneratedAt: time.Now().UTC(),
		Day:         day,
		Stats:       stats,
		Open:        open,
		Closed:      closed,
	}

	// --- the day's measurement volume ---------------------------------------
	obs, health, gates, err := scanDay(filepath.Join(abs, "journal"), day)
	if err != nil {
		// Not fatal: the ledger above is the substance of the report, and it is
		// complete without this. But say so rather than showing a silent zero.
		logger.Printf("journal: %v", err)
		in.Notes = append(in.Notes, fmt.Sprintf(
			"journal for %s could not be read (%v), so the Refusals sheet is empty. "+
				"The position figures above are unaffected -- they come from positions.json.",
			day, err))
	} else {
		in.Observations = obs
		in.HealthChecks = health
		in.Gates = gates
	}

	path, err := report.WritePaper(out, in)
	if err != nil {
		logger.Fatalf("FATAL writing report: %v", err)
	}

	logger.Printf("wrote %s", path)
	logger.Printf("  %d open, %d closed (%d win / %d loss), net %+.2f bps = %+.4f USD",
		stats.OpenCount, stats.ClosedCount, stats.Wins, stats.Losses,
		stats.TotalNetBps, stats.TotalNetUSD)
	logger.Printf("  %d observations, %d health records journaled on %s", obs, health, day)

	if stats.ClosedCount == 0 {
		logger.Printf("  NOTE nothing has closed yet, so there is no result to read")
	} else if stats.TotalNetUSD < 0 {
		logger.Printf("  NOTE the paper ledger is NET NEGATIVE after full costs")
	}
}

// journalLine is the minimum needed to count. Decoding only these two fields
// out of a 14 MB file keeps this cheap on a 1 GB box -- the full Record has
// thirty-odd fields and none of the others are wanted here.
type journalLine struct {
	Type    string `json:"type"`
	Gate30d string `json:"gate_30d"`
}

// scanDay counts observations, health records and refusal gates for one day.
func scanDay(dir, day string) (obs, health int, gates []report.GateCount, err error) {
	candidates := []string{
		filepath.Join(dir, day+".jsonl"),
		filepath.Join(dir, day+".jsonl.gz"),
	}

	var f *os.File
	var path string
	for _, c := range candidates {
		if fh, e := os.Open(c); e == nil {
			f, path = fh, c
			break
		}
	}
	if f == nil {
		return 0, 0, nil, fmt.Errorf("no journal file for %s in %s", day, dir)
	}
	defer f.Close()

	var r io.Reader = f
	if filepath.Ext(path) == ".gz" {
		zr, e := gzip.NewReader(f)
		if e != nil {
			return 0, 0, nil, e
		}
		defer zr.Close()
		r = zr
	}

	counts := map[string]int{}
	sc := bufio.NewScanner(r)
	// Records average ~670 bytes but a health record with a full gate map is
	// larger. 1 MB is generous and costs nothing.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l journalLine
		if json.Unmarshal(line, &l) != nil {
			// A malformed line is tolerated, exactly as the journal writer
			// tolerates one. Losing a day's counts to a single bad record
			// would be worse than an approximate count.
			continue
		}
		switch l.Type {
		case "obs":
			obs++
			if l.Gate30d != "" {
				counts[l.Gate30d]++
			}
		case "health":
			health++
		}
	}
	if e := sc.Err(); e != nil {
		// Return what was counted so far rather than nothing. A truncated
		// final line is normal on a file the monitor is still writing.
		return obs, health, sortGates(counts), nil
	}

	return obs, health, sortGates(counts), nil
}

// sortGates orders by count descending, so the dominant refusal reason is the
// first row of the sheet.
func sortGates(m map[string]int) []report.GateCount {
	out := make([]report.GateCount, 0, len(m))
	for k, v := range m {
		out = append(out, report.GateCount{Code: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code < out[j].Code
	})
	return out
}
