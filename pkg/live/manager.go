// Package live opens and closes real hedged positions.
//
// # LEG ORDER IS THE MOST IMPORTANT DECISION IN THIS FILE
//
// The two legs cannot fill atomically, so between them the position is naked.
// The choice is therefore not "avoid being naked" -- that is impossible -- but
// "which naked state do you want to be in for those few hundred milliseconds".
//
//	naked LONG SPOT   -- worst case the asset goes to zero. Bounded. No
//	                     liquidation, no margin call, no forced exit. You can
//	                     stand there holding it while you fix things.
//
//	naked SHORT PERP  -- worst case is unbounded, and long before unbounded it
//	                     is liquidated. A margin engine will close it for you,
//	                     at the worst price, at the worst moment.
//
// So VEGA is ALWAYS naked long spot, never naked short perp:
//
//	OPENING:  buy spot  -> sell perp     (between: long spot)
//	CLOSING:  buy perp  -> sell spot     (between: long spot)
//
// Note that closing is not the reverse of opening. Reversing it -- selling the
// spot first -- would leave a naked short. Getting this backwards is a subtle
// bug that only shows itself on the day something goes wrong.
package live

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/execution"
	"github.com/imtiyaz/vega-bot/pkg/journal"
	"github.com/imtiyaz/vega-bot/pkg/risk"
)

// Config is everything that decides whether an order may be sent.
type Config struct {
	// Mode must be Testnet until the venue order shapes are verified.
	Mode execution.Mode

	// Enabled is the master switch. False means every open path returns an
	// error before any request is built. Default false: a zero-value Config
	// cannot trade.
	Enabled bool

	// KillSwitchPath, if the file exists, blocks all opening. Closing is never
	// blocked -- a kill switch that traps you in a position is not a safety
	// feature. Checked before every open, so it takes effect within one cycle
	// without a restart, and survives one.
	KillSwitchPath string

	// MaxOpenPositions caps concurrent hedges.
	MaxOpenPositions int

	// NotionalUSDPerLeg is the size of ONE leg. Deployed capital is roughly
	// twice this, because both legs must be funded. The brief's headline
	// return figures are on notional; on deployed capital they are half.
	NotionalUSDPerLeg float64

	// MaxSlippageBpsPerLeg aborts a position after the first leg if that leg
	// filled worse than this. Aborting means immediately unwinding the leg
	// that filled -- expensive, but a position entered at bad prices is a
	// position that has already spent its funding income.
	MaxSlippageBpsPerLeg float64

	// ConfirmAttempts and ConfirmBackoff control how hard an UNCONFIRMED order
	// is chased before giving up and escalating to a human.
	ConfirmAttempts int
	ConfirmBackoff  time.Duration

	// Thresholds for liquidation risk.
	Risk risk.Thresholds
}

// DefaultConfig is deliberately unable to trade.
func DefaultConfig() Config {
	return Config{
		Mode:                 execution.Testnet,
		Enabled:              false,
		KillSwitchPath:       "/etc/vega/HALT",
		MaxOpenPositions:     1,
		NotionalUSDPerLeg:    50,
		MaxSlippageBpsPerLeg: 15,
		ConfirmAttempts:      5,
		ConfirmBackoff:       2 * time.Second,
		Risk:                 risk.DefaultThresholds(),
	}
}

var (
	// ErrDisabled means Config.Enabled is false.
	ErrDisabled = errors.New("live: trading is DISABLED in config")

	// ErrKillSwitch means the halt file exists.
	ErrKillSwitch = errors.New("live: KILL SWITCH engaged, refusing to open")

	// ErrManualIntervention is the worst outcome this package can produce: a
	// leg is naked AND the remedy failed. Nothing automated should run after
	// this. It is a stop, not a retry.
	ErrManualIntervention = errors.New("live: MANUAL INTERVENTION REQUIRED -- unhedged exposure could not be closed")
)

// Trader is what the manager needs from a venue: place, query, and size.
type Trader interface {
	execution.OrderPlacer
	Filters(ctx context.Context, symbol string, leg execution.Leg) (execution.SymbolFilters, error)
	LiveTradingReady() error
}

// HedgePlan is an intent to open, produced by the scanner and priced at the
// moment of the decision.
type HedgePlan struct {
	Venue  string `json:"venue"`
	Symbol string `json:"symbol"`

	// SpotRef and PerpRef are the prices observed when the decision was made.
	// They are the baseline every slippage number is measured against, so a
	// plan without them cannot be executed -- there would be no way to tell a
	// good fill from a bad one afterwards.
	SpotRef float64 `json:"spot_ref"`
	PerpRef float64 `json:"perp_ref"`

	FundingRatePct float64   `json:"funding_rate_pct"`
	CostBps        float64   `json:"cost_bps"`
	ExpectedNetBps float64   `json:"expected_net_bps"`
	DecidedAt      time.Time `json:"decided_at"`
}

