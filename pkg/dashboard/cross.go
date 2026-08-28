package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/crossvenue"
)

// CROSS-VENUE POSITIONS ON THE DASHBOARD
//
// Read from cross_positions.json, which cmd/cross owns. This process never
// contacts an exchange and never writes -- the same rule the rest of the
// dashboard follows, and the reason the page cannot lie about live state.
//
// The basis columns are the ones to watch. A cross-venue position's price P&L
// is entirely (basis now - basis at entry), and it is shown SEPARATELY from
// funding P&L rather than summed into it. Funding is settled and banked; basis
// is unrealised and reverses. One number combining them is how the four
// previous bots reported profits they did not have.

// CrossPositionView is one perp-perp position, rendered.
type CrossPositionView struct {
	Pair   string
	Coin   string
	IsOpen bool

	OpenedAt string
	ClosedAt string
	Reason   string
	HeldHrs  float64

	NotionalUSD float64
	CapitalUSD  float64

	EntrySpreadBpsHr float64
	SpreadBpsHr      float64

	FundingBps   float64
	LongLegBps   float64
	ShortLegBps  float64
	LongSettles  int
	ShortSettles int

	EntryCostBps float64
	ExitCostBps  float64
	ExitMeasured bool
	RoundTripBps float64

	NetBps    float64
	NetUSD    float64
	ReturnPct float64

	BreakEvenHrs float64
	BreakEvenOk  bool
	PastBE       bool

	EntryBasisBps float64
	BasisBps      float64
	BasisDriftBps float64
	BasisOk       bool

	ExitWatch  string
	NegSettles int
}

// CrossLedgerView is the money summary for the cross-venue book.
type CrossLedgerView struct {
	Available bool
	Err       string

	OpenCount   int
	ClosedCount int

	NotionalUSD   float64
	CapitalUSD    float64
	FundingUSD    float64
	CostUSD       float64
	NetUSD        float64
	RealizedUSD   float64
	UnrealizedUSD float64

	// BasisUSD is price P&L on CLOSED positions only. While a position is open
	// basis is unrealised and stays out of NET; once it closes it is banked and
	// belongs in the total. Reporting funding alone showed this book at -$4.93
	// on 2026-08-21 when it had actually lost $20.20.
	BasisUSD  float64
	ReturnPct float64
}

// crossStaleAfter is how old cross_positions.json may be before the page
// refuses to render it. cmd/cross rewrites it every poll, so anything older
// than a few polls means the writer is gone.
const crossStaleAfter = 45 * time.Minute

func (s *Server) loadCross(snap *Snapshot) {
	dir := s.CrossDataDir
	if dir == "" {
		dir = s.DataDir
	}
	// A dead file rendered as live is worse than no section at all. On
	// 2026-08-21 this showed five "open" positions held 88-224h whose writer had
	// stopped 38 hours earlier, against a live book it was not reading.
	if fi, err := os.Stat(filepath.Join(dir, "cross_positions.json")); err == nil {
		if age := time.Since(fi.ModTime()); age > crossStaleAfter {
			snap.CrossLedger.Err = fmt.Sprintf(
				"STALE -- cross_positions.json in %s was last written %s ago (%s UTC). Its writer is not running. These numbers are frozen, not live.",
				dir, age.Truncate(time.Minute), fi.ModTime().UTC().Format("2006-01-02 15:04"))
			return
		}
	}
	book, err := crossvenue.NewBook(dir, crossvenue.DefaultConfig(0))
	if err != nil {
		snap.CrossLedger.Err = fmt.Sprintf("reading cross_positions.json: %v", err)
		return
	}
	open, closed := book.Snapshot()
	now := time.Now().UTC()

	var l CrossLedgerView
	l.Available = true

	build := func(p crossvenue.Position, isOpen bool) CrossPositionView {
		be, beOk := p.BreakEvenHours()
		drift, basisOk := p.BasisDriftBps()

		v := CrossPositionView{
			Pair:             p.Pair(),
			Coin:             p.Coin,
			IsOpen:           isOpen,
			OpenedAt:         p.OpenedAt.Format("01-02 15:04"),
			HeldHrs:          p.HeldHours(now),
			NotionalUSD:      p.NotionalUSD,
			CapitalUSD:       p.CapitalUSD,
			EntrySpreadBpsHr: p.EntrySpreadBpsHr,
			SpreadBpsHr:      p.CurrentSpreadBpsHr(),
			FundingBps:       p.FundingCollectedBps(),
			LongLegBps:       p.LongLegBps,
			ShortLegBps:      p.ShortLegBps,
			LongSettles:      p.LongSettlements,
			ShortSettles:     p.ShortSettlements,
			EntryCostBps:     p.EntryCostBps,
			ExitCostBps:      p.EffectiveExitBps(),
			ExitMeasured:     p.ExitWatch.Measured || p.ExitCostMeasured,
			RoundTripBps:     p.RoundTripBps(),
			NetBps:           p.NetBps(),
			NetUSD:           p.NetUSD(),
			ReturnPct:        p.ReturnPct(),
			BreakEvenHrs:     be,
			BreakEvenOk:      beOk,
			PastBE:           p.PastBreakEven(now),
			EntryBasisBps:    p.EntryBasisBps,
			BasisBps:         p.LastBasisBps,
			BasisDriftBps:    drift,
			BasisOk:          basisOk,
			ExitWatch:        p.ExitWatch.String(),
			NegSettles:       p.NegativeSettlements,
		}
		if !isOpen {
			v.ClosedAt = p.ClosedAt.Format("01-02 15:04")
			v.Reason = p.CloseReason
		}

		l.FundingUSD += p.FundingCollectedBps() / 10000 * p.NotionalUSD
		l.CostUSD -= p.RoundTripBps() / 10000 * p.NotionalUSD
		if isOpen {
			l.NetUSD += v.NetUSD
		} else {
			l.NetUSD += p.AllInNetUSD()
			if d, ok := p.BasisDriftBps(); ok {
				l.BasisUSD += d / 10000 * p.NotionalUSD
			}
		}
		l.CapitalUSD += p.CapitalUSD
		if p.NotionalUSD > l.NotionalUSD {
			l.NotionalUSD = p.NotionalUSD
		}
		if isOpen {
			l.UnrealizedUSD += v.NetUSD
			l.OpenCount++
		} else {
			l.RealizedUSD += p.AllInNetUSD()
			l.ClosedCount++
		}
		return v
	}

	for _, p := range open {
		snap.CrossOpen = append(snap.CrossOpen, build(p, true))
	}
	for _, p := range closed {
		snap.CrossClosed = append(snap.CrossClosed, build(p, false))
	}
	if l.CapitalUSD > 0 {
		l.ReturnPct = l.NetUSD / l.CapitalUSD * 100
	}
	snap.CrossLedger = l
}
