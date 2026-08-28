package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/funding"
)

// THE REVERSE BOOK ON THE DASHBOARD
//
// Short spot, long perp, on its own capital in its own directory. It is a
// different trade from cash-and-carry -- it earns on NEGATIVE funding, it pays
// interest on a borrowed asset, and the asset can be recalled -- so it gets its
// own panel rather than being averaged into the headline book's numbers.
//
// The borrow column is the one to watch. It is the dominant cost of this trade
// and the reason roughly half the coins with the best negative funding cannot
// be traded at all.

// ReversePositionView is one short-spot position, rendered.
type ReversePositionView struct {
	Symbol string
	Venue  string
	IsOpen bool

	OpenedAt string
	ClosedAt string
	Reason   string
	HeldDays float64

	// RatePct is the venue's quoted rate. NEGATIVE is what this book wants:
	// shorts are being paid.
	RatePct       float64
	IntervalHours float64

	// NetCarryBps is funding already net of borrow. The two are shown apart
	// because a position earning 40 and paying 35 is not the same position as
	// one earning 6 and paying 1, even though both net 5.
	NetCarryBps   float64
	BorrowPaidBps float64
	BorrowBpsHr   float64

	EntryCostBps float64
	ExitCostBps  float64
	ExitEstimate bool

	NetBps    float64
	NetUSD    float64
	ReturnPct float64

	NotionalUSD float64
	CapitalUSD  float64
	Intervals   int
	Adverse     int
}

// ReverseView is the whole reverse book.
type ReverseView struct {
	// Three distinct states, kept distinct because collapsing them is how a
	// dead book gets rendered as a live empty one:
	//
	//   !Configured           the dashboard was never pointed at a reverse book
	//   Configured, !Started  pointed at it, but nothing has been written there
	//   Configured, Started   real numbers below
	Configured bool
	Started    bool
	Dir        string
	Err        string

	OpenCount   int
	ClosedCount int

	// Open and closed positions are counted separately, and only closed ones
	// are ever called won or lost.
	//
	// "Won" and "lost" describe realised outcomes. A position pays its entire
	// round trip the moment it opens and earns it back over hours, so an open
	// position is underwater by construction until it crosses break-even --
	// reporting that as a LOSS makes every healthy new entry look like a
	// failure, and makes a book of four young positions read 1 won / 3 lost
	// when nothing has closed at all.
	InProfit   int // open, currently above break-even
	Underwater int // open, not there yet -- expected, not failure
	Won        int // closed, net positive
	Lost       int // closed, net negative

	FundingUSD float64 // already net of borrow
	BorrowUSD  float64
	CostUSD    float64
	NetUSD     float64
	CapitalUSD float64
	ReturnPct  float64

	MeanHoldDays float64

	Open   []ReversePositionView
	Closed []ReversePositionView
}

// loadReverse reads the reverse book from its own directory.
//
// Read-only, and it never writes: the same rule the rest of the dashboard
// follows. A missing directory leaves Configured false so the page can say "not
// running" rather than "running and empty", which are different facts.
func (s *Server) loadReverse() ReverseView {
	var v ReverseView
	if s.ReverseDataDir == "" {
		return v
	}
	v.Configured = true
	v.Dir = s.ReverseDataDir

	// STAT BEFORE OPENING. funding.NewBook creates the directory and will
	// happily write an empty positions.json where none exists -- which on a
	// running reverse service is a clobber, and on a stopped one invents a book
	// that never existed. The dashboard reads. It does not create.
	pos := filepath.Join(s.ReverseDataDir, "positions.json")
	if _, err := os.Stat(pos); err != nil {
		if !os.IsNotExist(err) {
			v.Err = fmt.Sprintf("stat %s: %v", pos, err)
		}
		return v // Configured, not Started
	}
	v.Started = true

	book, err := funding.NewBook(s.ReverseDataDir, funding.DefaultPaperConfig())
	if err != nil {
		v.Err = fmt.Sprintf("reading %s/positions.json: %v", s.ReverseDataDir, err)
		return v
	}
	open, closed := book.Snapshot()
	now := time.Now().UTC()

	build := func(p funding.Position, isOpen bool) ReversePositionView {
		exit := p.ExitCostBps
		estimate := false
		if isOpen {
			// Not yet paid, and deliberately not hidden: a position is not
			// profitable until it has covered the cost of getting out.
			exit = p.EntryCostBps
			estimate = true
		}
		net := p.FundingCollectedBps - p.EntryCostBps - exit

		r := ReversePositionView{
			Symbol: p.Symbol, Venue: p.Venue, IsOpen: isOpen,
			OpenedAt:      p.OpenedAt.Format("01-02 15:04"),
			HeldDays:      p.HeldDays(now),
			RatePct:       p.LastRatePct,
			IntervalHours: p.IntervalHours,
			NetCarryBps:   p.FundingCollectedBps,
			BorrowPaidBps: p.BorrowPaidBps,
			BorrowBpsHr:   p.BorrowBpsHr,
			EntryCostBps:  p.EntryCostBps,
			ExitCostBps:   exit,
			ExitEstimate:  estimate,
			NetBps:        net,
			NetUSD:        net / 10000 * p.NotionalUSD,
			NotionalUSD:   p.NotionalUSD,
			CapitalUSD:    p.CapitalUSD,
			Intervals:     p.IntervalsCollected,
			Adverse:       p.AdverseSettlements,
		}
		if p.CapitalUSD > 0 {
			r.ReturnPct = r.NetUSD / p.CapitalUSD * 100
		}
		if !isOpen {
			r.ClosedAt = p.ClosedAt.Format("01-02 15:04")
			r.Reason = p.CloseReason
		}

		// Roll into the book totals.
		v.FundingUSD += p.FundingCollectedBps / 10000 * p.NotionalUSD
		v.BorrowUSD += p.BorrowPaidBps / 10000 * p.NotionalUSD
		v.CostUSD -= (p.EntryCostBps + exit) / 10000 * p.NotionalUSD
		v.NetUSD += r.NetUSD
		v.CapitalUSD += p.CapitalUSD
		v.MeanHoldDays += r.HeldDays
		if isOpen {
			if net >= 0 {
				v.InProfit++
			} else {
				v.Underwater++
			}
		} else if net >= 0 {
			v.Won++
		} else {
			v.Lost++
		}
		return r
	}

	for _, p := range open {
		v.Open = append(v.Open, build(p, true))
		v.OpenCount++
	}
	for _, p := range closed {
		v.Closed = append(v.Closed, build(p, false))
		v.ClosedCount++
	}

	if n := v.OpenCount + v.ClosedCount; n > 0 {
		v.MeanHoldDays /= float64(n)
	}
	if v.CapitalUSD > 0 {
		v.ReturnPct = v.NetUSD / v.CapitalUSD * 100
	}

	// Worst first. A book is exactly as good as its worst position, and a
	// table sorted best-first buries the one that needs attention.
	sort.Slice(v.Open, func(i, j int) bool { return v.Open[i].NetBps < v.Open[j].NetBps })
	sort.Slice(v.Closed, func(i, j int) bool { return v.Closed[i].ClosedAt > v.Closed[j].ClosedAt })
	return v
}
