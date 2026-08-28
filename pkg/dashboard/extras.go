package dashboard

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// EXTRA DASHBOARD DATA
//
// Three things the page could not show before:
//
//	CANDIDATES  the live cross-venue candidate list. It existed only in
//	            `vega cross log`, so the fourth venue was invisible on the page
//	            even though it had merged 140 coins.
//	BORROW      what it costs to borrow, per venue. Leverage multiplies
//	            (funding - borrow), and the page only ever showed funding.
//	LEVERAGE    what each open position would return at 1x/3x/5x/10x.
//
// EVERY FIELD CARRIES ITS UNIT IN THE NAME. FundingBps is basis points,
// AnnualPct is percent per year, HeldHours is hours. A number whose unit has
// to be guessed is how this project spent a day treating a 4-hour funding rate
// as an 8-hour one.

// CandidateRow is one live cross-venue candidate.
type CandidateRow struct {
	Coin       string
	LongVenue  string
	ShortVenue string

	LongIntervalHours  float64
	ShortIntervalHours float64
	IntervalsExplicit  bool

	SpreadBpsHr  float64
	SpreadPctDay float64 // the same number as a daily percentage
	CostBps      float64
	BreakEvenHrs float64
	NotionalUSD  float64
	BasisBps     float64
	BasisOk      bool

	Gate   string
	Viable bool
}

// BorrowRow is one venue's borrow cost for one currency.
type BorrowRow struct {
	Venue        string
	Currency     string
	AnnualPct    float64
	HourlyPct    float64
	MaxBorrowUSD float64
	Borrowable   bool
	Cheapest     bool
}

// LeverageRow is what a cash-and-carry position returns at each leverage.
//
// Cash-and-carry must OWN the spot, so scaling it means borrowing, and every
// turn of leverage pays interest: return = f*L - b*(L-1).
type LeverageRow struct {
	Symbol string
	Venue  string

	FundingPctYr float64 // on notional
	CostPctYr    float64 // amortised round trip
	NetPctYr     float64 // f

	At1x  float64
	At3x  float64
	At5x  float64
	At10x float64

	// Worse is true when leverage REDUCES the return, because the borrow
	// costs more than the edge is worth.
	Worse bool
}

// CrossLeverageRow is what a perp-perp position returns at each leverage.
//
// No borrowing: both legs are perps on margin, so capital is 2*notional/L and
// the return scales linearly with no interest drag.
type CrossLeverageRow struct {
	Pair      string
	State     string
	HeldDays  float64
	NetBps    float64
	NetUSD    float64
	AnnualPct float64 // on notional

	At1x float64
	At3x float64
	At5x float64

	TooShort bool // held under a day; annualising it would be noise
}

// VenueRow is reference data about a venue.
type VenueRow struct {
	Name         string
	Role         string
	PerpTakerBps float64
	SpotTakerBps float64
	FeesVerified bool
	FeeSource    string
	IntervalNote string
	BorrowAnnual float64
	HasBorrow    bool
	SymbolCount  int
	Note         string
}

// ScenarioRow is the WHOLE BOOK at one leverage.
//
// Per-position leverage answers "what would this trade have returned". This
// answers "what would the book have returned", which is the question that
// decides whether leverage is worth the risk at all.
//
// Two formulas, because the strategies borrow differently:
//
//	cash-and-carry   return = f*L - b*(L-1)   spot must be OWNED, so scaling
//	                                          means borrowing, and every turn
//	                                          pays interest
//	cross-venue      return = base*L          both legs are perps on margin,
//	                                          nothing to borrow, no drag
type ScenarioRow struct {
	Leverage    float64
	NotionalUSD float64
	CapitalUSD  float64
	BorrowedUSD float64
	FundingUSD  float64
	BorrowUSD   float64
	NetUSD      float64
	ReturnPct   float64
	AnnualPct   float64
	Worse       bool
}

