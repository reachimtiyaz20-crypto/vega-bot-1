package execution

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Balance is one asset's holding on one venue.
type Balance struct {
	Venue  string  `json:"venue"`
	Asset  string  `json:"asset"`
	Free   float64 `json:"free"`
	Locked float64 `json:"locked"`
}

// Total is free plus locked.
func (b Balance) Total() float64 { return b.Free + b.Locked }

// PerpPosition is an open perpetual position AS THE EXCHANGE SEES IT.
//
// Every field here is read from the venue, not computed by VEGA. That is the
// point: the previous bots' fatal habit was believing their own arithmetic
// over the exchange's ledger. Where VEGA's number and this number disagree,
// this one is right and the disagreement is a bug worth stopping for.
type PerpPosition struct {
	Venue  string `json:"venue"`
	Symbol string `json:"symbol"`

	// PositionAmt is signed: negative for a short. A cash-and-carry holds a
	// SHORT perp against long spot, so this should be negative for every
	// position VEGA opens. A positive value means something went wrong --
	// wrong side, or a leg that failed to close.
	PositionAmt float64 `json:"position_amt"`

	EntryPrice float64 `json:"entry_price"`
	MarkPrice  float64 `json:"mark_price"`

	// LiquidationPrice is the exchange's own number, and it is the single most
	// important field in this package. "Market neutral" is true right up until
	// the short leg is liquidated, at which point you are suddenly long spot
	// into a falling market with no hedge. During the March 2024 BTC run from
	// $60K to $73K, delta-neutral traders were liquidated on exactly this.
	//
	// Zero means the venue reports no liquidation price, usually because the
	// position is flat. Treat zero as UNKNOWN, never as "safe".
	LiquidationPrice float64 `json:"liquidation_price"`

	// UnrealizedPnl on the perp leg alone. For a hedged position this should
	// be roughly the mirror of the spot leg's move. It is not profit -- it is
	// half of a pair.
	UnrealizedPnl float64 `json:"unrealized_pnl"`

	Leverage       float64   `json:"leverage"`
	IsolatedMargin float64   `json:"isolated_margin"`
	MarginType     string    `json:"margin_type"`
	ObservedAt     time.Time `json:"observed_at"`
}

// NotionalUSD is the position's size at the current mark.
func (p PerpPosition) NotionalUSD() float64 {
	return math.Abs(p.PositionAmt) * p.MarkPrice
}

// IsShort reports whether this is a short position, which is what a
// cash-and-carry requires on the perp leg.
func (p PerpPosition) IsShort() bool { return p.PositionAmt < 0 }

// IsFlat reports whether the position is closed.
func (p PerpPosition) IsFlat() bool { return p.PositionAmt == 0 }

// DistanceToLiquidationPct is how far price must move, as a percentage of the
// mark, before this position is liquidated.
//
// For a SHORT, liquidation is ABOVE the mark, so the distance is positive when
// the liquidation price is higher. Returns +Inf when the venue reports no
// liquidation price -- callers must treat that as unknown rather than infinite
// safety, which is why HasLiquidationPrice exists separately.
func (p PerpPosition) DistanceToLiquidationPct() float64 {
	if p.LiquidationPrice <= 0 || p.MarkPrice <= 0 || p.IsFlat() {
		return math.Inf(1)
	}
	return math.Abs(p.LiquidationPrice-p.MarkPrice) / p.MarkPrice * 100
}

// HasLiquidationPrice reports whether the venue actually gave us one.
func (p PerpPosition) HasLiquidationPrice() bool {
	return p.LiquidationPrice > 0 && !p.IsFlat()
}

// FundingPayment is one settled funding transfer, read from the venue's own
// income ledger.
//
// THIS IS THE GROUND TRUTH FOR REVENUE. The previous funding bot got exactly
// one thing right: it read AddFundingPayment from Binance's FUNDING_FEE income
// endpoint rather than estimating from the published rate. That half was
// correct and is preserved here. It was the cost side that did not exist.
//
// Amount is SIGNED. Negative means the position PAID funding, which happens
// whenever the rate flips against the short leg. A sum over these can and must
// be able to go negative.
type FundingPayment struct {
	Venue    string    `json:"venue"`
	Symbol   string    `json:"symbol"`
	Amount   float64   `json:"amount"`
	Asset    string    `json:"asset"`
	SettleAt time.Time `json:"settle_at"`
	TranID   string    `json:"tran_id"`
}

