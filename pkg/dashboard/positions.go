package dashboard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/funding"
)

// POSITIONS, P&L AND CROSS-VENUE DISPERSION ON THE DASHBOARD
//
// Everything here is read from FILES the monitor already writes. This process
// never contacts an exchange -- that is what makes the page unable to lie about
// live state, and it is why the whole dashboard has stayed a pure reader.
//
// The money arithmetic is a deliberate copy of pkg/report/paper.go rather than
// a call into it, for one reason: the report imports excelize and the monitor
// must not. But it MUST agree with the workbook, so the same rule holds here --
//
//	funding collected  -  entry cost  -  exit cost  =  NET
//
// -- computed from the positions themselves, with funding.Stats used for counts
// and never for money. If these two ever disagree, one of them has a bug and
// the disagreement is the finding.

// LedgerView is the reconciled money summary for the paper book.
type LedgerView struct {
	Available bool
	Err       string

	NotionalUSD float64 // per leg, from the positions themselves
	CapitalUSD  float64 // both legs, across the whole book

	FundingUSD float64
	EntryUSD   float64 // negative
	ExitUSD    float64 // negative
	NetUSD     float64

	RealizedUSD   float64
	UnrealizedUSD float64
	ReturnPct     float64 // on deployed capital

	OpenCount   int
	ClosedCount int
	Profitable  int
	Losing      int

	MeanHoldDays     float64
	MeanRoundTripBps float64
	BreakEvenDays    float64
}

// PositionView is one paper position, open or closed.
type PositionView struct {
	Symbol string
	Venue  string
	IsOpen bool

	OpenedAt string
	ClosedAt string
	Reason   string
	HeldDays float64

	EntryRatePct  float64
	LastRatePct   float64
	IntervalHours float64

	FundingBps float64
	FundingUSD float64

	EntryCostBps   float64
	ExitCostBps    float64
	ExitIsEstimate bool

	NetBps    float64
	NetUSD    float64
	ReturnPct float64

	NotionalUSD float64
	CapitalUSD  float64

	Intervals     int
	Negatives     int
	BreakEvenDays float64
}

// DispersionRow is one cross-venue pair from the hourly cron log.
//
// HoursSeen is the point of this table. A pair that appears once is a print
// nobody could have acted on; one that appears for longer than its own
// break-even is a trade that could have been finished.
type DispersionRow struct {
	Coin       string
	LongVenue  string
	ShortVenue string

	SpreadBpsHr float64
	CostBps     float64
	BeHours     float64

	// HoursSeen counts DISTINCT hour buckets touched -- how many independent
	// samples exist. SpanHours is the wall clock between first and last sight.
	//
	// These are not the same number and the difference is not cosmetic. A pair
	// first seen 19:51 and last seen 21:07 touches buckets 19, 20 and 21, so
	// HoursSeen is 3 -- but only 1.27 hours elapsed. Break-even is a duration,
	// so it must be judged against the DURATION, never the sample count.
	HoursSeen int
	SpanHours float64
	FirstSeen string
	LastSeen  string

	// Cleared is true once the pair has persisted longer than it needs to
	// recover its own round trip.
	Cleared bool
}

// enrich fills the position, ledger and dispersion sections of a snapshot.
//
// Every failure is recorded rather than propagated: a missing dispersion log
// must not blank the rest of the page, and an unreadable book must say so
// rather than render as an empty one. An empty table and a broken reader look
// identical otherwise, and that is exactly the confusion this project exists
// to remove.
func (s *Server) enrich(snap *Snapshot) {
	if s.DataDir == "" {
		snap.Ledger.Err = "DataDir not configured on the dashboard server"
		return
	}

	s.loadPositions(snap)
	s.loadDispersion(snap)
	s.loadCross(snap)
	s.loadCandidates(snap)
	s.loadBorrow(snap)
	s.buildLeverage(snap)
	s.buildVenueTable(snap)
	s.buildScenarios(snap)
}

