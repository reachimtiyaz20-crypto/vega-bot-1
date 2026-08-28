// Package spot answers one question per coin per venue: can this be bought on
// the spot market, and can it be borrowed?
//
// # WHY IT EXISTS
//
// Single-venue cash-and-carry -- long spot, short perp on the same venue --
// captures the FULL funding rate. Cross-venue perp-perp captures only the
// DIFFERENCE between two rates. Measured on ONG, 141 bps/hr against 63: the
// same coin, more than double, purely from structure.
//
// VEGA chose cross-venue by default because it had no way to know whether the
// single-venue structure was available. exchange.Venue carries SpotAvailable as
// a per-VENUE boolean, which says Binance has spot markets -- true, and useless
// for deciding whether Binance has a spot market for the alt currently paying
// 30%. Most perp-listed alts do not.
//
// TWO CAPABILITIES, TWO STRUCTURES
//
//	perp + spot              -> cash-and-carry, for POSITIVE funding
//	perp + spot + borrowable -> reverse carry, for NEGATIVE funding
//
// Cash-and-carry needs no borrow: the spot leg is bought with cash. Borrow is
// what makes the mirror trade possible when funding turns negative, which on
// alts is common rather than exotic.
//
// # THE RULE THAT MATTERS MOST
//
// A failed fetch is not an absent market. If a venue's spot endpoint errors,
// this package reports UNKNOWN and never false, because false would silently
// route a tradeable coin into the worse structure -- reproducing the exact
// defect this package was written to remove, while looking like a decision.
package spot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Market is one spot market on one venue.
type Market struct {
	Venue  string `json:"venue"`
	Coin   string `json:"coin"`
	Symbol string `json:"symbol"`
	Quote  string `json:"quote"`

	// MinNotionalUSD is the venue's minimum order value where published. Zero
	// means the venue did not state one, NOT that there is no minimum.
	MinNotionalUSD float64 `json:"min_notional_usd,omitempty"`

	// Borrowable and BorrowAnnualPct are populated only where a borrow feed
	// covers the coin. Borrowable=false with BorrowChecked=false means "not
	// checked", which is different from "cannot borrow" and must not be read
	// as a refusal.
	BorrowChecked   bool    `json:"borrow_checked"`
	Borrowable      bool    `json:"borrowable,omitempty"`
	BorrowAnnualPct float64 `json:"borrow_annual_pct,omitempty"`
	MaxBorrowUSD    float64 `json:"max_borrow_usd,omitempty"`
}

