// Package capital enforces a hard ceiling on the money a book may commit.
//
// The problem this exists to solve: before this package, "$400" meant $400 per
// leg, per book, with no ceiling anywhere. Four books ran concurrently and the
// real committed capital was never computed, only assumed. A Ledger makes the
// ceiling explicit and makes exceeding it a refusal rather than a silent
// over-commitment.
//
// Two rules govern everything here:
//
//  1. Principal is TOTAL. A $400 book commits at most $400 across every leg of
//     every open position. Notional is not capital; see CapitalForNotional.
//  2. A hold that is never released is a leak of the entire budget. Holds are
//     persisted, and Reconcile releases any hold whose position is no longer
//     open. This is the same orphan failure that froze 22 positions, caught one
//     layer lower.
package capital

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNoCapital is the sentinel returned when a book cannot fund an allocation.
// Callers should test with errors.Is, and surface the wrapped *ShortfallError
// for the numbers.
var ErrNoCapital = errors.New("NO_CAPITAL")

// ShortfallError reports exactly how far short the book was, so a refusal in
// the log is diagnosable without re-deriving the arithmetic.
type ShortfallError struct {
	Book      string
	Want      float64
	Free      float64
	Allocated float64
	Principal float64
	Reserve   float64
}

func (e *ShortfallError) Error() string {
	return fmt.Sprintf(
		"NO_CAPITAL: book %q needs $%.2f but only $%.2f is free "+
			"(principal $%.2f, reserve $%.2f, allocated $%.2f across open positions)",
		e.Book, e.Want, e.Free, e.Principal, e.Reserve, e.Allocated)
}

func (e *ShortfallError) Unwrap() error { return ErrNoCapital }

// ErrDuplicateHold is returned when an ID that already holds capital asks for
// more. A retry after a timeout must not be able to double-allocate; the caller
// should treat this as "the first attempt succeeded", not as a failure.
var ErrDuplicateHold = errors.New("capital: hold already exists for this id")

// Structure names the shape of a position, because the shape determines how
// much capital the same notional locks up.
type Structure string

const (
	// CashAndCarry is long spot plus short perp on one venue. The spot leg is
	// owned outright, so it costs full notional; the perp leg costs margin.
	CashAndCarry Structure = "cash_and_carry"

	// CrossVenue is perp against perp across two venues. Both legs cost margin,
	// and neither venue offsets the other because they cannot see each other.
	CrossVenue Structure = "cross_venue"
)

// pmCapitalFactor is capital divided by notional for a cash-and-carry position
// held under portfolio margin with spot hedging enabled.
//
// Derivation: without portfolio margin the measured notional/capital ratio is
// 0.50, i.e. capital is 2.00x notional (spot owned outright at 1.00x, plus
// 1.00x margin against the short perp at 1x leverage). With spot hedging the
// spot holding collateralises the short, and the ratio moves to roughly 0.90,
// i.e. capital is about 1.11x notional. That 0.90 is a venue projection, not a
// VEGA measurement — it must be re-measured once task 23 is live, and this
// constant updated from the observed margin figures rather than left as is.
const pmCapitalFactor = 1.11

// CapitalForNotional returns the USD that must be locked to hold `notional` USD
// of exposure. leverage is the perp leverage; pass 1 for unlevered.
//
// This is the function that makes "$400 total" mean what it says. Sizing code
// must go through it rather than comparing notional against principal directly,
// because for every structure we trade, capital exceeds notional.
func CapitalForNotional(s Structure, notional, leverage float64, portfolioMargin bool) (float64, error) {
	if notional <= 0 {
		return 0, fmt.Errorf("capital: notional must be positive, got %.4f", notional)
	}
	if leverage < 1 {
		return 0, fmt.Errorf("capital: leverage must be at least 1, got %.4f", leverage)
	}
	switch s {
	case CashAndCarry:
		if portfolioMargin {
			return notional * pmCapitalFactor, nil
		}
		// Spot at full notional, plus margin on the short perp.
		return notional * (1 + 1/leverage), nil
	case CrossVenue:
		// Margin on both legs. Portfolio margin does not help here: the two
		// venues do not net against each other, which is the same reason
		// cross-venue captures only the difference between two funding rates
		// rather than the full rate.
		return 2 * notional / leverage, nil
	default:
		return 0, fmt.Errorf("capital: unknown structure %q", s)
	}
}

