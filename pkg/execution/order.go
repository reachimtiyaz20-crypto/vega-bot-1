package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// THE TWO-LEG PROBLEM
//
// A cash-and-carry needs two fills on two different markets: buy spot, sell
// perp. There is no exchange primitive that does both atomically. So there is
// always a window -- usually milliseconds, occasionally much longer -- in which
// one leg is filled and the other is not.
//
// In that window the position is NAKED. Long spot with no hedge, or short perp
// with no spot. A 2% move against a naked leg wipes out roughly two months of
// funding income on a position that was supposed to be market neutral.
//
// There is no undo. An order that filled cannot be un-filled. The only remedy
// is a COMPENSATING TRADE -- close the leg that filled -- and that costs
// another round trip in fees plus whatever the price did in between.
//
// So the rules encoded in this file are:
//
//  1. Every order carries a client-generated ID. A network timeout does NOT
//     mean the order failed; it means the outcome is unknown. Retrying without
//     an idempotency key is how you end up with two positions.
//
//  2. Fills are READ BACK, never assumed. The API returning 200 is not a fill.
//     AvgFillPrice comes from the exchange's response or a follow-up query, and
//     if it cannot be determined the order is marked unconfirmed rather than
//     booked at the price we hoped for. This is the same discipline as the
//     funding income endpoint: settled numbers only.
//
//  3. Slippage is MEASURED, not assumed. Every request records the reference
//     price at the moment of sending; every result computes the gap. The paper
//     rig had to estimate this from the book. Live does not have to guess.
//
//  4. A naked leg is a named, journaled state with an explicit remedy -- not an
//     error return that some caller might swallow.

// Side is the direction of an order.
type Side string

const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)

// Opposite returns the side that closes this one.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// OrderType is how the order is priced.
//
// VEGA uses MARKET for both legs, deliberately. A limit order that does not
// fill leaves the other leg naked for an unbounded time, and "unbounded time
// naked" is a worse risk than a few bps of taker fee. The cost model has
// always assumed taker on all four legs for exactly this reason -- using maker
// orders here would make the economics module optimistic, which is the failure
// mode this whole project exists to avoid.
type OrderType string

const (
	Market OrderType = "MARKET"
	Limit  OrderType = "LIMIT"
)

// Leg says which market an order belongs to. Kept explicit because spot and
// futures have different hosts, different endpoints, different symbol filters
// and different fee schedules on both venues.
type Leg string

const (
	SpotLeg Leg = "spot"
	PerpLeg Leg = "perp"
)

var (
	// ErrNotImplemented is returned by an order path that has not been built
	// or verified for a venue. It is deliberately an error, not a no-op: a
	// silent no-op on one leg is precisely how a hedge becomes naked.
	ErrNotImplemented = errors.New("execution: order path not implemented for this venue")

	// ErrUnconfirmed means the order was sent but its outcome could not be
	// established -- a timeout, a truncated response, a venue error after
	// submission. It is NOT a failure. The position may well exist. The caller
	// must reconcile against the account before doing anything else, and must
	// never simply retry.
	ErrUnconfirmed = errors.New("execution: order outcome UNCONFIRMED -- reconcile before retrying")

	// ErrNakedLeg means one leg filled and the other did not.
	ErrNakedLeg = errors.New("execution: NAKED LEG -- one side filled, the other did not; position is unhedged")

	// ErrDryRun is returned when a placer is configured to refuse real orders.
	ErrDryRun = errors.New("execution: dry-run mode, order not sent")
)

// OrderRequest is one leg of a trade.
type OrderRequest struct {
	Venue  string    `json:"venue"`
	Leg    Leg       `json:"leg"`
	Symbol string    `json:"symbol"`
	Side   Side      `json:"side"`
	Type   OrderType `json:"type"`

	// Quantity is in base asset units, already rounded to the venue's step
	// size. Rounding is NOT done here: it needs the symbol filters, which are
	// venue-specific, and a silent rounding at this layer would make the two
	// legs different sizes without anyone noticing.
	Quantity float64 `json:"quantity"`

	// Price is only used for Limit orders.
	Price float64 `json:"price,omitempty"`

	// ReduceOnly guarantees an order can only shrink a position, never flip it.
	// Set on every closing perp order. Without it, a size mismatch between our
	// view and the exchange's turns a close into an accidental open on the
	// opposite side -- and an accidental LONG perp beside long spot is the
	// double-exposure case pkg/risk marks Critical.
	ReduceOnly bool `json:"reduce_only,omitempty"`

	// ClientOrderID is the idempotency key. Generated before the first attempt
	// and REUSED on every retry of the same logical order. Both venues reject a
	// duplicate ID, which is the desired behaviour: the second attempt fails
	// loudly instead of opening a second position.
	ClientOrderID string `json:"client_order_id"`

	// RefPrice is the market price observed immediately BEFORE sending. It is
	// the baseline slippage is measured against. Zero means slippage cannot be
	// computed for this order, and the result says so rather than reporting 0.
	RefPrice float64 `json:"ref_price,omitempty"`

	SentAt time.Time `json:"sent_at"`
}

