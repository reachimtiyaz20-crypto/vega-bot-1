package live

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/execution"
)

// FakeTrader is a scripted venue for exercising the failure paths.
//
// # WHY THIS EXISTS
//
// pkg/live had no tests at all. Every defect in it -- partial fills accepted as
// complete, a full spot sale after a partial perp close, no recovery after a
// restart -- is invisible to a suite that only ever sees clean fills, and none
// of them can be reproduced against a real exchange without risking money.
//
// The point is not to simulate an exchange faithfully. It is to produce, on
// demand, the specific answers that broke this code: a partial fill, an
// ambiguous Place that a later Query resolves, a rejection, and silence.
type FakeTrader struct {
	VenueName string
	ModeVal   execution.Mode
	Ready     error
	Filt      execution.SymbolFilters

	// Script is consumed one entry per Place call, in order. When it runs out
	// every subsequent order fills completely.
	Script []FakeFill

	mu     sync.Mutex
	placed []execution.OrderRequest
	orders map[string]execution.OrderResult
	n      int
}

// FakeFill describes what the venue does with one order.
type FakeFill struct {
	// FillFrac of the requested quantity that fills. 1 is complete, 0.4 is a
	// partial, 0 is nothing.
	FillFrac float64

	// PlaceErr is returned by Place. Set it to execution.ErrUnconfirmed to
	// model a venue that answered ambiguously -- the case where the order may
	// well have filled and retrying would double it.
	PlaceErr error

	// QueryFillFrac is what a later Query reports. Zero means "same as
	// FillFrac", which is the ordinary case; setting it differently models a
	// venue whose create response and query response disagree.
	QueryFillFrac float64

	// Price is the average fill price. Zero defaults to the request's RefPrice,
	// which makes slippage zero and keeps tests focused on quantity handling.
	Price float64
}

func NewFakeTrader(venue string) *FakeTrader {
	return &FakeTrader{
		VenueName: venue,
		orders:    map[string]execution.OrderResult{},
		Filt: execution.SymbolFilters{
			Venue: venue, StepSize: 0.00000001, MinQty: 0.00000001,
			MinNotional: 5, TickSize: 0.01, FetchedAt: time.Now().UTC(),
		},
	}
}

func (f *FakeTrader) Venue() string           { return f.VenueName }
func (f *FakeTrader) Mode() execution.Mode    { return f.ModeVal }
func (f *FakeTrader) LiveTradingReady() error { return f.Ready }

func (f *FakeTrader) Filters(ctx context.Context, symbol string, leg execution.Leg) (execution.SymbolFilters, error) {
	g := f.Filt
	g.Symbol, g.Leg = symbol, leg
	return g, nil
}

// Placed returns every order the manager sent, in order. Tests assert on
// QUANTITIES here -- that is where a naked leg shows up.
func (f *FakeTrader) Placed() []execution.OrderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]execution.OrderRequest, len(f.placed))
	copy(out, f.placed)
	return out
}

func statusFor(frac float64) execution.OrderStatus {
	switch {
	case frac >= 1:
		return execution.StatusFilled
	case frac > 0:
		return execution.StatusPartiallyFilled
	default:
		return execution.StatusUnknown
	}
}

func (f *FakeTrader) Place(ctx context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.placed = append(f.placed, req)

	fill := FakeFill{FillFrac: 1}
	if f.n < len(f.Script) {
		fill = f.Script[f.n]
	}
	f.n++

	px := fill.Price
	if px == 0 {
		px = req.RefPrice
	}
	if px == 0 {
		px = 1
	}

	res := execution.OrderResult{
		Venue: f.VenueName, Leg: req.Leg, Symbol: req.Symbol, Side: req.Side,
		ClientOrderID: req.ClientOrderID, VenueOrderID: fmt.Sprintf("fake-%d", f.n),
		Status:       statusFor(fill.FillFrac),
		RequestedQty: req.Quantity,
		FilledQty:    req.Quantity * fill.FillFrac,
		AvgFillPrice: px,
		RefPrice:     req.RefPrice,
		SentAt:       req.SentAt,
		ReportedAt:   time.Now().UTC(),
	}

	// What a later Query will say. Modelling this separately matters: an
	// unconfirmed Place followed by a Query showing a fill is exactly the shape
	// that makes a blind retry double the position.
	q := res
	if fill.QueryFillFrac > 0 {
		q.FilledQty = req.Quantity * fill.QueryFillFrac
		q.Status = statusFor(fill.QueryFillFrac)
	}
	f.orders[req.ClientOrderID] = q

	if fill.PlaceErr != nil {
		return res, fill.PlaceErr
	}
	return res, nil
}

func (f *FakeTrader) Query(ctx context.Context, symbol, clientOrderID string, leg execution.Leg) (execution.OrderResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.orders[clientOrderID]
	if !ok {
		return execution.OrderResult{Status: execution.StatusUnknown}, nil
	}
	return r, nil
}

// FakeAccountReader satisfies the reconciliation requirement.
//
// The manager refuses to open a position on a venue it cannot later reconcile
// against, which is correct and which a fixture must satisfy rather than work
// around. Zero values are returned deliberately: these tests are about order
// quantities, not balances.
type FakeAccountReader struct {
	VenueName string
	ModeVal   execution.Mode
	Snap      execution.AccountSnapshot
	Funding   []execution.FundingPayment
	Err       error
}

func (f *FakeAccountReader) Venue() string        { return f.VenueName }
func (f *FakeAccountReader) Mode() execution.Mode { return f.ModeVal }

func (f *FakeAccountReader) Snapshot(ctx context.Context) (execution.AccountSnapshot, error) {
	return f.Snap, f.Err
}

func (f *FakeAccountReader) FundingSince(ctx context.Context, since time.Time) ([]execution.FundingPayment, error) {
	return f.Funding, f.Err
}