// Hold is one position's claim on a book's principal.
type Hold struct {
	ID        string    `json:"id"`
	USD       float64   `json:"usd"`
	Notional  float64   `json:"notional"`
	Structure Structure `json:"structure"`
	Symbol    string    `json:"symbol"`
	OpenedAt  time.Time `json:"opened_at"`
}

// Snapshot is a book's state at an instant, for the dashboard and for logs.
type Snapshot struct {
	Name        string    `json:"name"`
	Service     string    `json:"service"`
	Principal   float64   `json:"principal"`
	Reserve     float64   `json:"reserve"`
	Allocated   float64   `json:"allocated"`
	Free        float64   `json:"free"`
	Positions   int       `json:"positions"`
	Utilisation float64   `json:"utilisation"` // allocated / (principal - reserve)
	Holds       []Hold    `json:"holds"`
	At          time.Time `json:"at"`
}

// Ledger is one book's capital ceiling. Safe for concurrent use.
type Ledger struct {
	mu          sync.Mutex
	name        string
	service     string
	principal   float64
	reserveFrac float64
	structures  map[Structure]bool
	holds       map[string]Hold
	path        string
}

// Config describes one book on disk.
type Config struct {
	Name string `json:"name"`
	// Service is the systemd unit that owns this book, recorded so a snapshot
	// is attributable to a process without cross-referencing anything.
	Service string `json:"service"`
	// Principal is total capital in USD, across all legs of all positions.
	Principal float64 `json:"principal"`
	// ReserveFrac is the fraction of principal that is never allocatable, held
	// back for margin top-ups. In paper mode there are no margin calls, so a
	// low value is defensible; before real money this must be raised to match
	// the de-risk rules.
	ReserveFrac float64 `json:"reserve_frac"`
	// Structures the book is permitted to open. Empty means all.
	Structures []Structure `json:"structures"`
}