// LivePosition is a real, open, two-legged position.
type LivePosition struct {
	// PerpClosedQty and SpotClosedQty accumulate across close ATTEMPTS.
	//
	// A market close can fill partially. Sizing a retry from PerpEntry.FilledQty
	// would then re-close quantity that is already gone. These record what has
	// actually been unwound so a retry closes only the remainder.
	PerpClosedQty float64 `json:"perp_closed_qty,omitempty"`
	SpotClosedQty float64 `json:"spot_closed_qty,omitempty"`
	ID            string  `json:"id"`
	Venue         string  `json:"venue"`
	Symbol        string  `json:"symbol"`

	Plan HedgePlan `json:"plan"`

	SpotEntry execution.OrderResult `json:"spot_entry"`
	PerpEntry execution.OrderResult `json:"perp_entry"`

	Quantity  float64   `json:"quantity"`
	OpenedAt  time.Time `json:"opened_at"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
	CloseNote string    `json:"close_note,omitempty"`

	SpotExit execution.OrderResult `json:"spot_exit,omitempty"`
	PerpExit execution.OrderResult `json:"perp_exit,omitempty"`

	// EntrySlippageBps is measured, per leg, and only present when a reference
	// price existed. Absent means unmeasured, not zero.
	SpotEntrySlipBps  float64 `json:"spot_entry_slip_bps"`
	PerpEntrySlipBps  float64 `json:"perp_entry_slip_bps"`
	EntrySlipMeasured bool    `json:"entry_slip_measured"`
}

// Open reports whether the position is still open.
func (p *LivePosition) Open() bool { return p.ClosedAt.IsZero() }

// event is one journaled line from this package.
type event struct {
	Type    string    `json:"type"`
	TsMs    int64     `json:"ts_ms"`
	Venue   string    `json:"venue,omitempty"`
	Symbol  string    `json:"symbol,omitempty"`
	Mode    string    `json:"mode,omitempty"`
	Message string    `json:"message,omitempty"`
	Payload any       `json:"payload,omitempty"`
	At      time.Time `json:"at"`
}

// Manager owns the live positions on one or more venues.
type Manager struct {
	cfg     Config
	traders map[string]Trader
	readers map[string]execution.AccountReader
	journal *journal.Journal
	logger  *log.Logger

	mu        sync.Mutex
	positions map[string]*LivePosition

	// halted latches. Once ErrManualIntervention has been returned, this
	// manager does not open anything again in this process, no matter what the
	// caller does. Recovery is a human clearing the position and restarting.
	halted     bool
	haltReason string

	// store is the durable intent log. Live trading without one is refused:
	// a hedge is two orders, and a process that dies between them must be able
	// to find out what it left behind.
	store     *Store
	recovered bool
}

// ErrNotRecovered is returned while unresolved intents remain unreconciled.
var ErrNotRecovered = errors.New("live: intents not reconciled; call Recover before trading")

// ErrNoIntentStore is returned when live trading is attempted without durable
// state.
var ErrNoIntentStore = errors.New("live: no intent store attached")

// AttachStore gives the manager durable state. Without it OpenHedge refuses.
func (m *Manager) AttachStore(st *Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = st
	m.recovered = false
}

// Recover replays the intent log and reconciles anything unresolved against the
// venue, blocking entries until it completes.
//
// A record whose legs query as filled and MATCHED is reconstructed far enough
// to be closed -- which is the thing you actually need after a crash. Anything
// else is quarantined and halts the manager, because a position of unknown
// shape is exactly what should wake a human rather than be guessed at.
func (m *Manager) Recover(ctx context.Context) error {
	m.mu.Lock()
	st := m.store
	m.mu.Unlock()
	if st == nil {
		return ErrNoIntentStore
	}
	latest, readErr := st.Latest()
	if readErr != nil {
		m.logf("intent log: %v", readErr)
	}

	reconstructed, quarantined := 0, 0
	for id, r := range latest {
		if !r.Unresolved() {
			continue
		}
		t, ok := m.traders[r.Venue]
		if !ok {
			m.halt(fmt.Sprintf("unresolved intent %s on %s but no trader for that venue", id, r.Venue))
			quarantined++
			continue
		}
		var spot, perp execution.OrderResult
		if r.SpotOrderID != "" {
			spot, _ = t.Query(ctx, r.Symbol, r.SpotOrderID, execution.SpotLeg)
		}
		if r.PerpOrderID != "" {
			perp, _ = t.Query(ctx, r.Symbol, r.PerpOrderID, execution.PerpLeg)
		}

		matched := spot.FilledQty > 0 && perp.FilledQty > 0 &&
			absDiff(spot.FilledQty, perp.FilledQty) <= spot.FilledQty*closeDustTolerance
		if matched {
			pos := &LivePosition{
				ID: id, Venue: r.Venue, Symbol: r.Symbol,
				Quantity: spot.FilledQty, SpotEntry: spot, PerpEntry: perp,
				OpenedAt: r.At,
			}
			m.mu.Lock()
			m.positions[id] = pos
			m.mu.Unlock()
			_ = st.Append(IntentRecord{ID: id, Stage: StageOpen, Venue: r.Venue, Symbol: r.Symbol,
				SpotOrderID: r.SpotOrderID, PerpOrderID: r.PerpOrderID,
				SpotFilledQty: spot.FilledQty, PerpFilledQty: perp.FilledQty,
				Note: "reconstructed on startup"})
			m.logf("RECOVERED %s: spot %.8f perp %.8f", id, spot.FilledQty, perp.FilledQty)
			reconstructed++
			continue
		}

		_ = st.Append(IntentRecord{ID: id, Stage: StageQuarantined, Venue: r.Venue, Symbol: r.Symbol,
			SpotOrderID: r.SpotOrderID, PerpOrderID: r.PerpOrderID,
			SpotFilledQty: spot.FilledQty, PerpFilledQty: perp.FilledQty,
			Note: fmt.Sprintf("unmatched legs at stage %s", r.Stage)})
		m.halt(fmt.Sprintf("QUARANTINED %s: stage %s, spot %.8f against perp %.8f -- reconcile by hand",
			id, r.Stage, spot.FilledQty, perp.FilledQty))
		quarantined++
	}

	m.mu.Lock()
	m.recovered = true
	m.mu.Unlock()
	m.logf("recovery complete: %d reconstructed, %d quarantined, %d records", reconstructed, quarantined, len(latest))
	if quarantined > 0 {
		return fmt.Errorf("%w: %d intent(s) quarantined", ErrManualIntervention, quarantined)
	}
	return nil
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// New builds a Manager.
func New(cfg Config, traders map[string]Trader, readers map[string]execution.AccountReader,
	j *journal.Journal, logger *log.Logger) *Manager {
	return &Manager{
		cfg:       cfg,
		traders:   traders,
		readers:   readers,
		journal:   j,
		logger:    logger,
		positions: make(map[string]*LivePosition, 8),
	}
}

// Positions returns a snapshot of tracked positions.
func (m *Manager) Positions() []*LivePosition {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*LivePosition, 0, len(m.positions))
	for _, p := range m.positions {
		out = append(out, p)
	}
	return out
}

// Halted reports whether the manager has latched off.
func (m *Manager) Halted() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.halted, m.haltReason
}

// preflight runs every check that can refuse BEFORE any money moves.
//
// Ordered cheapest and most decisive first: a config switch beats a network
// call. Every refusal names itself, because "it did not open and I do not know
// why" is the state that makes an operator start disabling safety checks.
func (m *Manager) preflight(ctx context.Context, plan HedgePlan) (Trader, error) {
	if halted, why := m.Halted(); halted {
		return nil, fmt.Errorf("%w: %s", ErrManualIntervention, why)
	}
	if !m.cfg.Enabled {
		return nil, ErrDisabled
	}
	m.mu.Lock()
	haveStore, done := m.store != nil, m.recovered
	m.mu.Unlock()
	if !haveStore {
		return nil, ErrNoIntentStore
	}
	if !done {
		return nil, ErrNotRecovered
	}
	if m.cfg.KillSwitchPath != "" {
		if _, err := os.Stat(m.cfg.KillSwitchPath); err == nil {
			return nil, fmt.Errorf("%w: %s exists", ErrKillSwitch, m.cfg.KillSwitchPath)
		}
	}
	if plan.SpotRef <= 0 || plan.PerpRef <= 0 {
		return nil, fmt.Errorf("live: plan for %s %s has no reference prices; "+
			"slippage would be unmeasurable and the fill unauditable", plan.Venue, plan.Symbol)
	}

	t, ok := m.traders[plan.Venue]
	if !ok {
		return nil, fmt.Errorf("live: no trader configured for venue %q", plan.Venue)
	}

	// Testnet may run with unverified shapes -- that is what testnet is for.
	// Mainnet may not.
	if m.cfg.Mode == execution.Mainnet {
		if err := t.LiveTradingReady(); err != nil {
			return nil, fmt.Errorf("live: refusing mainnet order: %w", err)
		}
	}
	if t.Mode() != m.cfg.Mode {
		return nil, fmt.Errorf("live: trader for %s is in %s but config says %s; "+
			"refusing rather than guessing which is intended", plan.Venue, t.Mode(), m.cfg.Mode)
	}

	m.mu.Lock()
	open := 0
	for _, p := range m.positions {
		if p.Open() {
			open++
		}
	}
	m.mu.Unlock()
	if open >= m.cfg.MaxOpenPositions {
		return nil, fmt.Errorf("live: %d positions already open, cap is %d", open, m.cfg.MaxOpenPositions)
	}

	// Liquidation risk, read from the venue itself rather than from our own
	// records. If anything already open is in danger or unmeasured, this
	// refuses -- see risk.PortfolioRisk.SafeToOpen.
	if r, ok := m.readers[plan.Venue]; ok {
		snap, err := r.Snapshot(ctx)
		if err != nil {
			return nil, fmt.Errorf("live: cannot read %s account, refusing to trade blind: %w", plan.Venue, err)
		}
		pr := risk.AssessPortfolio([]execution.AccountSnapshot{snap}, m.cfg.Risk)
		if ok, why := pr.SafeToOpen(); !ok {
			return nil, fmt.Errorf("live: %s", why)
		}
	} else {
		return nil, fmt.Errorf("live: no account reader for %s; refusing to open a position "+
			"that could not then be reconciled", plan.Venue)
	}

	return t, nil
}

// OpenHedge buys spot, then sells the perp.
func (m *Manager) OpenHedge(ctx context.Context, plan HedgePlan) (*LivePosition, error) {
	t, err := m.preflight(ctx, plan)
	if err != nil {
		m.record("open_refused", plan.Venue, plan.Symbol, err.Error(), plan)
		return nil, err
	}

	spotF, err := t.Filters(ctx, plan.Symbol, execution.SpotLeg)
	if err != nil {
		return nil, err
	}
	perpF, err := t.Filters(ctx, plan.Symbol, execution.PerpLeg)
	if err != nil {
		return nil, err
	}

	desired := m.cfg.NotionalUSDPerLeg / plan.SpotRef
	qty, err := execution.MatchQuantity(spotF, perpF, desired, plan.SpotRef)
	if err != nil {
		m.record("open_refused", plan.Venue, plan.Symbol, err.Error(), plan)
		return nil, err
	}

	pos := &LivePosition{
		ID:       fmt.Sprintf("%s-%s-%d", plan.Venue, plan.Symbol, time.Now().UnixMilli()),
		Venue:    plan.Venue,
		Symbol:   plan.Symbol,
		Plan:     plan,
		Quantity: qty,
	}

	m.logf("OPEN %s %s qty %.8f (notional ~$%.2f/leg, deployed ~$%.2f)",
		plan.Venue, plan.Symbol, qty, qty*plan.SpotRef, 2*qty*plan.SpotRef)

	// THE INTENT IS WRITTEN BEFORE ANYTHING IS SENT.
	//
	// If the process dies between the two legs, this line is the only evidence
	// the position exists. The client order ID is generated here rather than
	// inline for the same reason: without it there is no handle to ask the
	// venue about afterwards, and a blind retry doubles the position.
	spotID := execution.NewClientOrderID(execution.SpotLeg)
	if err := m.store.Append(IntentRecord{
		ID: pos.ID, Stage: StageIntent, Venue: plan.Venue, Symbol: plan.Symbol,
		Quantity: qty, SpotOrderID: spotID,
	}); err != nil {
		return nil, fmt.Errorf("live: could not record intent; refusing to trade blind: %w", err)
	}

	// --- leg 1: BUY SPOT. If this fails, nothing is naked. ---
	spotReq := execution.OrderRequest{
		Venue: plan.Venue, Leg: execution.SpotLeg, Symbol: plan.Symbol,
		Side: execution.Buy, Type: execution.Market, Quantity: qty,
		ClientOrderID: spotID,
		RefPrice:      plan.SpotRef, SentAt: time.Now().UTC(),
	}
	spotRes, err := m.placeConfirmed(ctx, t, spotReq)
	if err != nil && !errors.Is(err, ErrPartialFill) {
		m.record("leg1_failed", plan.Venue, plan.Symbol, err.Error(), spotRes)
		return nil, fmt.Errorf("live: spot leg failed, nothing opened: %w", err)
	}
	if errors.Is(err, ErrPartialFill) {
		// Tolerated on the FIRST leg only: the hedge below is sized to what
		// actually filled, so a partial simply opens a smaller position.
		m.record("leg1_partial", plan.Venue, plan.Symbol, err.Error(), spotRes)
		m.logf("leg 1 partial: %v -- hedging to the actual fill", err)
	}
	pos.SpotEntry = spotRes
	// One leg on, no hedge. This is the exposed stage, and it must be on disk
	// before the second order is attempted.
	_ = m.store.Append(IntentRecord{
		ID: pos.ID, Stage: StageSpotFilled, Venue: plan.Venue, Symbol: plan.Symbol,
		Quantity: qty, SpotOrderID: spotID, SpotFilledQty: spotRes.FilledQty,
	})
	m.record("leg1_filled", plan.Venue, plan.Symbol, spotRes.String(), spotRes)

	// Abort on a bad first fill, before committing the second leg. Unwinding
	// one leg costs a round trip; entering a position that has already spent
	// its edge costs the whole position.
	if bps, ok := spotRes.SlippageBps(); ok && bps > m.cfg.MaxSlippageBpsPerLeg {
		reason := fmt.Sprintf("spot fill slipped %.2f bps, cap is %.2f", bps, m.cfg.MaxSlippageBpsPerLeg)
		return nil, m.unwind(ctx, t, spotRes, execution.PerpLeg, reason)
	}

	// Size the hedge to what ACTUALLY filled, not what was requested. A
	// partial spot fill hedged at the requested size is over-hedged, which is
	// a net short position wearing a market-neutral label.
	hedgeQty := perpF.RoundQuantity(spotRes.FilledQty)
	if err := perpF.Acceptable(hedgeQty, plan.PerpRef); err != nil {
		return nil, m.unwind(ctx, t, spotRes, execution.PerpLeg,
			fmt.Sprintf("spot filled %.8f but that is unhedgeable: %v", spotRes.FilledQty, err))
	}

	// --- leg 2: SELL PERP. From here until it fills, we are long spot. ---
	perpID := execution.NewClientOrderID(execution.PerpLeg)
	_ = m.store.Append(IntentRecord{
		ID: pos.ID, Stage: StageSpotFilled, Venue: plan.Venue, Symbol: plan.Symbol,
		Quantity: qty, SpotOrderID: spotID, PerpOrderID: perpID,
		SpotFilledQty: spotRes.FilledQty,
	})
	perpReq := execution.OrderRequest{
		Venue: plan.Venue, Leg: execution.PerpLeg, Symbol: plan.Symbol,
		Side: execution.Sell, Type: execution.Market, Quantity: hedgeQty,
		ClientOrderID: perpID,
		RefPrice:      plan.PerpRef, SentAt: time.Now().UTC(),
	}
	perpRes, err := m.placeConfirmed(ctx, t, perpReq)
	if err != nil && !errors.Is(err, ErrPartialFill) {
		return nil, m.unwind(ctx, t, spotRes, execution.PerpLeg, err.Error())
	}
	pos.PerpEntry = perpRes
	if errors.Is(err, ErrPartialFill) {
		// A PARTIAL HEDGE IS AN OPEN DIRECTIONAL POSITION.
		//
		// Spot Q against perp p leaves net long (Q-p). Until 2026-08-19 this
		// was recorded as a complete hedge and the difference never surfaced.
		//
		// It is deliberately NOT flattened automatically. Trimming the spot to
		// match needs another market order that can itself fill partially, and
		// that recovery loop is not being written against zero test coverage.
		// The position is registered so it is visible and closeable, and the
		// manager halts so a human decides.
		m.mu.Lock()
		pos.OpenedAt = time.Now().UTC()
		m.positions[pos.ID] = pos
		m.mu.Unlock()
		m.record("leg2_partial", plan.Venue, plan.Symbol, err.Error(), pos)
		m.halt(fmt.Sprintf("%s: hedge incomplete -- spot %.8f against perp %.8f, net long %.8f %s",
			pos.ID, spotRes.FilledQty, perpRes.FilledQty,
			spotRes.FilledQty-perpRes.FilledQty, plan.Symbol))
		return pos, fmt.Errorf("%w: %s opened with an incomplete hedge", ErrManualIntervention, pos.ID)
	}
	m.record("leg2_filled", plan.Venue, plan.Symbol, perpRes.String(), perpRes)

	if sb, ok1 := spotRes.SlippageBps(); ok1 {
		if pb, ok2 := perpRes.SlippageBps(); ok2 {
			pos.SpotEntrySlipBps, pos.PerpEntrySlipBps = sb, pb
			pos.EntrySlipMeasured = true
		}
	}

	pos.OpenedAt = time.Now().UTC()
	_ = m.store.Append(IntentRecord{
		ID: pos.ID, Stage: StageOpen, Venue: plan.Venue, Symbol: plan.Symbol,
		Quantity: qty, SpotOrderID: spotID, PerpOrderID: perpID,
		SpotFilledQty: spotRes.FilledQty, PerpFilledQty: perpRes.FilledQty,
	})
	m.mu.Lock()
	m.positions[pos.ID] = pos
	m.mu.Unlock()

	slip := "UNMEASURED"
	if pos.EntrySlipMeasured {
		slip = fmt.Sprintf("%.2f bps total", pos.SpotEntrySlipBps+pos.PerpEntrySlipBps)
	}
	m.logf("OPENED %s: spot %.8f @ %.8f, perp %.8f @ %.8f, entry slippage %s",
		pos.ID, spotRes.FilledQty, spotRes.AvgFillPrice, perpRes.FilledQty, perpRes.AvgFillPrice, slip)
	m.record("opened", plan.Venue, plan.Symbol, "hedge open", pos)

	return pos, nil
}

// CloseHedge buys back the perp, then sells the spot.
//
// NOT the reverse of opening. Closing the spot first would leave a naked
// short, which is the state this package exists to never be in.
// closeDustTolerance is the fraction of a position that may remain unclosed
// and still count as fully closed. Venues leave fractions of a lot behind;
// refusing to finish over 0.01% would strand positions permanently.
const closeDustTolerance = 0.005

func (m *Manager) CloseHedge(ctx context.Context, pos *LivePosition, reason string) error {
	if !pos.Open() {
		return fmt.Errorf("live: position %s is already closed", pos.ID)
	}
	t, ok := m.traders[pos.Venue]
	if !ok {
		return fmt.Errorf("live: no trader for venue %q", pos.Venue)
	}

	// Note there is NO kill-switch or Enabled check here. Closing must always
	// be possible. A safety mechanism that prevents you from exiting is not a
	// safety mechanism.
	if m.store != nil {
		_ = m.store.Append(IntentRecord{
			ID: pos.ID, Stage: StageClosing, Venue: pos.Venue, Symbol: pos.Symbol,
			SpotFilledQty: pos.SpotEntry.FilledQty, PerpFilledQty: pos.PerpEntry.FilledQty,
			Note: reason,
		})
	}
	m.logf("CLOSE %s: %s", pos.ID, reason)

	// --- leg 1: BUY PERP (close the short), reduce-only. ---
	perpReq := execution.OrderRequest{
		Venue: pos.Venue, Leg: execution.PerpLeg, Symbol: pos.Symbol,
		Side: execution.Buy, Type: execution.Market,
		Quantity:      pos.PerpEntry.FilledQty - pos.PerpClosedQty,
		ReduceOnly:    true,
		ClientOrderID: execution.NewClientOrderID(execution.PerpLeg),
		SentAt:        time.Now().UTC(),
	}
	perpRes, err := m.placeConfirmed(ctx, t, perpReq)
	if err != nil && !errors.Is(err, ErrPartialFill) {
		// The short is still open and still hedged by the spot. Bad, but not
		// naked -- so this is an error, not a halt.
		m.record("close_leg1_failed", pos.Venue, pos.Symbol, err.Error(), perpRes)
		return fmt.Errorf("live: could not close perp leg of %s (position remains hedged and open): %w", pos.ID, err)
	}
	pos.PerpExit = perpRes
	pos.PerpClosedQty += perpRes.FilledQty

	// SELL ONLY THE SPOT THE PERP CLOSE ACTUALLY UNHEDGED.
	//
	// Entry is long spot Q against short perp Q. If the buy-back only fills q:
	//
	//	sell spot Q  ->  spot 0,   short perp (Q-q)  ->  NAKED SHORT
	//	sell spot q  ->  spot Q-q, short perp (Q-q)  ->  still hedged
	//
	// The old code sized this from pos.SpotEntry.FilledQty regardless, so any
	// partial perp close left a directional short with nothing against it and
	// marked the position closed. Found by external code review 2026-08-19.
	perpRemaining := pos.PerpEntry.FilledQty - pos.PerpClosedQty
	fullyClosed := pos.PerpEntry.FilledQty <= 0 ||
		perpRemaining <= pos.PerpEntry.FilledQty*closeDustTolerance

	spotQty := pos.SpotEntry.FilledQty - pos.SpotClosedQty
	if !fullyClosed {
		if unhedged := perpRes.FilledQty; unhedged < spotQty {
			spotQty = unhedged
		}
	}
	if spotQty <= 0 {
		m.record("close_no_spot_to_sell", pos.Venue, pos.Symbol,
			"perp close filled nothing; position remains hedged and open", perpRes)
		return fmt.Errorf("live: perp close of %s filled %.8f; nothing to unwind on spot",
			pos.ID, perpRes.FilledQty)
	}

	// --- leg 2: SELL SPOT. Between the two we are long spot. ---
	spotReq := execution.OrderRequest{
		Venue: pos.Venue, Leg: execution.SpotLeg, Symbol: pos.Symbol,
		Side: execution.Sell, Type: execution.Market,
		Quantity:      spotQty,
		ClientOrderID: execution.NewClientOrderID(execution.SpotLeg),
		SentAt:        time.Now().UTC(),
	}
	spotRes, err := m.placeConfirmed(ctx, t, spotReq)
	if err != nil {
		// Perp closed, spot did not sell: unhedged long spot. Escalate.
		m.halt(fmt.Sprintf("%s: perp leg closed but spot leg did not sell (%v); holding unhedged %.8f %s",
			pos.ID, err, pos.SpotEntry.FilledQty, pos.Symbol))
		return fmt.Errorf("%w: %s", ErrManualIntervention, pos.ID)
	}
	pos.SpotExit = spotRes
	pos.SpotClosedQty += spotRes.FilledQty

	// RECONCILE BEFORE DECLARING IT CLOSED.
	//
	// A position marked closed while a leg still carries quantity disappears
	// from monitoring while remaining exposed -- the same shape as the
	// cross-venue freeze, with real money behind it.
	if !fullyClosed {
		m.record("close_partial", pos.Venue, pos.Symbol,
			fmt.Sprintf("perp closed %.8f of %.8f, spot sold %.8f of %.8f; still hedged, retry required",
				pos.PerpClosedQty, pos.PerpEntry.FilledQty,
				pos.SpotClosedQty, pos.SpotEntry.FilledQty), pos)
		m.logf("CLOSE PARTIAL %s: perp %.8f/%.8f spot %.8f/%.8f -- remains open and hedged",
			pos.ID, pos.PerpClosedQty, pos.PerpEntry.FilledQty,
			pos.SpotClosedQty, pos.SpotEntry.FilledQty)
		return fmt.Errorf("live: %s only partially closed (perp %.8f of %.8f); position remains open and hedged",
			pos.ID, pos.PerpClosedQty, pos.PerpEntry.FilledQty)
	}

	pos.ClosedAt = time.Now().UTC()
	pos.CloseNote = reason
	if m.store != nil {
		// Only now is it safe to mark this resolved: both legs reconciled.
		_ = m.store.Append(IntentRecord{
			ID: pos.ID, Stage: StageClosed, Venue: pos.Venue, Symbol: pos.Symbol,
			SpotFilledQty: pos.SpotClosedQty, PerpFilledQty: pos.PerpClosedQty,
			Note: reason,
		})
	}
	m.record("closed", pos.Venue, pos.Symbol, reason, pos)
	m.logf("CLOSED %s: %s", pos.ID, reason)
	return nil
}

// unwind closes a leg that filled when its partner did not.
//
// This is the naked-leg path. It returns an error in every case -- the
// position did not open. What differs is how bad it is: a successful unwind
// costs a round trip, a failed one costs a human being woken up.
func (m *Manager) unwind(ctx context.Context, t Trader, filled execution.OrderResult,
	failedLeg execution.Leg, reason string) error {

	naked := execution.NewNakedLeg(filled, failedLeg, reason)
	m.logf("NAKED LEG %s %s: %s -- unwinding %.8f",
		naked.Venue, naked.Symbol, reason, filled.FilledQty)
	m.record("naked_leg", naked.Venue, naked.Symbol, reason, naked)

	if filled.FilledQty <= 0 {
		// Nothing actually filled, so nothing is exposed. The partner's
		// failure is just a failure.
		return fmt.Errorf("live: %s aborted: %s", naked.Symbol, reason)
	}

	remedyRes, err := m.placeConfirmed(ctx, t, naked.Remedy)
	if err != nil {
		m.halt(fmt.Sprintf("%s %s: naked %.8f could not be unwound (%v)",
			naked.Venue, naked.Symbol, filled.FilledQty, err))
		m.record("unwind_failed", naked.Venue, naked.Symbol, err.Error(), remedyRes)
		return fmt.Errorf("%w: %s %s, %.8f exposed",
			ErrManualIntervention, naked.Venue, naked.Symbol, filled.FilledQty)
	}

	m.record("unwound", naked.Venue, naked.Symbol, remedyRes.String(), remedyRes)
	m.logf("UNWOUND %s %s: flat again, cost one round trip", naked.Venue, naked.Symbol)
	return fmt.Errorf("live: %s aborted and unwound: %s", naked.Symbol, reason)
}

// placeConfirmed sends an order and does not return until its outcome is
// known, or until it has run out of attempts.
//
// This is where ErrUnconfirmed is handled properly. A timeout is NOT a
// failure: the order may be live. So on ErrUnconfirmed it QUERIES by client
// ID rather than resending -- resending is how one position becomes two.
// ErrPartialFill marks an order the venue filled only in part.
//
// It is returned WITH a usable result rather than instead of one, because the
// right response differs by leg. On a first leg a partial is fine -- the hedge
// is sized to what actually filled. On a second leg it leaves the first leg
// exposed. Returning nil for both, as this code did until 2026-08-19, made the
// difference invisible.
var ErrPartialFill = errors.New("live: order filled only in part")

func (m *Manager) placeConfirmed(ctx context.Context, t Trader, req execution.OrderRequest) (execution.OrderResult, error) {
	res, err := t.Place(ctx, req)

	if err != nil && !errors.Is(err, execution.ErrUnconfirmed) {
		// A clean rejection. Nothing is live, so there is nothing to confirm.
		return res, err
	}

	// Either the venue confirmed a fill, or the outcome is unknown, or (Bybit)
	// the create response never carries fills. All three are resolved the same
	// way: ask the venue what happened.
	if err == nil && res.Status == execution.StatusFilled && res.AvgFillPrice > 0 {
		return res, nil
	}

	attempts := m.cfg.ConfirmAttempts
	if attempts < 1 {
		attempts = 3
	}
	backoff := m.cfg.ConfirmBackoff
	if backoff <= 0 {
		backoff = 2 * time.Second
	}

	m.logf("confirming %s %s (id %s): %v", req.Leg, req.Symbol, req.ClientOrderID, err)

	var last error = err
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return res, fmt.Errorf("%w: context cancelled while confirming %s",
				execution.ErrUnconfirmed, req.ClientOrderID)
		case <-time.After(backoff):
		}

		q, qerr := t.Query(ctx, req.Symbol, req.ClientOrderID, req.Leg)
		if qerr != nil {
			last = qerr
			continue
		}
		// Carry the reference price across: Query has no way to know it, and
		// without it slippage is unmeasurable for the rest of this order's
		// life.
		q.RefPrice = req.RefPrice
		q.SentAt = req.SentAt

		switch q.Status {
		case execution.StatusFilled, execution.StatusPartiallyFilled:
			if q.AvgFillPrice > 0 {
				// A PARTIAL IS NOT A FILL. Hand back the result so the caller
				// can size from what genuinely filled, but make it impossible
				// to mistake for a completed order.
				if q.Status == execution.StatusPartiallyFilled ||
					(q.RequestedQty > 0 && q.FilledQty < q.RequestedQty*(1-closeDustTolerance)) {
					return q, fmt.Errorf("%w: %s %s filled %.8f of %.8f",
						ErrPartialFill, req.Leg, req.Symbol, q.FilledQty, q.RequestedQty)
				}
				return q, nil
			}
		case execution.StatusRejected, execution.StatusCanceled:
			return q, fmt.Errorf("live: %s %s %s: %s", req.Leg, req.Symbol, q.Status, q.RawError)
		}
		last = fmt.Errorf("%w: %s still %s after %d attempt(s)",
			execution.ErrUnconfirmed, req.ClientOrderID, q.Status, i+1)
	}

	// Out of attempts and still unsure. This is deliberately NOT retried as a
	// new order.
	return res, fmt.Errorf("%w: %s %s could not be confirmed after %d attempts: %v",
		execution.ErrUnconfirmed, req.Leg, req.Symbol, attempts, last)
}

// halt latches the manager off.
func (m *Manager) halt(reason string) {
	m.mu.Lock()
	m.halted = true
	m.haltReason = reason
	m.mu.Unlock()

	m.logger.Printf("!!! HALT -- MANUAL INTERVENTION REQUIRED: %s", reason)
	m.logger.Printf("!!! No further positions will be opened by this process.")
	m.record("halt", "", "", reason, nil)
	if m.journal != nil {
		_ = m.journal.Flush()
	}
}

// record writes one event to the journal and never fails the caller for it.
// A journal write that errors must not abort an unwind.
func (m *Manager) record(kind, venue, symbol, msg string, payload any) {
	if m.journal == nil {
		return
	}
	now := time.Now().UTC()
	e := event{
		Type: "live_" + kind, TsMs: now.UnixMilli(),
		Venue: venue, Symbol: symbol, Mode: string(m.cfg.Mode),
		Message: msg, Payload: payload, At: now,
	}
	if err := m.journal.Write(e); err != nil {
		m.logger.Printf("journal write failed for %s: %v", kind, err)
		return
	}
	// Live events are flushed immediately. The paper monitor batches because
	// volume matters there; here, a record that exists only in a buffer when
	// the process dies is a record of a real position that no longer exists.
	_ = m.journal.Flush()
}

func (m *Manager) logf(format string, args ...any) {
	m.logger.Printf(format, args...)
}
