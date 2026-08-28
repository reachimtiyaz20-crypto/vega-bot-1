package capital

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// READING A LEDGER WITHOUT OWNING IT
//
// The dashboard must be able to show a book's budget without being able to
// change it. Everything here is read-only and takes no locks on the writer's
// behalf: a torn read is impossible because Ledger writes through a temp file
// and renames, so a reader either sees the whole previous state or the whole
// new one.
//
// This exists so the on-disk format is known in exactly one package. A reader
// that reimplemented the arithmetic would drift from the writer, and the first
// symptom would be a dashboard confidently reporting a different free balance
// from the one the book is actually enforcing.

// ReadSnapshot loads a ledger file for display. It never writes.
//
// A missing file is not an error at this layer: a ledger writes on its first
// hold, not at startup, so "no file yet" means "no position has ever been
// opened in this book" -- which is a real state and not a fault. Callers get
// ok=false and can render that honestly rather than as zeros.
func ReadSnapshot(path string) (snap Snapshot, ok bool, err error) {
	b, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		return Snapshot{}, false, nil
	}
	if rerr != nil {
		return Snapshot{}, false, fmt.Errorf("capital: reading %s: %w", path, rerr)
	}

	var p persisted
	if uerr := json.Unmarshal(b, &p); uerr != nil {
		return Snapshot{}, false, fmt.Errorf("capital: %s is corrupt (%d bytes): %w",
			path, len(b), uerr)
	}

	holds := make([]Hold, 0, len(p.Holds))
	var allocated float64
	for _, h := range p.Holds {
		allocated += h.USD
		holds = append(holds, h)
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].ID < holds[j].ID })

	reserve := p.Principal * p.Reserve
	free := p.Principal - reserve - allocated
	if free < 0 {
		free = 0
	}
	deployable := p.Principal - reserve
	var util float64
	if deployable > 0 {
		util = allocated / deployable
	}

	return Snapshot{
		Name:        p.Name,
		Principal:   p.Principal,
		Reserve:     reserve,
		Allocated:   allocated,
		Free:        free,
		Positions:   len(p.Holds),
		Utilisation: util,
		Holds:       holds,
		At:          p.SavedAt,
	}, true, nil
}

// LedgerPath is where a book's ledger lives inside a data directory. Callers
// that build this string themselves will drift from NewManager; this is the
// one definition.
func LedgerPath(dataDir, name string) string {
	return filepath.Join(dataDir, "capital_"+name+".json")
}

// ReadConfig parses the book definitions without constructing any ledgers.
//
// Callers that only need to know WHICH books should exist must not go through
// LoadManager: that builds live Ledgers, and a reader holding a Ledger is one
// refactor away from writing through it.
//
// A missing file returns (nil, nil). Nothing is configured, which is a state,
// not a fault.
func ReadConfig(path string) ([]Config, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("capital: reading %s: %w", path, err)
	}
	var cfg ManagerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("capital: parsing %s: %w", path, err)
	}
	return cfg.Books, nil
}

// FindLedgers returns every ledger file in a directory, sorted by book name.
//
// Discovery by glob rather than by configured name on purpose: the dashboard
// should show what a book is ACTUALLY enforcing, which is whatever ledger the
// running service wrote. Reading the config instead would show what the config
// says it should be, and the gap between those two is exactly the thing worth
// noticing.
func FindLedgers(dataDir string) ([]string, error) {
	if dataDir == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, "capital_*.json"))
	if err != nil {
		return nil, fmt.Errorf("capital: scanning %s: %w", dataDir, err)
	}
	sort.Strings(matches)
	return matches, nil
}
