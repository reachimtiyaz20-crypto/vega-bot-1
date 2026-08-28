package live

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/execution"
)

// testManager wires a Manager to a scripted venue.
//
// DefaultConfig deliberately ships disabled with a kill-switch path, so a test
// that forgets these never reaches the code it means to exercise -- it just
// gets ErrDisabled and passes for the wrong reason.
func testManager(t *testing.T, script []FakeFill) (*Manager, *FakeTrader) {
	t.Helper()
	ft := NewFakeTrader("fake")
	ft.ModeVal = execution.Testnet
	ft.Script = script

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.KillSwitchPath = ""
	cfg.Mode = execution.Testnet
	cfg.MaxOpenPositions = 5
	cfg.ConfirmBackoff = time.Millisecond
	cfg.ConfirmAttempts = 2

	ar := &FakeAccountReader{VenueName: "fake", ModeVal: execution.Testnet}
	m := New(cfg, map[string]Trader{"fake": ft},
		map[string]execution.AccountReader{"fake": ar}, nil,
		log.New(io.Discard, "", 0))

	// Durable state is not optional for live trading, so a fixture must supply
	// it rather than work around it.
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	m.AttachStore(st)
	if err := m.Recover(context.Background()); err != nil {
		t.Fatalf("Recover on an empty log: %v", err)
	}
	return m, ft
}

func plan() HedgePlan {
	return HedgePlan{Venue: "fake", Symbol: "TESTUSDT", SpotRef: 100, PerpRef: 100}
}

// TestPartialFirstLegHedgesToTheActualFill.
//
// A partial spot fill is legitimate: the hedge is sized to what filled, so the
// position simply opens smaller. What must NOT happen is hedging the requested
// quantity against a smaller spot fill -- that is a net short wearing a
// market-neutral label.
func TestPartialFirstLegHedgesToTheActualFill(t *testing.T) {
	m, ft := testManager(t, []FakeFill{{FillFrac: 0.5}, {FillFrac: 1}})
	pos, err := m.OpenHedge(context.Background(), plan())
	if err != nil {
		t.Fatalf("a partial first leg should still open: %v", err)
	}
	orders := ft.Placed()
	if len(orders) != 2 {
		t.Fatalf("expected 2 orders, got %d", len(orders))
	}
	spotFilled := pos.SpotEntry.FilledQty
	if orders[1].Quantity != spotFilled {
		t.Fatalf("hedged %.8f against a spot fill of %.8f -- over-hedged is a naked short",
			orders[1].Quantity, spotFilled)
	}
}

// TestPartialSecondLegDoesNotRecordACompleteHedge.
//
// Spot Q against perp p leaves net long (Q-p). Before 2026-08-19 this was
// recorded as a complete hedge because placeConfirmed returned a partial with a
// nil error.
func TestPartialSecondLegDoesNotRecordACompleteHedge(t *testing.T) {
	m, _ := testManager(t, []FakeFill{{FillFrac: 1}, {FillFrac: 0.5}})
	pos, err := m.OpenHedge(context.Background(), plan())
	if err == nil {
		t.Fatal("an incomplete hedge was accepted silently")
	}
	if !errors.Is(err, ErrManualIntervention) {
		t.Fatalf("wrong error for an incomplete hedge: %v", err)
	}
	if pos == nil {
		t.Fatal("position not returned; an exposed position must stay visible")
	}
	if halted, _ := m.Halted(); !halted {
		t.Fatal("manager did not halt on an incomplete hedge")
	}
	if pos.PerpEntry.FilledQty >= pos.SpotEntry.FilledQty {
		t.Fatalf("perp %.8f vs spot %.8f: fixture did not produce a partial",
			pos.PerpEntry.FilledQty, pos.SpotEntry.FilledQty)
	}
}

// TestPartialCloseDoesNotSellTheWholeSpot.
//
// THE NAKED SHORT.
//
// Entry is long spot Q against short perp Q. If the buy-back fills only q,
// selling the full spot leaves a residual short perp with nothing against it.
// The old code sized the spot sale from SpotEntry.FilledQty regardless and then
// marked the position closed.
func TestPartialCloseDoesNotSellTheWholeSpot(t *testing.T) {
	m, ft := testManager(t, []FakeFill{
		{FillFrac: 1},   // open spot
		{FillFrac: 1},   // open perp
		{FillFrac: 0.4}, // close perp -- PARTIAL
		{FillFrac: 1},   // close spot
	})
	pos, err := m.OpenHedge(context.Background(), plan())
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	entryQty := pos.SpotEntry.FilledQty

	err = m.CloseHedge(context.Background(), pos, "test")
	if err == nil {
		t.Fatal("a partial close reported success")
	}

	orders := ft.Placed()
	if len(orders) != 4 {
		t.Fatalf("expected 4 orders, got %d", len(orders))
	}
	spotSell := orders[3]
	perpClosed := pos.PerpExit.FilledQty

	if spotSell.Quantity > perpClosed {
		t.Fatalf("sold %.8f of spot against a perp close of %.8f -- leaves a NAKED SHORT of %.8f",
			spotSell.Quantity, perpClosed, spotSell.Quantity-perpClosed)
	}
	if spotSell.Quantity >= entryQty {
		t.Fatalf("sold the entire spot position (%.8f) after a partial perp close", spotSell.Quantity)
	}
	if !pos.Open() {
		t.Fatal("position marked closed while a leg still carries quantity")
	}
}