// buildScenarios models the whole book at each leverage.
func (s *Server) buildScenarios(snap *Snapshot) {
	levs := []float64{1, 2, 3, 5, 10}
	b := snap.BorrowUSDTPct
	if b < 0 {
		b = 0
	}

	// --- cash-and-carry ---
	var notional, netUSD, fundUSD, heldWeighted float64
	for _, p := range snap.OpenPositions {
		notional += p.NotionalUSD
		netUSD += p.NetUSD
		fundUSD += p.FundingUSD
		heldWeighted += p.HeldDays * p.NotionalUSD
	}
	for _, p := range snap.ClosedPositions {
		notional += p.NotionalUSD
		netUSD += p.NetUSD
		fundUSD += p.FundingUSD
		heldWeighted += p.HeldDays * p.NotionalUSD
	}
	if notional > 0 && heldWeighted > 0 {
		avgHeld := heldWeighted / notional
		// f: net return on notional, annualised.
		f := netUSD / notional / avgHeld * 365 * 100
		base := snap.Ledger.CapitalUSD

		for _, L := range levs {
			// At leverage L the same capital carries L times the notional.
			n := base * L
			borrowed := n - base
			if borrowed < 0 {
				borrowed = 0
			}
			ret := f*L - b*(L-1)
			row := ScenarioRow{
				Leverage:    L,
				NotionalUSD: n,
				CapitalUSD:  base,
				BorrowedUSD: borrowed,
				AnnualPct:   ret,
				// Over the period actually held, not a year.
				NetUSD:    ret / 100 * base * (avgHeld / 365),
				ReturnPct: ret * (avgHeld / 365),
			}
			row.FundingUSD = f * L / 100 * base * (avgHeld / 365)
			row.BorrowUSD = -b * (L - 1) / 100 * base * (avgHeld / 365)
			row.Worse = L > 1 && ret < f
			snap.CashScenario = append(snap.CashScenario, row)
		}
	}

	// --- cross-venue: no borrowing at all ---
	var xNet, xCap, xHeldW, xNotional float64
	addX := func(v CrossPositionView) {
		xNet += v.NetUSD
		xCap += v.CapitalUSD
		xNotional += v.NotionalUSD
		xHeldW += (v.HeldHrs / 24) * v.NotionalUSD
	}
	for _, v := range snap.CrossOpen {
		addX(v)
	}
	for _, v := range snap.CrossClosed {
		addX(v)
	}
	if xCap > 0 && xNotional > 0 && xHeldW > 0 {
		avgHeld := xHeldW / xNotional
		baseRet := xNet / xCap * 100 // over the period held
		for _, L := range levs {
			snap.CrossScenario = append(snap.CrossScenario, ScenarioRow{
				Leverage:    L,
				NotionalUSD: xNotional,
				CapitalUSD:  xCap / L,
				BorrowedUSD: 0,
				FundingUSD:  0,
				BorrowUSD:   0,
				NetUSD:      xNet,
				ReturnPct:   baseRet * L,
				AnnualPct:   baseRet * L / avgHeld * 365,
			})
		}
	}
}

// --- candidates ---------------------------------------------------------------

type passLine struct {
	TsMs                  int64   `json:"ts_ms"`
	Coin                  string  `json:"coin"`
	LongVenue             string  `json:"long_venue"`
	ShortVenue            string  `json:"short_venue"`
	SpreadBpsHr           float64 `json:"spread_bps_hr"`
	RoundTripBps          float64 `json:"round_trip_bps"`
	BeHours               float64 `json:"be_hours"`
	NotionalUSD           float64 `json:"notional_usd"`
	BasisBps              float64 `json:"basis_bps"`
	BasisMeasured         bool    `json:"basis_measured"`
	LongIntervalHours     float64 `json:"long_interval_hours"`
	ShortIntervalHours    float64 `json:"short_interval_hours"`
	LongIntervalExplicit  bool    `json:"long_interval_explicit"`
	ShortIntervalExplicit bool    `json:"short_interval_explicit"`
	Gate                  string  `json:"gate"`
	Viable                bool    `json:"viable"`
}

