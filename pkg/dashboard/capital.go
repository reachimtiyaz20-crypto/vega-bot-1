package dashboard

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/imtiyaz/vega-bot/pkg/capital"
)

// CAPITAL BUDGETS ON THE DASHBOARD
//
// Read-only, like everything else the dashboard does.
//
// TWO SOURCES, DELIBERATELY. The config says which books SHOULD exist and what
// their ceilings are. The ledger files say what is ACTUALLY being enforced. The
// page shows both, because the gap between them is the thing worth noticing:
//
//	configured, ledger present  -- normal, numbers are live
//	configured, no ledger yet   -- the book has never opened a position
//	ledger with no config entry -- something is enforcing a ceiling nobody
//	                               declared, or the config was edited under a
//	                               running service
//
// The first version of this scanned for ledger files only. A book that had
// never taken a hold produced no row at all, so a configured book sat silently
// invisible on the page -- which is the failure this file's comments claimed to
// be avoiding.

// capitalConfigPath is where the book definitions live.
//
// Relative on purpose: every vega unit sets WorkingDirectory=/root/vega-bot, so
// this resolves the same way for the services that WRITE the ledgers and the
// dashboard that reads them. If it is ever absent the page falls back to
// showing whatever ledgers exist, rather than showing nothing.
const capitalConfigPath = "config/capital.json"

// BudgetView is one book's capital budget, rendered.
type BudgetView struct {
	Name    string
	Service string
	Active  bool

	// Note explains a non-Active row rather than leaving it blank. A book with
	// no ledger file, a book with a corrupt one, and a book sitting at zero are
	// three different situations and must not look alike.
	Note string

	Principal float64
	Reserve   float64
	Allocated float64
	Free      float64
	Positions int

	// UsedPct is a PERCENTAGE, 0 to 100, not the 0-to-1 fraction the ledger
	// carries. The template has no arithmetic, so the scaling happens here
	// where it can be seen, rather than as a trick in the markup.
	UsedPct float64

	LastChange string
}

// budgets reconciles the configured books against the ledgers on disk.
func (s *Server) budgets() []BudgetView {
	dirs := []string{s.DataDir}
	if s.CrossDataDir != "" && s.CrossDataDir != s.DataDir {
		dirs = append(dirs, s.CrossDataDir)
	}
	// The reverse book keeps its ledger in its own directory. Leaving it out is
	// what made the reverse budget row read "no position opened yet"
	// unconditionally -- it found the book in config/capital.json and then looked
	// for its ledger in two directories that could never contain it.
	if s.ReverseDataDir != "" && s.ReverseDataDir != s.DataDir &&
		s.ReverseDataDir != s.CrossDataDir {
		dirs = append(dirs, s.ReverseDataDir)
	}

	// Ledger files first, indexed by book name.
	found := map[string]string{} // name -> path
	var scanErrs []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		paths, err := capital.FindLedgers(dir)
		if err != nil {
			scanErrs = append(scanErrs, err.Error())
			continue
		}
		for _, p := range paths {
			name := strings.TrimSuffix(
				strings.TrimPrefix(filepath.Base(p), "capital_"), ".json")
			if _, dup := found[name]; !dup {
				found[name] = p
			}
		}
	}

	books, cfgErr := capital.ReadConfig(capitalConfigPath)

	var out []BudgetView
	claimed := map[string]bool{}

	for _, b := range books {
		claimed[b.Name] = true
		bv := BudgetView{Name: b.Name, Service: b.Service}

		path, ok := found[b.Name]
		if !ok {
			// A ledger writes on its first hold, not at startup.
			bv.Principal = b.Principal
			bv.Reserve = b.Principal * b.ReserveFrac
			bv.Free = b.Principal - bv.Reserve
			bv.Note = "no position opened yet"
			out = append(out, bv)
			continue
		}
		out = append(out, mergeLedger(bv, path, b.Principal))
	}

	// Ledgers with no config entry. Something is enforcing a ceiling that is
	// not declared anywhere, which is worth saying out loud.
	for name, path := range found {
		if claimed[name] {
			continue
		}
		bv := mergeLedger(BudgetView{Name: name}, path, 0)
		if bv.Note == "" {
			bv.Note = "NOT IN " + capitalConfigPath
		}
		out = append(out, bv)
	}

	if cfgErr != nil {
		out = append(out, BudgetView{
			Name: "(config)", Note: cfgErr.Error(),
		})
	}
	for _, e := range scanErrs {
		out = append(out, BudgetView{Name: "(scan)", Note: e})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mergeLedger fills bv from the ledger at path.
//
// wantPrincipal is the configured ceiling, or 0 when there is no config entry.
// A mismatch means the config was changed while a book was running, so the
// ceiling on the page and the ceiling being enforced are different numbers --
// and the enforced one is the ledger's.
func mergeLedger(bv BudgetView, path string, wantPrincipal float64) BudgetView {
	snap, ok, err := capital.ReadSnapshot(path)
	switch {
	case err != nil:
		bv.Note = err.Error()
		return bv
	case !ok:
		bv.Note = "no position opened yet"
		return bv
	}

	bv.Active = true
	bv.Principal = snap.Principal
	bv.Reserve = snap.Reserve
	bv.Allocated = snap.Allocated
	bv.Free = snap.Free
	bv.Positions = snap.Positions
	bv.UsedPct = snap.Utilisation * 100
	if snap.Name != "" {
		bv.Name = snap.Name
	}
	if !snap.At.IsZero() {
		bv.LastChange = snap.At.Format("01-02 15:04")
	}

	switch {
	case wantPrincipal > 0 && snap.Principal != wantPrincipal:
		bv.Note = fmt.Sprintf("config says $%.0f, book is enforcing $%.0f",
			wantPrincipal, snap.Principal)
	case bv.Free <= 0 && bv.Principal > 0:
		// A book at its ceiling is the state worth seeing at a glance: it means
		// candidates are being refused for want of money rather than edge.
		bv.Note = "FULL -- refusing further entries"
	}
	return bv
}