// AccountSnapshot is everything read from one venue at one moment.
type AccountSnapshot struct {
	Venue      string         `json:"venue"`
	Mode       Mode           `json:"mode"`
	ObservedAt time.Time      `json:"observed_at"`
	Balances   []Balance      `json:"balances"`
	Positions  []PerpPosition `json:"positions"`

	// WalletUSD and AvailableUSD are the venue's own view of the futures
	// wallet. Reconciliation compares VEGA's expected balance against this.
	WalletUSD     float64 `json:"wallet_usd"`
	AvailableUSD  float64 `json:"available_usd"`
	UnrealizedUSD float64 `json:"unrealized_usd"`
}

// OpenPositions returns only positions that are not flat.
func (s AccountSnapshot) OpenPositions() []PerpPosition {
	out := make([]PerpPosition, 0, len(s.Positions))
	for _, p := range s.Positions {
		if !p.IsFlat() {
			out = append(out, p)
		}
	}
	return out
}

// AssetBalance finds one asset, or a zero Balance if absent.
func (s AccountSnapshot) AssetBalance(asset string) Balance {
	for _, b := range s.Balances {
		if b.Asset == asset {
			return b
		}
	}
	return Balance{Venue: s.Venue, Asset: asset}
}

// AccountReader is READ-ONLY access to an account.
//
// Deliberately a separate interface from OrderPlacer. A binary can import this
// and reconcile a ledger without having any ability to trade -- and phases 1
// and 2 do exactly that, with read-only keys, so no bug in this code can move
// money.
type AccountReader interface {
	// Venue names the exchange.
	Venue() string

	// Mode reports mainnet or testnet, so a caller can never be confused about
	// whether it is looking at real balances.
	Mode() Mode

	// Snapshot reads balances and open positions.
	Snapshot(ctx context.Context) (AccountSnapshot, error)

	// FundingSince reads settled funding payments from the venue's income
	// ledger. This is the reconciled revenue source.
	FundingSince(ctx context.Context, since time.Time) ([]FundingPayment, error)
}

// Divergence records a disagreement between VEGA's arithmetic and the
// exchange's ledger.
//
// The rule from the brief is that PnL comes from reconciled balances. That
// cannot mean "trust the exchange and discard our own number" -- if the two
// disagree, one of them contains a bug, and the disagreement is the signal.
// So both are kept, the gap is computed, and anything beyond tolerance is
// journaled loudly rather than silently overwritten.
type Divergence struct {
	Venue      string    `json:"venue"`
	Symbol     string    `json:"symbol,omitempty"`
	Field      string    `json:"field"`
	VegaValue  float64   `json:"vega_value"`
	VenueValue float64   `json:"venue_value"`
	DiffAbs    float64   `json:"diff_abs"`
	DiffPct    float64   `json:"diff_pct"`
	Tolerance  float64   `json:"tolerance"`
	Material   bool      `json:"material"`
	ObservedAt time.Time `json:"observed_at"`
}

// Compare builds a Divergence between a computed value and the venue's value.
//
// tolerancePct is how far apart they may be before it is called material.
// Small gaps are expected: rounding, a settlement landing between two reads,
// a fee credited a moment later. Large gaps are not.
func Compare(venue, symbol, field string, vega, venueVal, tolerancePct float64) Divergence {
	d := Divergence{
		Venue:      venue,
		Symbol:     symbol,
		Field:      field,
		VegaValue:  vega,
		VenueValue: venueVal,
		DiffAbs:    vega - venueVal,
		Tolerance:  tolerancePct,
		ObservedAt: time.Now().UTC(),
	}
	if venueVal != 0 {
		d.DiffPct = (vega - venueVal) / math.Abs(venueVal) * 100
	} else if vega != 0 {
		// Venue says zero, we say something. That is always material -- it
		// usually means a position we think exists does not, or vice versa.
		d.DiffPct = math.Inf(1)
	}
	d.Material = math.Abs(d.DiffPct) > tolerancePct
	return d
}

// String renders a divergence for a log line.
func (d Divergence) String() string {
	sev := "ok"
	if d.Material {
		sev = "MATERIAL"
	}
	return fmt.Sprintf("%s %s %s/%s: vega=%.8f venue=%.8f diff=%.8f (%.3f%%, tolerance %.3f%%)",
		sev, d.Venue, d.Symbol, d.Field, d.VegaValue, d.VenueValue, d.DiffAbs, d.DiffPct, d.Tolerance)
}