// loadCandidates reads the LATEST pass only.
//
// Earlier passes are history, not state. Showing all of them would mix a
// candidate that qualified two days ago with one that qualifies now.
func (s *Server) loadCandidates(snap *Snapshot) {
	path := filepath.Join(s.DataDir, "crossvenue", "passes.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var lines []passLine
	var latest int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var p passLine
		if json.Unmarshal(sc.Bytes(), &p) != nil || p.Coin == "" {
			continue
		}
		if p.TsMs > latest {
			latest = p.TsMs
			lines = lines[:0]
		}
		if p.TsMs == latest {
			lines = append(lines, p)
		}
	}
	if latest == 0 {
		return
	}

	for _, p := range lines {
		snap.Candidates = append(snap.Candidates, CandidateRow{
			Coin:               p.Coin,
			LongVenue:          p.LongVenue,
			ShortVenue:         p.ShortVenue,
			LongIntervalHours:  p.LongIntervalHours,
			ShortIntervalHours: p.ShortIntervalHours,
			IntervalsExplicit:  p.LongIntervalExplicit && p.ShortIntervalExplicit,
			SpreadBpsHr:        p.SpreadBpsHr,
			SpreadPctDay:       p.SpreadBpsHr * 24 / 100,
			CostBps:            p.RoundTripBps,
			BreakEvenHrs:       p.BeHours,
			NotionalUSD:        p.NotionalUSD,
			BasisBps:           p.BasisBps,
			BasisOk:            p.BasisMeasured,
			Gate:               p.Gate,
			Viable:             p.Viable,
		})
	}
	sort.Slice(snap.Candidates, func(i, j int) bool {
		if snap.Candidates[i].Viable != snap.Candidates[j].Viable {
			return snap.Candidates[i].Viable
		}
		return snap.Candidates[i].SpreadBpsHr > snap.Candidates[j].SpreadBpsHr
	})
	snap.CandidatesAt = time.UnixMilli(latest).UTC()
	if len(snap.Candidates) > 25 {
		snap.Candidates = snap.Candidates[:25]
	}
}

// --- borrow -------------------------------------------------------------------

type borrowSnapshot struct {
	At    time.Time `json:"at"`
	Rates []struct {
		Venue        string  `json:"venue"`
		Currency     string  `json:"currency"`
		AnnualPct    float64 `json:"annual_pct"`
		HourlyPct    float64 `json:"hourly_pct"`
		MaxBorrowUSD float64 `json:"max_borrow_usd"`
		Borrowable   bool    `json:"borrowable"`
		Ok           bool    `json:"ok"`
	} `json:"rates"`
}

// loadBorrow reads the most recent borrow snapshot.
func (s *Server) loadBorrow(snap *Snapshot) {
	path := filepath.Join(s.DataDir, "borrow", "rates.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var last borrowSnapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var b borrowSnapshot
		if json.Unmarshal(sc.Bytes(), &b) == nil && len(b.Rates) > 0 {
			last = b
		}
	}
	if len(last.Rates) == 0 {
		return
	}

	cheapestUSDT := -1.0
	for _, r := range last.Rates {
		if r.Ok && r.Currency == "USDT" && r.Borrowable {
			if cheapestUSDT < 0 || r.AnnualPct < cheapestUSDT {
				cheapestUSDT = r.AnnualPct
			}
		}
	}
	for _, r := range last.Rates {
		if !r.Ok {
			continue
		}
		snap.Borrow = append(snap.Borrow, BorrowRow{
			Venue: r.Venue, Currency: r.Currency,
			AnnualPct: r.AnnualPct, HourlyPct: r.HourlyPct,
			MaxBorrowUSD: r.MaxBorrowUSD, Borrowable: r.Borrowable,
			Cheapest: r.Currency == "USDT" && r.AnnualPct == cheapestUSDT,
		})
	}
	sort.Slice(snap.Borrow, func(i, j int) bool {
		if snap.Borrow[i].Currency != snap.Borrow[j].Currency {
			return snap.Borrow[i].Currency < snap.Borrow[j].Currency
		}
		return snap.Borrow[i].AnnualPct < snap.Borrow[j].AnnualPct
	})
	snap.BorrowAt = last.At
	snap.BorrowUSDTPct = cheapestUSDT
}

// --- leverage -----------------------------------------------------------------