func (s *Server) loadPositions(snap *Snapshot) {
	book, err := funding.NewBook(s.DataDir, funding.DefaultPaperConfig())
	if err != nil {
		snap.Ledger.Err = fmt.Sprintf("reading positions.json: %v", err)
		return
	}
	open, closed := book.Snapshot()
	now := time.Now().UTC()

	var l LedgerView
	l.Available = true

	build := func(p funding.Position, isOpen bool) PositionView {
		// Exit cost: measured at close, estimated as symmetric with entry
		// while still open. The estimate is FLAGGED, not hidden -- a position
		// still owes an exit it has not paid.
		exit := p.ExitCostBps
		estimate := false
		if isOpen {
			exit = p.EntryCostBps
			estimate = true
		}

		net := p.FundingCollectedBps - p.EntryCostBps - exit
		v := PositionView{
			Symbol:         p.Symbol,
			Venue:          p.Venue,
			IsOpen:         isOpen,
			OpenedAt:       p.OpenedAt.Format("01-02 15:04"),
			HeldDays:       p.HeldDays(now),
			EntryRatePct:   p.EntryRatePct,
			LastRatePct:    p.LastRatePct,
			IntervalHours:  p.IntervalHours,
			FundingBps:     p.FundingCollectedBps,
			FundingUSD:     p.FundingCollectedBps / 10000 * p.NotionalUSD,
			EntryCostBps:   p.EntryCostBps,
			ExitCostBps:    exit,
			ExitIsEstimate: estimate,
			NetBps:         net,
			NetUSD:         net / 10000 * p.NotionalUSD,
			NotionalUSD:    p.NotionalUSD,
			CapitalUSD:     p.CapitalUSD,
			Intervals:      p.IntervalsCollected,
			Negatives:      p.NegativeIntervals,
		}
		if p.CapitalUSD > 0 {
			v.ReturnPct = v.NetUSD / p.CapitalUSD * 100
		}
		if p.IntervalHours > 0 && p.EntryRatePct != 0 {
			perDay := p.EntryRatePct * 100 * (24 / p.IntervalHours)
			if perDay > 0 {
				v.BreakEvenDays = (p.EntryCostBps + exit) / perDay
			}
		}
		if !isOpen {
			v.ClosedAt = p.ClosedAt.Format("01-02 15:04")
			v.Reason = p.CloseReason
		}

		// Roll into the ledger.
		l.FundingUSD += v.FundingUSD
		l.EntryUSD -= p.EntryCostBps / 10000 * p.NotionalUSD
		l.ExitUSD -= exit / 10000 * p.NotionalUSD
		l.NetUSD += v.NetUSD
		l.CapitalUSD += p.CapitalUSD
		l.MeanRoundTripBps += p.EntryCostBps + exit
		l.MeanHoldDays += v.HeldDays

		if p.NotionalUSD > l.NotionalUSD {
			l.NotionalUSD = p.NotionalUSD
		}
		if isOpen {
			l.UnrealizedUSD += v.NetUSD
			l.OpenCount++
		} else {
			l.RealizedUSD += v.NetUSD
			l.ClosedCount++
		}
		if net >= 0 {
			l.Profitable++
		} else {
			l.Losing++
		}
		return v
	}

	for _, p := range open {
		snap.OpenPositions = append(snap.OpenPositions, build(p, true))
	}
	for _, p := range closed {
		snap.ClosedPositions = append(snap.ClosedPositions, build(p, false))
	}

	total := l.OpenCount + l.ClosedCount
	if total > 0 {
		l.MeanHoldDays /= float64(total)
		l.MeanRoundTripBps /= float64(total)
	}
	if l.CapitalUSD > 0 {
		l.ReturnPct = l.NetUSD / l.CapitalUSD * 100
	}

	// Worst first: a book is exactly as good as its worst position, and a
	// table sorted best-first buries the one that needs attention.
	sort.Slice(snap.OpenPositions, func(i, j int) bool {
		return snap.OpenPositions[i].NetBps < snap.OpenPositions[j].NetBps
	})
	sort.Slice(snap.ClosedPositions, func(i, j int) bool {
		return snap.ClosedPositions[i].ClosedAt > snap.ClosedPositions[j].ClosedAt
	})

	snap.Ledger = l
}