// TestRestartReconstructsAnOpenPosition.
//
// A hedge is two orders and the account is exposed between them. Before
// 2026-08-19 a process that died anywhere in that window came back with no
// memory the position existed: nothing reconciled it, nothing closed it, and
// the next pass would open another alongside it.
//
// The same FakeTrader is reused across both managers on purpose -- a restart
// loses the process, not the exchange.
func TestRestartReconstructsAnOpenPosition(t *testing.T) {
	dir := t.TempDir()
	ft := NewFakeTrader("fake")
	ft.ModeVal = execution.Testnet
	ft.Script = []FakeFill{{FillFrac: 1}, {FillFrac: 1}}

	mk := func() *Manager {
		cfg := DefaultConfig()
		cfg.Enabled = true
		cfg.KillSwitchPath = ""
		cfg.Mode = execution.Testnet
		cfg.MaxOpenPositions = 5
		cfg.ConfirmBackoff = time.Millisecond
		cfg.ConfirmAttempts = 2
		ar := &FakeAccountReader{VenueName: "fake", ModeVal: execution.Testnet}
		m := New(cfg, map[string]Trader{"fake": ft},
			map[string]execution.AccountReader{"fake": ar}, nil, log.New(io.Discard, "", 0))
		st, err := NewStore(dir)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		m.AttachStore(st)
		return m
	}

	a := mk()
	if err := a.Recover(context.Background()); err != nil {
		t.Fatalf("recover on empty log: %v", err)
	}
	pos, err := a.OpenHedge(context.Background(), plan())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// --- the process dies here; nothing closed the position ---

	b := mk()
	if _, err := b.OpenHedge(context.Background(), plan()); !errors.Is(err, ErrNotRecovered) {
		t.Fatalf("opened a new position before reconciling the old one: %v", err)
	}
	if err := b.Recover(context.Background()); err != nil {
		t.Fatalf("recover after restart: %v", err)
	}

	got := b.Positions()
	if len(got) != 1 {
		t.Fatalf("reconstructed %d positions, want 1", len(got))
	}
	if got[0].ID != pos.ID {
		t.Fatalf("wrong position: %s, want %s", got[0].ID, pos.ID)
	}
	if got[0].SpotEntry.FilledQty != pos.SpotEntry.FilledQty {
		t.Fatalf("quantity lost across restart: %.8f vs %.8f",
			got[0].SpotEntry.FilledQty, pos.SpotEntry.FilledQty)
	}

	// Reconstruction is only worth anything if the position can be CLOSED.
	ft.Script = append(ft.Script, FakeFill{FillFrac: 1}, FakeFill{FillFrac: 1})
	if err := b.CloseHedge(context.Background(), got[0], "post-restart"); err != nil {
		t.Fatalf("could not close a reconstructed position: %v", err)
	}
}

// TestRestartQuarantinesUnmatchedLegs.
//
// The counterpart. A record whose legs do not reconcile must NOT be
// reconstructed from a guess -- it halts and waits for a human. Silently
// inventing a position's shape is worse than admitting it is unknown.
func TestRestartQuarantinesUnmatchedLegs(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// One leg away, its outcome never recorded: the shape this exists to catch.
	if err := st.Append(IntentRecord{
		ID: "fake-TESTUSDT-1", Stage: StageSpotFilled, Venue: "fake",
		Symbol: "TESTUSDT", SpotOrderID: "spot-orphan", SpotFilledQty: 0.5,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	m, _ := testManager(t, nil)
	m.AttachStore(st)
	err = m.Recover(context.Background())
	if !errors.Is(err, ErrManualIntervention) {
		t.Fatalf("unmatched legs did not demand intervention: %v", err)
	}
	if halted, _ := m.Halted(); !halted {
		t.Fatal("manager did not halt on a quarantined intent")
	}
	if n := len(m.Positions()); n != 0 {
		t.Fatalf("invented %d position(s) from an unresolved record", n)
	}
}
