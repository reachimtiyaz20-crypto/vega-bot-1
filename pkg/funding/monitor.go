// Package funding runs the unattended measurement loop.
//
// It polls every registered venue, costs every observation with that venue's
// verified fees and the symbol's measured book, and journals the result. It
// places no orders and holds no credentials.
//
// The design constraint is write volume over ninety days. 806 symbols at a
// full record every minute is 440 MB/day and fills the disk in under a month.
// So the loop polls often -- because a funding settlement missed is a data
// point lost permanently -- but writes selectively. See SweepPolicy.
package funding

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/economics"
	"github.com/imtiyaz/vega-bot/pkg/exchange"
	"github.com/imtiyaz/vega-bot/pkg/journal"
)

// Gate codes are short on purpose. A full Reason string averages 140 bytes
// and would roughly double the journal. The code is enough to reconstruct
// why a symbol was refused; the arithmetic to re-derive the sentence is all
// in the record already.
const (
	GateOK          = "ok"          // cleared every filter and the cost gate
	GateNoSpot      = "nospot"      // no spot pair; position unconstructable
	GateThinPerp    = "thinperp"    // perp 24h volume below floor
	GateThinSpot    = "thinspot"    // spot 24h volume below floor
	GateShallow     = "shallow"     // top-of-book smaller than required
	GateUnmeasured  = "unmeasured"  // book unreadable; cost unknown
	GateUnverified  = "unverified"  // venue fees not verified
	GateNegative    = "negfunding"  // funding negative; the short leg pays
	GateZero        = "zerofunding" // funding exactly zero
	GateSlippage    = "slippage"    // measured execution cost above the ceiling
	GateNotCovering = "notcovering" // funding positive but below the round trip
)

// Record is one journal line. Field names are snake_case and stable: three
// months of history will be parsed by tools that do not exist yet, so
// renaming any of these later is a migration, not an edit.
type Record struct {
	Type   string `json:"type"` // "obs" or "health"
	TsMs   int64  `json:"ts_ms"`
	Venue  string `json:"venue,omitempty"`
	Symbol string `json:"symbol,omitempty"`

	FundingRatePct float64 `json:"funding_rate_pct,omitempty"`
	IntervalHours  float64 `json:"interval_hours,omitempty"`
	AnnualizedPct  float64 `json:"annualized_pct,omitempty"`

	MarkPrice  float64 `json:"mark_price,omitempty"`
	IndexPrice float64 `json:"index_price,omitempty"`
	BasisBps   float64 `json:"basis_bps,omitempty"`

	SpotAvailable     bool    `json:"spot_available"`
	LiquidityMeasured bool    `json:"liq_measured"`
	SpotHalfSpreadBps float64 `json:"spot_half_spread_bps,omitempty"`
	PerpHalfSpreadBps float64 `json:"perp_half_spread_bps,omitempty"`
	SpotTopOfBookUSD  float64 `json:"spot_top_usd,omitempty"`
	PerpTopOfBookUSD  float64 `json:"perp_top_usd,omitempty"`
	SpotVol24hUSD     float64 `json:"spot_vol_24h_usd,omitempty"`
	PerpVol24hUSD     float64 `json:"perp_vol_24h_usd,omitempty"`

	// CostBps is the FULL round trip actually applied: four fee legs plus
	// measured slippage on both books. This is the number the previous bot
	// did not have.
	CostBps       float64 `json:"cost_bps"`
	BreakevenDays float64 `json:"breakeven_days"`

	NetBps7d  float64 `json:"net_bps_7d"`
	NetBps30d float64 `json:"net_bps_30d"`
	Viable7d  bool    `json:"viable_7d"`
	Viable30d bool    `json:"viable_30d"`
	Gate7d    string  `json:"gate_7d"`
	Gate30d   string  `json:"gate_30d"`

	// --- health records only ---
	Observed     int               `json:"observed,omitempty"`
	Journaled    int               `json:"journaled,omitempty"`
	Passing      int               `json:"passing,omitempty"`
	Hedgeable    int               `json:"hedgeable,omitempty"`
	NegativeRate int               `json:"negative_rate,omitempty"`
	Gates        map[string]int    `json:"gates,omitempty"`
	PollMs       int64             `json:"poll_ms,omitempty"`
	VenueErrors  map[string]string `json:"venue_errors,omitempty"`
	JournalBytes int64             `json:"journal_bytes,omitempty"`
	Note         string            `json:"note,omitempty"`
}