// dispersionLine matches what cmd/dispersion writes.
type dispersionLine struct {
	TsMs        int64   `json:"ts_ms"`
	Coin        string  `json:"coin"`
	LongVenue   string  `json:"long_venue"`
	ShortVenue  string  `json:"short_venue"`
	SpreadBpsHr float64 `json:"spread_bps_hr"`
	CostBps     float64 `json:"cost_bps"`
	BeHours     float64 `json:"be_hours"`
}

// loadDispersion reads the hourly cron log and counts persistence per pair.
//
// Persistence is counted in DISTINCT HOURS, not in lines. The cron runs hourly
// but can be run by hand as well, and counting lines would let three manual
// runs in one minute look like three hours of a surviving dislocation.
func (s *Server) loadDispersion(snap *Snapshot) {
	path := filepath.Join(s.DataDir, "dispersion", "log.jsonl")

	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			snap.DispersionErr = fmt.Sprintf("reading %s: %v", path, err)
		}
		return
	}
	defer f.Close()

	type agg struct {
		row   DispersionRow
		hours map[int64]bool
		first int64
		last  int64
	}
	byPair := map[string]*agg{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var d dispersionLine
		if json.Unmarshal(line, &d) != nil || d.Coin == "" {
			continue
		}

		key := d.Coin + "|" + d.LongVenue + "|" + d.ShortVenue
		a, ok := byPair[key]
		if !ok {
			a = &agg{hours: map[int64]bool{}, first: d.TsMs, last: d.TsMs}
			byPair[key] = a
		}
		// Truncate to the hour so repeated manual runs collapse to one.
		a.hours[d.TsMs/3_600_000] = true
		if d.TsMs < a.first {
			a.first = d.TsMs
		}
		if d.TsMs >= a.last {
			a.last = d.TsMs
			// Latest reading wins for the live figures.
			a.row.Coin = d.Coin
			a.row.LongVenue = d.LongVenue
			a.row.ShortVenue = d.ShortVenue
			a.row.SpreadBpsHr = d.SpreadBpsHr
			a.row.CostBps = d.CostBps
			a.row.BeHours = d.BeHours
		}
	}

	for _, a := range byPair {
		r := a.row
		r.HoursSeen = len(a.hours)
		r.SpanHours = float64(a.last-a.first) / 3_600_000
		r.FirstSeen = time.UnixMilli(a.first).UTC().Format("01-02 15:04")
		r.LastSeen = time.UnixMilli(a.last).UTC().Format("01-02 15:04")

		// Two independent conditions, both required:
		//   - the dislocation OUTLIVED its own break-even in wall clock
		//   - it was seen in at least two separate hours, so the span is not
		//     an artefact of two readings taken moments apart
		r.Cleared = r.BeHours > 0 && r.SpanHours >= r.BeHours && r.HoursSeen >= 2
		snap.Dispersion = append(snap.Dispersion, r)
	}

	// Persistence first, then size. A small spread that has survived nine
	// hours is worth more than a huge one seen once.
	sort.Slice(snap.Dispersion, func(i, j int) bool {
		if snap.Dispersion[i].Cleared != snap.Dispersion[j].Cleared {
			return snap.Dispersion[i].Cleared
		}
		if snap.Dispersion[i].SpanHours != snap.Dispersion[j].SpanHours {
			return snap.Dispersion[i].SpanHours > snap.Dispersion[j].SpanHours
		}
		return snap.Dispersion[i].SpreadBpsHr > snap.Dispersion[j].SpreadBpsHr
	})
	if len(snap.Dispersion) > 20 {
		snap.Dispersion = snap.Dispersion[:20]
	}
	snap.DispersionPath = path
}