// NewLedger builds a book. dataPath is where holds persist; pass "" to keep the
// ledger in memory only, which is for tests, not for a running service.
func NewLedger(cfg Config, dataPath string) (*Ledger, error) {
	if cfg.Name == "" {
		return nil, errors.New("capital: book name is required")
	}
	if cfg.Principal <= 0 {
		return nil, fmt.Errorf("capital: book %q principal must be positive, got %.2f",
			cfg.Name, cfg.Principal)
	}
	if cfg.ReserveFrac < 0 || cfg.ReserveFrac >= 1 {
		return nil, fmt.Errorf("capital: book %q reserve_frac must be in [0,1), got %.4f",
			cfg.Name, cfg.ReserveFrac)
	}
	l := &Ledger{
		name:        cfg.Name,
		service:     cfg.Service,
		principal:   cfg.Principal,
		reserveFrac: cfg.ReserveFrac,
		structures:  make(map[Structure]bool, len(cfg.Structures)),
		holds:       make(map[string]Hold),
		path:        dataPath,
	}
	for _, s := range cfg.Structures {
		l.structures[s] = true
	}
	if dataPath != "" {
		if err := l.load(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *Ledger) Name() string       { return l.name }
func (l *Ledger) Service() string    { return l.service }
func (l *Ledger) Principal() float64 { return l.principal }

// Permits reports whether this book may open the given structure.
func (l *Ledger) Permits(s Structure) bool {
	if len(l.structures) == 0 {
		return true
	}
	return l.structures[s]
}

// reserveUSD and freeLocked must be called with the mutex held.
func (l *Ledger) reserveUSD() float64 { return l.principal * l.reserveFrac }

func (l *Ledger) allocatedLocked() float64 {
	var t float64
	for _, h := range l.holds {
		t += h.USD
	}
	return t
}

func (l *Ledger) freeLocked() float64 {
	f := l.principal - l.reserveUSD() - l.allocatedLocked()
	if f < 0 {
		return 0
	}
	return f
}

// Free is the USD available to allocate right now.
func (l *Ledger) Free() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.freeLocked()
}

// CanFund reports whether an allocation would succeed, without taking it. Use
// this to filter candidates cheaply; Hold is still the authority, because
// another goroutine may allocate in between.
func (l *Ledger) CanFund(usd float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return usd <= l.freeLocked()
}

// Hold locks capital against an ID. The ID must be stable for the life of the
// position, because Release and Reconcile both key on it.
//
// Returns an error wrapping ErrNoCapital when the book cannot fund the request.
// That refusal is the entire point of this package: it is better to skip a
// candidate than to open a position the book cannot actually carry.
func (l *Ledger) Hold(id string, usd, notional float64, s Structure, symbol string, now time.Time) error {
	if id == "" {
		return errors.New("capital: hold id is required")
	}
	if usd <= 0 {
		return fmt.Errorf("capital: hold must be positive, got $%.4f", usd)
	}
	if !l.Permits(s) {
		return fmt.Errorf("capital: book %q does not permit structure %q", l.name, s)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, exists := l.holds[id]; exists {
		return fmt.Errorf("%w: id %q in book %q", ErrDuplicateHold, id, l.name)
	}
	free := l.freeLocked()
	if usd > free {
		return &ShortfallError{
			Book:      l.name,
			Want:      usd,
			Free:      free,
			Allocated: l.allocatedLocked(),
			Principal: l.principal,
			Reserve:   l.reserveUSD(),
		}
	}
	l.holds[id] = Hold{
		ID: id, USD: usd, Notional: notional,
		Structure: s, Symbol: symbol, OpenedAt: now,
	}
	return l.saveLocked()
}

// Release returns capital to the book when a position closes. It is safe to
// call for an ID that holds nothing — a close is allowed to be idempotent, and
// a double release must never inflate the budget.
func (l *Ledger) Release(id string) (float64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	h, ok := l.holds[id]
	if !ok {
		return 0, false, nil
	}
	delete(l.holds, id)
	return h.USD, true, l.saveLocked()
}

// Reconcile releases every hold whose ID is not in openIDs, and returns the IDs
// it freed. Call this after loading and on every cycle.
//
// Without it, one crash between "position closed" and "capital released" leaks
// that capital until someone notices the book refusing trades it should be able
// to afford. That is precisely how 22 positions came to be frozen open at the
// layer above; there is no reason to rebuild the same failure here.
func (l *Ledger) Reconcile(openIDs []string) ([]string, error) {
	open := make(map[string]bool, len(openIDs))
	for _, id := range openIDs {
		open[id] = true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var freed []string
	for id := range l.holds {
		if !open[id] {
			freed = append(freed, id)
			delete(l.holds, id)
		}
	}
	if len(freed) == 0 {
		return nil, nil
	}
	sort.Strings(freed)
	return freed, l.saveLocked()
}

// Snapshot reports the book's state. Holds are sorted by ID so that successive
// snapshots diff cleanly.
func (l *Ledger) Snapshot(now time.Time) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	alloc := l.allocatedLocked()
	deployable := l.principal - l.reserveUSD()
	var util float64
	if deployable > 0 {
		util = alloc / deployable
	}
	hs := make([]Hold, 0, len(l.holds))
	for _, h := range l.holds {
		hs = append(hs, h)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })

	return Snapshot{
		Name:        l.name,
		Service:     l.service,
		Principal:   l.principal,
		Reserve:     l.reserveUSD(),
		Allocated:   alloc,
		Free:        l.freeLocked(),
		Positions:   len(l.holds),
		Utilisation: util,
		Holds:       hs,
		At:          now,
	}
}

// --------------------------------------------------------------- persistence

type persisted struct {
	Name      string          `json:"name"`
	Principal float64         `json:"principal"`
	Reserve   float64         `json:"reserve_frac"`
	Holds     map[string]Hold `json:"holds"`
	SavedAt   time.Time       `json:"saved_at"`
}

func (l *Ledger) load() error {
	b, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // first run
	}
	if err != nil {
		return fmt.Errorf("capital: reading %s: %w", l.path, err)
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		// Report rather than silently starting from zero. Starting from zero
		// after a corrupt read would mean the book believes it has its full
		// principal free while positions are open against it.
		return fmt.Errorf("capital: %s is corrupt (%d bytes): %w", l.path, len(b), err)
	}
	if p.Principal != l.principal {
		return fmt.Errorf("capital: book %q principal changed from $%.2f to $%.2f while "+
			"%d holds were open; resolve deliberately rather than letting the ceiling move "+
			"under live positions", l.name, p.Principal, l.principal, len(p.Holds))
	}
	if p.Holds != nil {
		l.holds = p.Holds
	}
	return nil
}