// SweepPolicy decides how often to poll and how much to write.
//
// Polling and journaling are separated deliberately. Polling is cheap (a few
// hundred KB of HTTP against a 2400/min weight budget) and frequent polling
// is what catches a rate spike between settlements. Journaling is what fills
// the disk, so it is rationed.
type SweepPolicy struct {
	// PollInterval is how often venues are queried.
	PollInterval time.Duration

	// FullSweepInterval is how often EVERY hedgeable symbol is written,
	// regardless of whether anything changed. This is the backbone of the
	// dataset: a regular, unbiased sample.
	FullSweepInterval time.Duration

	// AlwaysJournalPassing writes any symbol clearing the gate on every poll,
	// not just on sweeps. Passing symbols are rare (1 of 806 on 2026-08-05)
	// and are the ones worth high resolution.
	AlwaysJournalPassing bool

	// MaterialRateChangePct writes a symbol off-sweep when its rate moves by
	// more than this in absolute percentage points. Catches spikes that a
	// 30-minute sweep would average away.
	MaterialRateChangePct float64
}

// DefaultPolicy targets roughly 7 MB/day before compression, against 12 GB
// free. Ninety days lands near 600 MB uncompressed, ~65 MB gzipped.
func DefaultPolicy() SweepPolicy {
	return SweepPolicy{
		PollInterval:          5 * time.Minute,
		FullSweepInterval:     30 * time.Minute,
		AlwaysJournalPassing:  true,
		MaterialRateChangePct: 0.005,
	}
}

// Monitor is the long-running loop.
type Monitor struct {
	Registry    *exchange.Registry
	Journal     *journal.Journal
	Constraints exchange.Constraints
	Policy      SweepPolicy
	Logger      *log.Logger

	// HoldDaysShort and HoldDaysLong are the two horizons every observation is
	// assessed against, so the journal answers "was this worth it over a week"
	// and "over a month" without re-deriving anything later.
	HoldDaysShort float64
	HoldDaysLong  float64

	// Book is the paper position ledger. Nil disables paper trading entirely,
	// which is a legitimate mode: observation-only still produces the dataset.
	Book *Book

	lastRate   map[string]float64
	lastSweep  time.Time
	consecErrs int
}

// New builds a Monitor with sane defaults.
func New(reg *exchange.Registry, j *journal.Journal, cons exchange.Constraints, logger *log.Logger) *Monitor {
	return &Monitor{
		Registry:      reg,
		Journal:       j,
		Constraints:   cons,
		Policy:        DefaultPolicy(),
		Logger:        logger,
		HoldDaysShort: 7,
		HoldDaysLong:  30,
		lastRate:      make(map[string]float64, 1024),
	}
}

// Run polls until the context is cancelled. It never returns an error for a
// transient venue failure: funding settles on a fixed clock, and a loop that
// exits on the first HTTP timeout misses every window until someone notices.
func (m *Monitor) Run(ctx context.Context) error {
	m.Logger.Printf("monitor starting: poll=%s sweep=%s holds=%.0fd/%.0fd notional=$%.0f",
		m.Policy.PollInterval, m.Policy.FullSweepInterval,
		m.HoldDaysShort, m.HoldDaysLong, m.Constraints.NotionalUSD)

	// Poll immediately so a restart produces data at once rather than after a
	// full interval of silence.
	m.pollOnce(ctx)

	t := time.NewTicker(m.Policy.PollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			m.Logger.Printf("monitor stopping: %v", ctx.Err())
			return m.Journal.Flush()
		case <-t.C:
			m.pollOnce(ctx)
		}
	}
}

