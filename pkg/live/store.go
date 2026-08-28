package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DURABLE INTENT LOG.
//
// # THE PROBLEM IT SOLVES
//
// A hedge is two orders. Between them the account is directionally exposed. If
// the process dies in that window -- crash, OOM, deploy, host reboot -- the old
// code came back with no memory that a position existed. Nothing reconciled it,
// nothing closed it, and the next pass would happily open another.
//
// An external code review flagged this on 2026-08-19 alongside the partial-fill
// defects. The two compound: a partial fill leaves an unbalanced position, and
// a restart then forgets it exists.
//
// # HOW IT WORKS
//
// An intent is written BEFORE the first order is sent, and updated as the
// position advances. On startup every record is replayed; anything not cleanly
// closed is queried against the venue and either reconstructed or quarantined.
// Entries stay BLOCKED until that finishes.
//
// Append-only, one JSON object per line. The file is the source of truth about
// what was attempted; the exchange is the source of truth about what happened.
// Derived state is rebuilt from both and never trusted on its own.
type Store struct {
	path string
	mu   sync.Mutex
}

// Stage is where an intent got to. Ordering matters: anything short of
// StageClosed needs reconciling after a restart.
type Stage string

const (
	StageIntent      Stage = "intent"      // nothing sent yet
	StageSpotSent    Stage = "spot_sent"   // first leg away, outcome unknown
	StageSpotFilled  Stage = "spot_filled" // first leg done, UNHEDGED
	StageOpen        Stage = "open"        // both legs on
	StageClosing     Stage = "closing"     // unwind started
	StageClosed      Stage = "closed"      // reconciled to zero
	StageQuarantined Stage = "quarantined" // needs a human
)

// IntentRecord is one line of the log.
type IntentRecord struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Stage    Stage     `json:"stage"`
	Venue    string    `json:"venue"`
	Symbol   string    `json:"symbol"`
	Quantity float64   `json:"quantity,omitempty"`

	// Client order IDs are the ONLY handle on an order after a restart. They
	// are generated before the first attempt precisely so that an unconfirmed
	// send can still be asked about rather than retried blindly.
	SpotOrderID string `json:"spot_order_id,omitempty"`
	PerpOrderID string `json:"perp_order_id,omitempty"`

	SpotFilledQty float64 `json:"spot_filled_qty,omitempty"`
	PerpFilledQty float64 `json:"perp_filled_qty,omitempty"`

	Note string `json:"note,omitempty"`
}

// Unresolved reports whether this record needs reconciling on startup.
func (r IntentRecord) Unresolved() bool {
	return r.Stage != StageClosed && r.Stage != StageQuarantined
}

// Exposed reports whether the record describes a state where the account may
// carry directional risk. StageSpotFilled is the dangerous one: one leg on, no
// hedge.
func (r IntentRecord) Exposed() bool {
	switch r.Stage {
	case StageSpotSent, StageSpotFilled, StageOpen, StageClosing:
		return true
	}
	return false
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("live: intent store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("live: creating %s: %w", dir, err)
	}
	return &Store{path: filepath.Join(dir, "live_intents.jsonl")}, nil
}

func (s *Store) Path() string { return s.path }

// Append writes one record and FSYNCS.
//
// The sync is the point. A record buffered in the page cache when the host
// loses power is a position nobody knows about. The cost is a few milliseconds
// per order, against a lifetime of not knowing what is open.
func (s *Store) Append(r IntentRecord) error {
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("live: opening intent log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("live: writing intent: %w", err)
	}
	return f.Sync()
}

// Latest replays the log and returns the last state of each intent.
//
// Last-write-wins per ID, which is what append-only gives you for free: the
// history stays auditable while the current state is a single pass away.
func (s *Store) Latest() (map[string]IntentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return map[string]IntentRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("live: reading intent log: %w", err)
	}
	out := map[string]IntentRecord{}
	var bad int
	for _, line := range splitLines(raw) {
		if len(line) == 0 {
			continue
		}
		var r IntentRecord
		if json.Unmarshal(line, &r) != nil || r.ID == "" {
			// A torn final line is expected after an unclean shutdown. Skip it
			// rather than refusing to start -- but count it, because a nonzero
			// figure means something more than the last write was lost.
			bad++
			continue
		}
		out[r.ID] = r
	}
	if bad > 0 {
		return out, fmt.Errorf("live: %d unreadable line(s) in %s; state may be incomplete", bad, s.path)
	}
	return out, nil
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