// saveLocked writes holds durably. Must be called with the mutex held.
func (l *Ledger) saveLocked() error {
	if l.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("capital: creating dir for %s: %w", l.path, err)
	}
	b, err := json.MarshalIndent(persisted{
		Name:      l.name,
		Principal: l.principal,
		Reserve:   l.reserveFrac,
		Holds:     l.holds,
		SavedAt:   time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("capital: encoding %s: %w", l.path, err)
	}

	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("capital: opening %s: %w", tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("capital: writing %s: %w", tmp, err)
	}
	// fsync before rename, so a power loss cannot leave a renamed-but-empty
	// file. Same discipline as the universe scanner.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("capital: syncing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("capital: closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("capital: renaming %s to %s: %w", tmp, l.path, err)
	}
	return nil
}

// ------------------------------------------------------------------- manager

// Manager holds every book in one process and routes by name.
type Manager struct {
	books map[string]*Ledger
	order []string
}

// ManagerConfig is the on-disk shape of config/capital.json.
type ManagerConfig struct {
	Books []Config `json:"books"`
}

// NewManager builds every configured book, persisting each under dataDir.
func NewManager(cfg ManagerConfig, dataDir string) (*Manager, error) {
	if len(cfg.Books) == 0 {
		return nil, errors.New("capital: no books configured")
	}
	m := &Manager{books: make(map[string]*Ledger, len(cfg.Books))}
	for _, bc := range cfg.Books {
		if _, dup := m.books[bc.Name]; dup {
			return nil, fmt.Errorf("capital: duplicate book name %q", bc.Name)
		}
		path := ""
		if dataDir != "" {
			path = filepath.Join(dataDir, "capital_"+bc.Name+".json")
		}
		l, err := NewLedger(bc, path)
		if err != nil {
			return nil, err
		}
		m.books[bc.Name] = l
		m.order = append(m.order, bc.Name)
	}
	return m, nil
}

// LoadManager reads config/capital.json and builds the books.
func LoadManager(configPath, dataDir string) (*Manager, error) {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("capital: reading %s: %w", configPath, err)
	}
	var cfg ManagerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("capital: parsing %s: %w", configPath, err)
	}
	return NewManager(cfg, dataDir)
}

// Book returns a ledger by name, erroring rather than returning nil so a typo
// in a service flag fails loudly at start rather than nil-panicking later.
func (m *Manager) Book(name string) (*Ledger, error) {
	l, ok := m.books[name]
	if !ok {
		return nil, fmt.Errorf("capital: no book named %q (configured: %v)", name, m.order)
	}
	return l, nil
}

// Snapshots returns every book in configured order.
func (m *Manager) Snapshots(now time.Time) []Snapshot {
	out := make([]Snapshot, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, m.books[n].Snapshot(now))
	}
	return out
}

// WriteSnapshots persists all books to one file for the dashboard to read.
func (m *Manager) WriteSnapshots(path string, now time.Time) error {
	b, err := json.MarshalIndent(m.Snapshots(now), "", "  ")
	if err != nil {
		return fmt.Errorf("capital: encoding snapshots: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("capital: writing %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}