func (m *Monitor) pollOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()

	start := time.Now()
	obs, venueErrs := m.Registry.CollectAll(ctx)
	pollMs := time.Since(start).Milliseconds()

	if len(obs) == 0 {
		m.consecErrs++
		m.Logger.Printf("ERROR poll returned nothing (consecutive failures: %d) errs=%v", m.consecErrs, venueErrs)
		m.writeHealth(0, 0, 0, 0, 0, nil, pollMs, venueErrs, "poll returned no observations")
		return
	}
	m.consecErrs = 0

	venues := make(map[string]exchange.Venue)
	for _, s := range m.Registry.Sources() {
		venues[s.Venue().Name] = s.Venue()
	}

	fullSweep := time.Since(m.lastSweep) >= m.Policy.FullSweepInterval
	now := time.Now().UTC()

	var (
		batch     []any
		passing   int
		hedgeable int
		negative  int
	)
	gates := map[string]int{}

	for _, o := range obs {
		v, known := venues[o.Venue]
		if !known {
			continue
		}

		short := m.Constraints
		short.HoldDays = m.HoldDaysShort
		long := m.Constraints
		long.HoldDays = m.HoldDaysLong

		aShort, errShort := exchange.Assess(v, o, short)
		aLong, errLong := exchange.Assess(v, o, long)

		g7 := gateCode(errShort, aShort)
		g30 := gateCode(errLong, aLong)
		gates[g30]++

		if o.SpotSymbolAvailable {
			hedgeable++
		}
		if o.FundingRatePct < 0 {
			negative++
		}
		if aLong.Viable || aShort.Viable {
			passing++
		}

		if !m.shouldJournal(o, aShort, aLong, fullSweep) {
			continue
		}

		batch = append(batch, m.record(now, o, aShort, aLong, g7, g30))
		m.lastRate[o.Venue+":"+o.Symbol] = o.FundingRatePct
	}

	if fullSweep {
		m.lastSweep = time.Now()
	}

	if err := m.Journal.WriteAll(batch); err != nil {
		m.Logger.Printf("ERROR journal write failed: %v", err)
	}

	m.writeHealth(len(obs), len(batch), passing, hedgeable, negative, gates, pollMs, venueErrs, "")

	// Paper positions. Every event is journaled AND logged: three months from
	// now the ledger must explain itself without anyone remembering today.
	if m.Book != nil {
		for _, e := range m.Book.Update(now, obs, venues) {
			if err := m.Journal.Write(e); err != nil {
				m.Logger.Printf("ERROR journaling paper event: %v", err)
			}
			m.Logger.Printf("PAPER %s %s net=%+.2f bps ($%+.2f on $%.0f capital) %s",
				e.Type, e.Symbol, e.NetBps, e.NetUSD, e.CapitalUSD, e.Reason)
		}
		if err := m.Journal.Flush(); err != nil {
			m.Logger.Printf("ERROR flushing after paper events: %v", err)
		}
		if err := m.Book.Save(); err != nil {
			m.Logger.Printf("ERROR saving paper book: %v", err)
		}
	}

	m.Logger.Printf("poll: %d observed, %d hedgeable, %d passing, %d journaled, %d negative, %dms, sweep=%v",
		len(obs), hedgeable, passing, len(batch), negative, pollMs, fullSweep)
}

// shouldJournal rations disk. A full sweep writes every hedgeable symbol.
// Off-sweep, only passing symbols and material rate moves are written.
//
// Symbols with no spot market are excluded from sweeps entirely: 435 of 806
// on Binance, none of them ever tradeable as cash-and-carry, and writing them
// every thirty minutes for ninety days would more than double the dataset to
// record that a thing which cannot be done still cannot be done.
func (m *Monitor) shouldJournal(o exchange.Observation, short, long economics.Assessment, fullSweep bool) bool {
	if short.Viable || long.Viable {
		return m.Policy.AlwaysJournalPassing || fullSweep
	}
	if !o.SpotSymbolAvailable {
		return false
	}
	if fullSweep {
		return true
	}
	if prev, ok := m.lastRate[o.Venue+":"+o.Symbol]; ok {
		if math.Abs(o.FundingRatePct-prev) >= m.Policy.MaterialRateChangePct {
			return true
		}
	}
	return false
}