// buildLeverage computes what each position returns at each leverage.
//
// Two different formulas, and the difference is the point:
//
//	cash-and-carry   return = f*L - b*(L-1)   borrowed spot, interest drag
//	cross-venue      return = base*L          both legs on margin, no interest
func (s *Server) buildLeverage(snap *Snapshot) {
	now := time.Now().UTC()
	b := snap.BorrowUSDTPct
	if b < 0 {
		b = 0
	}

	for _, p := range snap.OpenPositions {
		if p.HeldDays <= 0 || p.NotionalUSD <= 0 {
			continue
		}
		fundYr := p.FundingBps / p.HeldDays * 365 / 100
		hold := 30.0
		costYr := (p.EntryCostBps + p.ExitCostBps) * (365 / hold) / 100
		f := fundYr - costYr

		row := LeverageRow{
			Symbol: p.Symbol, Venue: p.Venue,
			FundingPctYr: fundYr, CostPctYr: costYr, NetPctYr: f,
		}
		lev := func(L float64) float64 { return f*L - b*(L-1) }
		row.At1x, row.At3x, row.At5x, row.At10x = lev(1), lev(3), lev(5), lev(10)
		row.Worse = row.At3x < row.At1x
		snap.Leverage = append(snap.Leverage, row)
	}
	sort.Slice(snap.Leverage, func(i, j int) bool {
		return snap.Leverage[i].NetPctYr > snap.Leverage[j].NetPctYr
	})

	add := func(v CrossPositionView, state string) {
		days := v.HeldHrs / 24
		if days <= 0 {
			return
		}
		r := CrossLeverageRow{
			Pair: v.Pair, State: state, HeldDays: days,
			NetBps: v.NetBps, NetUSD: v.NetUSD,
		}
		if days < 1 {
			// Annualising a sub-day hold divides a paid entry cost by a small
			// number and produces four-digit percentages that describe
			// division, not performance.
			r.TooShort = true
		} else {
			r.AnnualPct = v.NetBps / days * 365 / 100
			base := r.AnnualPct / 2 // capital is 2x notional at 1x
			r.At1x, r.At3x, r.At5x = base, base*3, base*5
		}
		snap.CrossLeverage = append(snap.CrossLeverage, r)
	}
	for _, v := range snap.CrossOpen {
		add(v, "open")
	}
	for _, v := range snap.CrossClosed {
		add(v, "closed")
	}
	_ = now
}

// --- venues -------------------------------------------------------------------

// buildVenueTable assembles reference data for every venue in play.
//
// Fees are asserted by a person, not fetched: no venue publishes a taker fee
// on an unauthenticated endpoint, so each carries the date someone read it.
func (s *Server) buildVenueTable(snap *Snapshot) {
	borrowFor := func(v string) (float64, bool) {
		for _, r := range snap.Borrow {
			if r.Venue == v && r.Currency == "USDT" && r.Borrowable {
				return r.AnnualPct, true
			}
		}
		return 0, false
	}

	defs := []VenueRow{
		{Name: "binance", Role: "spot + perp", PerpTakerBps: 5.0, SpotTakerBps: 10.0,
			FeesVerified: true, FeeSource: "binance.com/en/fee/schedule, read 2026-08-05",
			IntervalNote: "per SYMBOL, 4h or 8h", Note: "portfolio margin needs 50,000 USDT"},
		{Name: "bybit", Role: "spot + perp", PerpTakerBps: 5.5, SpotTakerBps: 10.0,
			FeesVerified: true, FeeSource: "bybit.com Trading-Fee-Structure, read 2026-08-05",
			IntervalNote: "per SYMBOL, from instruments-info", Note: "unified account, no minimum"},
		{Name: "hyperliquid", Role: "perp only (DEX)", PerpTakerBps: 4.5,
			FeesVerified: true, FeeSource: "hyperliquid.gitbook.io fees, read 2026-08-11",
			IntervalNote: "1h, venue-wide", Note: "collateral held in a contract, no recourse"},
		{Name: "lighter", Role: "perp only (DEX)", PerpTakerBps: 0.0,
			FeesVerified: true,
			FeeSource:    "API: all 210 markets report taker_fee 0.0000, read 2026-08-16",
			IntervalNote: "quoted per 8h, SETTLED every 1h",
			Note:         "$10 minimum order; zero fee is not zero cost, bid-ask runs 5-17 bps"},
		{Name: "okx", Role: "spot + perp", PerpTakerBps: 25.0, SpotTakerBps: 60.0,
			FeesVerified: true, FeeSource: "okx.com/en-ae/fees, read 2026-08-15 (UAE regular tier)",
			IntervalNote: "per SYMBOL, from settlement gap", Note: "cheapest USDT borrow"},
	}
	for i := range defs {
		if r, ok := borrowFor(defs[i].Name); ok {
			defs[i].BorrowAnnual, defs[i].HasBorrow = r, true
		}
	}
	snap.VenueTable = defs
}