// VenueResult is one venue's scan outcome, kept separately from its markets so
// that a failure is recorded rather than presented as an empty universe.
type VenueResult struct {
	Venue     string    `json:"venue"`
	OK        bool      `json:"ok"`
	Err       string    `json:"err,omitempty"`
	Count     int       `json:"count"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Table is the whole picture: every venue's spot markets, plus whether each
// venue's scan actually succeeded.
type Table struct {
	At      time.Time                    `json:"at"`
	Results map[string]VenueResult       `json:"results"`
	Markets map[string]map[string]Market `json:"markets"` // venue -> coin -> market
}

// Availability is the answer to "can I do cash-and-carry here".
type Availability int

const (
	// Unknown means the venue was not scanned or its scan failed. It is the
	// zero value deliberately: an uninitialised Table must not claim that
	// nothing has spot.
	Unknown Availability = iota
	// Absent means the venue was scanned successfully and has no spot market
	// for this coin. This is a fact, and it is safe to act on.
	Absent
	// Present means the coin can be bought on that venue's spot market.
	Present
)

func (a Availability) String() string {
	switch a {
	case Present:
		return "present"
	case Absent:
		return "absent"
	default:
		return "unknown"
	}
}

// NewTable returns an empty table ready to be filled.
func NewTable(at time.Time) *Table {
	return &Table{
		At:      at,
		Results: map[string]VenueResult{},
		Markets: map[string]map[string]Market{},
	}
}

// Put records one market.
func (t *Table) Put(m Market) {
	if t.Markets[m.Venue] == nil {
		t.Markets[m.Venue] = map[string]Market{}
	}
	t.Markets[m.Venue][strings.ToUpper(m.Coin)] = m
}

// SetResult records how a venue's scan went.
func (t *Table) SetResult(r VenueResult) {
	if t.Results == nil {
		t.Results = map[string]VenueResult{}
	}
	t.Results[r.Venue] = r
}

// Spot reports whether a coin can be bought on a venue's spot market.
//
// Returns Unknown when the venue was never scanned or its scan failed. Callers
// must treat Unknown as "do not know" -- not as a refusal and not as a licence.
func (t *Table) Spot(venue, coin string) Availability {
	if t == nil {
		return Unknown
	}
	r, scanned := t.Results[venue]
	if !scanned || !r.OK {
		return Unknown
	}
	if _, found := t.Markets[venue][strings.ToUpper(coin)]; found {
		return Present
	}
	return Absent
}

// Market returns the market record if one exists.
func (t *Table) Market(venue, coin string) (Market, bool) {
	if t == nil {
		return Market{}, false
	}
	m, ok := t.Markets[venue][strings.ToUpper(coin)]
	return m, ok
}

// VenuesWithSpot lists venues where the coin is confirmed buyable, sorted.
// Venues whose scan failed are omitted rather than guessed at either way.
func (t *Table) VenuesWithSpot(coin string) []string {
	if t == nil {
		return nil
	}
	var out []string
	for v := range t.Markets {
		if t.Spot(v, coin) == Present {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// CanCashAndCarry reports whether long-spot plus short-perp is possible on this
// venue. It is exactly Spot()==Present, named for the decision it informs so
// call sites read as intent rather than as plumbing.
func (t *Table) CanCashAndCarry(venue, coin string) bool {
	return t.Spot(venue, coin) == Present
}

// CanReverseCarry reports whether short-spot plus long-perp is possible: the
// trade for NEGATIVE funding, which needs the coin borrowable and not merely
// listed.
//
// Returns false when borrow was never checked. Unlike spot availability this
// errs closed, because entering a short-spot leg that cannot actually be
// borrowed fails at execution rather than at assessment.
func (t *Table) CanReverseCarry(venue, coin string) bool {
	m, ok := t.Market(venue, coin)
	if !ok || t.Spot(venue, coin) != Present {
		return false
	}
	return m.BorrowChecked && m.Borrowable
}

// ApplyBorrow attaches borrow data to an existing market.
//
// Returns false if the venue has no spot market for the coin: borrow without a
// spot market is not actionable, and recording it would imply a structure that
// cannot be built.
func (t *Table) ApplyBorrow(venue, coin string, borrowable bool, annualPct, maxUSD float64) bool {
	coin = strings.ToUpper(coin)
	m, ok := t.Markets[venue][coin]
	if !ok {
		return false
	}
	m.BorrowChecked = true
	m.Borrowable = borrowable
	m.BorrowAnnualPct = annualPct
	m.MaxBorrowUSD = maxUSD
	t.Markets[venue][coin] = m
	return true
}

// Coins lists every coin with a spot market anywhere, sorted.
func (t *Table) Coins() []string {
	seen := map[string]bool{}
	for _, byCoin := range t.Markets {
		for c := range byCoin {
			seen[c] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Summary is a one-line-per-venue report for logs and the dashboard.
func (t *Table) Summary() []string {
	names := make([]string, 0, len(t.Results))
	for v := range t.Results {
		names = append(names, v)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, v := range names {
		r := t.Results[v]
		if !r.OK {
			out = append(out, fmt.Sprintf("%-10s SCAN FAILED -- treated as unknown, not as absent: %s", v, r.Err))
			continue
		}
		var borrowable int
		for _, m := range t.Markets[v] {
			if m.BorrowChecked && m.Borrowable {
				borrowable++
			}
		}
		out = append(out, fmt.Sprintf("%-10s %4d spot markets, %d borrowable", v, r.Count, borrowable))
	}
	return out
}

// --------------------------------------------------------------- persistence

// Path is where the table lives inside a data directory.
func Path(dataDir string) string {
	return filepath.Join(dataDir, "spot", "markets.json")
}

// Save writes the table durably: temp file, fsync, rename.
func (t *Table) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("spot: creating dir for %s: %w", path, err)
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("spot: encoding table: %w", err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("spot: opening %s: %w", tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("spot: writing %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("spot: syncing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("spot: closing %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// Load reads a table. A missing file returns (nil, nil): no scan has run yet,
// which is a state rather than a fault, and every lookup on a nil Table
// correctly answers Unknown.
func Load(path string) (*Table, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("spot: reading %s: %w", path, err)
	}
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("spot: %s is corrupt (%d bytes): %w", path, len(b), err)
	}
	if t.Markets == nil {
		t.Markets = map[string]map[string]Market{}
	}
	if t.Results == nil {
		t.Results = map[string]VenueResult{}
	}
	return &t, nil
}

// Age reports how stale the table is. Spot listings change on the scale of
// days, so a table hours old is fine and one weeks old is not.
func (t *Table) Age(now time.Time) time.Duration {
	if t == nil || t.At.IsZero() {
		return 1 << 62
	}
	return now.Sub(t.At)
}