func (m *Monitor) record(now time.Time, o exchange.Observation, short, long economics.Assessment, g7, g30 string) Record {
	return Record{
		Type:   "obs",
		TsMs:   now.UnixMilli(),
		Venue:  o.Venue,
		Symbol: o.Symbol,

		FundingRatePct: o.FundingRatePct,
		IntervalHours:  o.IntervalHours,
		AnnualizedPct:  economics.AnnualizedPct(o.FundingRatePct, o.IntervalsPerDay()),

		MarkPrice:  o.MarkPrice,
		IndexPrice: o.IndexPrice,
		BasisBps:   o.BasisBps(),

		SpotAvailable:     o.SpotSymbolAvailable,
		LiquidityMeasured: o.LiquidityMeasured,
		SpotHalfSpreadBps: o.SpotHalfSpreadBps,
		PerpHalfSpreadBps: o.PerpHalfSpreadBps,
		SpotTopOfBookUSD:  o.SpotTopOfBookUSD,
		PerpTopOfBookUSD:  o.PerpTopOfBookUSD,
		SpotVol24hUSD:     o.SpotQuoteVolume24hUSD,
		PerpVol24hUSD:     o.PerpQuoteVolume24hUSD,

		CostBps:       long.CostBps,
		BreakevenDays: capInf(long.BreakEvenDays),

		NetBps7d:  short.NetBps,
		NetBps30d: long.NetBps,
		Viable7d:  short.Viable,
		Viable30d: long.Viable,
		Gate7d:    g7,
		Gate30d:   g30,
	}
}

func (m *Monitor) writeHealth(observed, journaled, passing, hedgeable, negative int,
	gates map[string]int, pollMs int64, venueErrs map[string]error, note string) {

	rec := Record{
		Type:         "health",
		TsMs:         time.Now().UTC().UnixMilli(),
		Observed:     observed,
		Journaled:    journaled,
		Passing:      passing,
		Hedgeable:    hedgeable,
		NegativeRate: negative,
		Gates:        gates,
		PollMs:       pollMs,
		Note:         note,
	}
	if len(venueErrs) > 0 {
		rec.VenueErrors = make(map[string]string, len(venueErrs))
		for k, v := range venueErrs {
			rec.VenueErrors[k] = v.Error()
		}
	}
	if _, bytes, _ := m.Journal.Stats(); bytes > 0 {
		rec.JournalBytes = bytes
	}
	if err := m.writeAndFlush(rec); err != nil {
		m.Logger.Printf("ERROR health write failed: %v", err)
	}
}

// gateCode maps a refusal to a short stable code.
func gateCode(err error, a economics.Assessment) string {
	if err == nil {
		if a.Viable {
			return GateOK
		}
		return GateNotCovering
	}
	r := a.Reason
	switch {
	case strings.Contains(r, "no spot pair"):
		return GateNoSpot
	case strings.Contains(r, "SPOT 24h volume"):
		return GateThinSpot
	case strings.Contains(r, "perp 24h volume"):
		return GateThinPerp
	case strings.Contains(r, "measured round-trip slippage"):
		return GateSlippage
	case strings.Contains(r, "at the touch"):
		return GateShallow
	case strings.Contains(r, "book could not be read"):
		return GateUnmeasured
	case strings.Contains(r, "UNVERIFIED"):
		return GateUnverified
	case strings.Contains(r, "funding is negative"):
		return GateNegative
	case strings.Contains(r, "funding is zero"):
		return GateZero
	default:
		return GateNotCovering
	}
}

// capInf keeps +Inf out of the JSON, which cannot represent it.
func capInf(v float64) float64 {
	if math.IsInf(v, 1) || math.IsNaN(v) {
		return -1
	}
	return v
}

// EstimateDailyBytes projects journal growth so the founder can check the
// disk maths against reality rather than against my arithmetic.
func EstimateDailyBytes(hedgeable int, p SweepPolicy, bytesPerRecord int) int64 {
	sweepsPerDay := int64(24 * time.Hour / p.FullSweepInterval)
	pollsPerDay := int64(24 * time.Hour / p.PollInterval)
	sweepBytes := sweepsPerDay * int64(hedgeable) * int64(bytesPerRecord)
	healthBytes := pollsPerDay * 300
	return sweepBytes + healthBytes
}

// String renders a policy for logs.
func (p SweepPolicy) String() string {
	return fmt.Sprintf("poll=%s sweep=%s passing=%v moveThreshold=%.4f%%",
		p.PollInterval, p.FullSweepInterval, p.AlwaysJournalPassing, p.MaterialRateChangePct)
}
