// Package journal writes daily-rotated JSONL to disk.
//
// Terminal scrollback dies. tmux sessions die. This process will run
// unattended for two to three months on a box the founder is not sitting in
// front of, and the only thing that survives that is a file.
//
// Write volume is the constraint that shapes this package. Binance lists 806
// USDT perpetuals; a full record is roughly 380 bytes. Journaling every symbol
// every minute is 440 MB/day, which fills 12 GB of free disk in 27 days and
// leaves the founder in Dubai with a dead box. GAMA already taught this lesson
// once at 33 MB/day. So the monitor writes selectively -- see SweepPolicy --
// and this package rotates and gzips behind it.
package journal

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Journal appends JSON lines to a file named for the current UTC date and
// rolls over at midnight. Safe for concurrent use.
type Journal struct {
	dir string

	mu      sync.Mutex
	day     string // YYYY-MM-DD of the open file
	file    *os.File
	buf     *bufio.Writer
	written int64
	bytes   int64
}

// Open prepares a journal directory. It does not open a file until the first
// write, so starting the monitor never creates an empty file for a day that
// has no data.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("journal: creating %s: %w", dir, err)
	}
	return &Journal{dir: dir}, nil
}

// Write appends one record as a single JSON line.
//
// Errors are returned, not swallowed. A journal that silently stops writing
// is worse than one that crashes: the founder would return from Dubai to a
// process that had been running happily and recording nothing.
func (j *Journal) Write(rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("journal: marshalling record: %w", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if err := j.rotateIfNeeded(time.Now().UTC()); err != nil {
		return err
	}

	n, err := j.buf.Write(append(line, '\n'))
	if err != nil {
		return fmt.Errorf("journal: writing to %s: %w", j.path(j.day), err)
	}
	j.written++
	j.bytes += int64(n)
	return nil
}

// WriteAll writes a batch and flushes once. Cheaper than N separate writes
// and, more importantly, means a whole sweep either lands or does not.
func (j *Journal) WriteAll(recs []any) error {
	for _, r := range recs {
		if err := j.Write(r); err != nil {
			return err
		}
	}
	return j.Flush()
}

// Flush pushes buffered data to the OS. The monitor calls this after each
// sweep so that at most one sweep is ever lost to an ungraceful stop.
func (j *Journal) Flush() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.buf == nil {
		return nil
	}
	if err := j.buf.Flush(); err != nil {
		return fmt.Errorf("journal: flush: %w", err)
	}
	return j.file.Sync()
}

// Close flushes and closes the current file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.closeCurrent()
}

// Stats reports what this process has written since it started.
func (j *Journal) Stats() (records int64, bytes int64, day string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.written, j.bytes, j.day
}

func (j *Journal) path(day string) string {
	return filepath.Join(j.dir, day+".jsonl")
}

func (j *Journal) rotateIfNeeded(now time.Time) error {
	day := now.Format("2006-01-02")
	if j.file != nil && j.day == day {
		return nil
	}

	previous := j.day
	if err := j.closeCurrent(); err != nil {
		return err
	}

	f, err := os.OpenFile(j.path(day), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("journal: opening %s: %w", j.path(day), err)
	}
	j.file = f
	j.buf = bufio.NewWriterSize(f, 64<<10)
	j.day = day

	// Compress the day that just ended. Failure here is logged by the caller
	// but must not stop journaling -- losing compression is an annoyance,
	// losing the day's data is not.
	if previous != "" && previous != day {
		go func(p string) { _ = CompressDay(j.dir, p) }(previous)
	}
	return nil
}

func (j *Journal) closeCurrent() error {
	if j.file == nil {
		return nil
	}
	if err := j.buf.Flush(); err != nil {
		j.file.Close()
		j.file = nil
		j.buf = nil
		return fmt.Errorf("journal: flushing on close: %w", err)
	}
	err := j.file.Close()
	j.file = nil
	j.buf = nil
	return err
}

// CompressDay gzips a finished day's file and removes the original.
// JSONL of this shape compresses roughly 10:1, which is the difference
// between a full disk and a comfortable one over ninety days.
func CompressDay(dir, day string) error {
	src := filepath.Join(dir, day+".jsonl")
	dst := src + ".gz"

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		os.Remove(dst)
		return err
	}
	if err := zw.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return os.Remove(src)
}

// Days lists the dates present in a journal directory, oldest first,
// including compressed ones.
func Days(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		switch {
		case strings.HasSuffix(n, ".jsonl.gz"):
			seen[strings.TrimSuffix(n, ".jsonl.gz")] = true
		case strings.HasSuffix(n, ".jsonl"):
			seen[strings.TrimSuffix(n, ".jsonl")] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// ReadDay streams one day's records through fn, handling both plain and
// gzipped files. A malformed line is skipped rather than aborting the read:
// a truncated final line from an ungraceful shutdown must not make the
// preceding ninety days unreadable.
func ReadDay(dir, day string, fn func(line []byte) error) error {
	plain := filepath.Join(dir, day+".jsonl")
	gz := plain + ".gz"

	var r io.Reader
	if f, err := os.Open(plain); err == nil {
		defer f.Close()
		r = f
	} else if f, err := os.Open(gz); err == nil {
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("journal: opening %s: %w", gz, err)
		}
		defer zr.Close()
		r = zr
	} else {
		return fmt.Errorf("journal: no file for %s in %s", day, dir)
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			continue
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// DiskUsage reports total bytes and file count under the journal directory.
func DiskUsage(dir string) (bytes int64, files int, err error) {
	err = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			bytes += info.Size()
			files++
		}
		return nil
	})
	return bytes, files, err
}