// NewClientOrderID builds an idempotency key.
//
// Format: vega-<leg>-<unix-millis>-<6 random hex>. The prefix makes VEGA's
// orders identifiable in an exchange's trade history next to anything else the
// account does; the random suffix prevents a collision if two orders are
// generated in the same millisecond. Both venues cap this field's length
// (Binance at 36 characters), so it is kept short.
func NewClientOrderID(leg Leg) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("vega-%s-%d-%s", leg, time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// Validate catches the errors that are cheap to catch here and expensive to
// discover after one leg has already filled.
func (r OrderRequest) Validate() error {
	switch {
	case r.Symbol == "":
		return errors.New("execution: order has no symbol")
	case r.Side != Buy && r.Side != Sell:
		return fmt.Errorf("execution: invalid side %q", r.Side)
	case r.Quantity <= 0:
		return fmt.Errorf("execution: %s %s quantity must be positive, got %v", r.Symbol, r.Side, r.Quantity)
	case math.IsNaN(r.Quantity) || math.IsInf(r.Quantity, 0):
		return fmt.Errorf("execution: %s quantity is not a finite number", r.Symbol)
	case r.Type == Limit && r.Price <= 0:
		return fmt.Errorf("execution: %s limit order has no price", r.Symbol)
	case r.ClientOrderID == "":
		// Refusing here rather than generating one: an order without an
		// idempotency key cannot be safely retried, and the caller is the only
		// one who knows whether this is a first attempt or a retry.
		return errors.New("execution: order has no ClientOrderID; retries would not be idempotent")
	}
	return nil
}

// String renders an order for a log line.
func (r OrderRequest) String() string {
	ro := ""
	if r.ReduceOnly {
		ro = " reduce-only"
	}
	return fmt.Sprintf("%s %s %s %s %.8f%s id=%s", r.Venue, r.Leg, r.Side, r.Symbol, r.Quantity, ro, r.ClientOrderID)
}

// OrderStatus is what the venue says happened.
type OrderStatus string

const (
	StatusFilled          OrderStatus = "FILLED"
	StatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	StatusRejected        OrderStatus = "REJECTED"
	StatusCanceled        OrderStatus = "CANCELED"

	// StatusUnknown is the honest answer when the request was sent and no
	// usable response came back. It must never be collapsed into REJECTED --
	// treating an unknown outcome as a failure and retrying is how one intended
	// position becomes two.
	StatusUnknown OrderStatus = "UNKNOWN"
)

// Terminal reports whether this status can still change.
func (s OrderStatus) Terminal() bool {
	return s == StatusFilled || s == StatusRejected || s == StatusCanceled
}

// OrderResult is what actually happened, as the exchange reports it.
//
// Every price and quantity here is READ FROM THE VENUE. Nothing is carried
// over from the request except the identifiers. That separation is the point:
// the request is what we asked for, the result is what we got, and the previous
// bots' fatal habit was booking the first as if it were the second.
type OrderResult struct {
	Venue         string      `json:"venue"`
	Leg           Leg         `json:"leg"`
	Symbol        string      `json:"symbol"`
	Side          Side        `json:"side"`
	ClientOrderID string      `json:"client_order_id"`
	VenueOrderID  string      `json:"venue_order_id"`
	Status        OrderStatus `json:"status"`

	RequestedQty float64 `json:"requested_qty"`
	FilledQty    float64 `json:"filled_qty"`

	// AvgFillPrice is the volume-weighted price the venue actually gave us.
	// Zero with a non-zero FilledQty means the venue did not report it and the
	// caller must query the trade history rather than assume.
	AvgFillPrice float64 `json:"avg_fill_price"`

	// QuoteSpent is the settled quote-asset amount. Where the venue reports it
	// directly this is preferred over qty x price, because it already accounts
	// for a fill that swept several book levels.
	QuoteSpent float64 `json:"quote_spent"`

	// FeePaid and FeeAsset are the venue's own charge. This is the number that
	// eventually settles the argument the economics module can only estimate.
	// Fees paid in a discount token (BNB) arrive in that asset, so FeeAsset
	// must be checked before adding this to a USD total.
	FeePaid  float64 `json:"fee_paid"`
	FeeAsset string  `json:"fee_asset"`

	RefPrice   float64   `json:"ref_price,omitempty"`
	SentAt     time.Time `json:"sent_at"`
	ReportedAt time.Time `json:"reported_at"`

	// RawError is the venue's message when something went wrong, already
	// scrubbed of any signature.
	RawError string `json:"raw_error,omitempty"`
}

// Complete reports whether the full requested quantity filled.
//
// A tolerance is applied because step-size rounding can leave a dust
// difference that is not a partial fill in any meaningful sense.
func (r OrderResult) Complete() bool {
	if r.Status != StatusFilled {
		return false
	}
	if r.RequestedQty <= 0 {
		return false
	}
	return math.Abs(r.FilledQty-r.RequestedQty)/r.RequestedQty < 0.001
}

// SlippageBps is the measured cost of crossing the spread, in basis points,
// signed so that POSITIVE always means it cost us money.
//
// A buy filled above the reference price is positive. A sell filled below the
// reference price is positive. Ok is false when there is nothing to compare
// against -- and a caller that ignores ok and treats the zero as "no slippage"
// has reintroduced the exact optimism this project was built to remove.
func (r OrderResult) SlippageBps() (bps float64, ok bool) {
	if r.RefPrice <= 0 || r.AvgFillPrice <= 0 || r.FilledQty <= 0 {
		return 0, false
	}
	diff := (r.AvgFillPrice - r.RefPrice) / r.RefPrice * 10_000
	if r.Side == Sell {
		diff = -diff
	}
	return diff, true
}

// String renders a result for a log line.
func (r OrderResult) String() string {
	slip := "slippage UNMEASURED"
	if bps, ok := r.SlippageBps(); ok {
		slip = fmt.Sprintf("slippage %+.2f bps", bps)
	}
	s := fmt.Sprintf("%s %s %s %s %s: filled %.8f/%.8f @ %.8f, %s",
		r.Venue, r.Leg, r.Side, r.Symbol, r.Status, r.FilledQty, r.RequestedQty, r.AvgFillPrice, slip)
	if r.FeePaid != 0 {
		s += fmt.Sprintf(", fee %.8f %s", r.FeePaid, r.FeeAsset)
	}
	if r.RawError != "" {
		s += ", err: " + r.RawError
	}
	return s
}

// OrderPlacer can place orders. DELIBERATELY separate from AccountReader.
//
// A binary that only needs to observe imports AccountReader and is structurally
// incapable of trading -- there is no method on it that could. That is why
// phases 1 and 2 could run against a live account with no possibility of a bug
// moving money, and why cmd/monitor still cannot place an order today.
type OrderPlacer interface {
	Venue() string
	Mode() Mode

	// Place sends one order and returns what the venue reports.
	//
	// An error return does NOT mean nothing happened. If it wraps
	// ErrUnconfirmed the order may well have filled, and the caller must
	// reconcile against the account rather than retry.
	Place(ctx context.Context, req OrderRequest) (OrderResult, error)

	// Query re-reads an order by its client ID. This is the confirmation path
	// after an unconfirmed Place, and it is the reason ClientOrderID must be
	// generated before the first attempt: without it there is no handle to ask
	// about afterwards.
	Query(ctx context.Context, symbol, clientOrderID string, leg Leg) (OrderResult, error)
}

// NakedLeg describes a half-executed hedge. It is a value, not just an error,
// because it has to be journaled, shown on the dashboard, and acted on.
type NakedLeg struct {
	Venue  string    `json:"venue"`
	Symbol string    `json:"symbol"`
	At     time.Time `json:"at"`

	// Filled is the leg that went through and now carries unhedged exposure.
	Filled OrderResult `json:"filled"`

	// FailedLeg and FailReason describe what did not happen.
	FailedLeg  Leg    `json:"failed_leg"`
	FailReason string `json:"fail_reason"`

	// Remedy is the compensating order that flattens the exposure. It is
	// pre-built here, at the moment of detection, so the recovery path does not
	// depend on reconstructing intent later under time pressure.
	Remedy OrderRequest `json:"remedy"`
}

// Error implements error so a NakedLeg can be returned directly.
func (n NakedLeg) Error() string {
	return fmt.Sprintf("%v: %s %s -- %s leg failed (%s); filled leg is %.8f %s, remedy is %s",
		ErrNakedLeg, n.Venue, n.Symbol, n.FailedLeg, n.FailReason,
		n.Filled.FilledQty, n.Filled.Side, n.Remedy.Side)
}

// Unwrap lets errors.Is(err, ErrNakedLeg) work.
func (n NakedLeg) Unwrap() error { return ErrNakedLeg }

// NewNakedLeg builds the record and its compensating order together.
//
// The remedy is always a MARKET order on the opposite side of whatever filled,
// marked ReduceOnly on the perp leg. Speed matters more than price here: the
// exposure being closed is the exposure the entire strategy is designed not to
// have.
func NewNakedLeg(filled OrderResult, failedLeg Leg, reason string) NakedLeg {
	remedy := OrderRequest{
		Venue:         filled.Venue,
		Leg:           filled.Leg,
		Symbol:        filled.Symbol,
		Side:          filled.Side.Opposite(),
		Type:          Market,
		Quantity:      filled.FilledQty,
		ReduceOnly:    filled.Leg == PerpLeg,
		ClientOrderID: NewClientOrderID(filled.Leg),
	}
	return NakedLeg{
		Venue:      filled.Venue,
		Symbol:     filled.Symbol,
		At:         time.Now().UTC(),
		Filled:     filled,
		FailedLeg:  failedLeg,
		FailReason: strings.TrimSpace(reason),
		Remedy:     remedy,
	}
}
