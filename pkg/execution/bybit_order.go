package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BybitOrderShapesVerified is false for the same reason its Binance twin is:
// nothing here has been checked against a real response.
const BybitOrderShapesVerified = false

// BYBIT'S DIFFERENCES FROM BINANCE, AND WHY EACH ONE MATTERS
//
//  1. The create response contains ONLY {orderId, orderLinkId}. No fill price,
//     no filled quantity, no fee. Binance hands you the fill in the same
//     response; Bybit does not. So every Place here MUST be followed by a
//     query, and a caller that books the position from the create response
//     alone has booked a position at price zero.
//
//  2. Orders are a JSON BODY, not query parameters, and the signature covers
//     that body byte for byte. Re-serialising the struct after signing
//     invalidates the signature -- so the body is marshalled once and the same
//     string is both signed and sent.
//
//  3. The idempotency key is called orderLinkId, not clientOrderId.
//
//  4. Spot and linear use DIFFERENT FIELD NAMES for the same concept. Linear
//     publishes lotSizeFilter.qtyStep; spot publishes lotSizeFilter.basePrecision.
//     Reading only qtyStep gives a zero step on spot, and a zero step means no
//     rounding happens at all.
//
//  5. THE DANGEROUS ONE. A spot MARKET BUY on Bybit interprets qty as the
//     QUOTE amount by default, not the base amount. Sending qty="0.001" for
//     BTCUSDT buys one tenth of a cent of Bitcoin, not 0.001 BTC -- a factor of
//     roughly 64,000,000 off, in the direction that looks like a tiny harmless
//     fill rather than an error. The perp leg meanwhile sells 0.001 BTC
//     correctly. Result: an almost entirely naked short.
//
//     marketUnit=baseCoin fixes it, and is set unconditionally below.

const bybitMarketUnitBase = "baseCoin"

// --- instruments-info -------------------------------------------------------

// bybitInstrument covers both categories. The two lot-size shapes are unioned
// here rather than split, so the parser cannot silently pick the wrong one.
type bybitInstrument struct {
	Symbol        string `json:"symbol"`
	Status        string `json:"status"`
	LotSizeFilter struct {
		// linear
		QtyStep     string `json:"qtyStep"`
		MinOrderQty string `json:"minOrderQty"`
		MaxOrderQty string `json:"maxOrderQty"`
		// spot
		BasePrecision string `json:"basePrecision"`
		MinOrderAmt   string `json:"minOrderAmt"`
		// linear only, and often absent
		MinNotionalValue string `json:"minNotionalValue"`
	} `json:"lotSizeFilter"`
	PriceFilter struct {
		TickSize string `json:"tickSize"`
	} `json:"priceFilter"`
}

type bybitInstrumentResult struct {
	Category string            `json:"category"`
	List     []bybitInstrument `json:"list"`
}

// --- order results ----------------------------------------------------------

// bybitCreateResult is ALL you get back from /v5/order/create.
type bybitCreateResult struct {
	OrderID     string `json:"orderId"`
	OrderLinkID string `json:"orderLinkId"`
}

// bybitOrderRecord is one order from /v5/order/realtime or /v5/order/history.
//
// AvgPrice can be an empty string on an unfilled order -- the same trap as
// liqPrice in bybit_account.go, where "" means unknown and parsing it as a
// number yields a confident zero.
//
// CumExecFee is the settled fee and is reported here, which is one thing Bybit
// does better than Binance futures.
type bybitOrderRecord struct {
	OrderID      string `json:"orderId"`
	OrderLinkID  string `json:"orderLinkId"`
	Symbol       string `json:"symbol"`
	Side         string `json:"side"`
	OrderStatus  string `json:"orderStatus"`
	Qty          string `json:"qty"`
	AvgPrice     string `json:"avgPrice"`
	CumExecQty   string `json:"cumExecQty"`
	CumExecValue string `json:"cumExecValue"`
	CumExecFee   string `json:"cumExecFee"`
	RejectReason string `json:"rejectReason"`
	UpdatedTime  string `json:"updatedTime"`
}

type bybitOrderListResult struct {
	Category string             `json:"category"`
	List     []bybitOrderRecord `json:"list"`
}

// bybitStatus maps Bybit's order states onto ours.
//
// "Untriggered" and "New" are NOT terminal and must not be read as rejections.
// "Deactivated" and "Rejected" are refusals. Anything unrecognised becomes
// StatusUnknown rather than being guessed at -- an unknown state that gets
// guessed as REJECTED invites a retry against an order that may be live.
func bybitStatus(s string) OrderStatus {
	switch s {
	case "Filled":
		return StatusFilled
	case "PartiallyFilled":
		return StatusPartiallyFilled
	case "Cancelled", "PartiallyFilledCanceled", "Deactivated":
		return StatusCanceled
	case "Rejected":
		return StatusRejected
	case "New", "Created", "Untriggered", "Triggered":
		return StatusUnknown
	default:
		return StatusUnknown
	}
}

// --- the trader -------------------------------------------------------------

// BybitTrader places orders on Bybit v5.
type BybitTrader struct {
	creds Credentials
	http  *http.Client
	base  string

	DryRun bool

	filters map[string]SymbolFilters
}

// NewBybitTrader builds an order placer, refusing read-only credentials.
func NewBybitTrader(creds Credentials) (*BybitTrader, error) {
	if !creds.CanTrade() {
		return nil, fmt.Errorf("%w: BybitTrader requires Trade capability, got %s",
			ErrReadOnly, creds.Capability)
	}
	return &BybitTrader{
		creds:   creds,
		http:    &http.Client{Timeout: 20 * time.Second},
		base:    BybitBase(creds.Mode),
		filters: make(map[string]SymbolFilters, 8),
	}, nil
}

// Venue implements OrderPlacer.
func (b *BybitTrader) Venue() string { return "bybit" }

// Mode implements OrderPlacer.
func (b *BybitTrader) Mode() Mode { return b.creds.Mode }

// LiveTradingReady refuses until the shapes above have been checked.
func (b *BybitTrader) LiveTradingReady() error {
	if !BybitOrderShapesVerified {
		return errors.New("execution: bybit order response shapes are UNVERIFIED -- " +
			"place a testnet order and confirm the JSON before trusting fill prices")
	}
	return nil
}

// bybitCategory maps a leg to Bybit's category parameter.
func bybitCategory(leg Leg) string {
	if leg == PerpLeg {
		return "linear"
	}
	return "spot"
}

// Filters fetches a symbol's trading rules.
func (b *BybitTrader) Filters(ctx context.Context, symbol string, leg Leg) (SymbolFilters, error) {
	key := string(leg) + ":" + symbol
	if f, ok := b.filters[key]; ok {
		return f, nil
	}

	q := url.Values{}
	q.Set("category", bybitCategory(leg))
	q.Set("symbol", symbol)

	var res bybitInstrumentResult
	if err := b.getPublic(ctx, "/v5/market/instruments-info", q, &res); err != nil {
		return SymbolFilters{}, err
	}
	for _, in := range res.List {
		if !strings.EqualFold(in.Symbol, symbol) {
			continue
		}
		f := SymbolFilters{Venue: "bybit", Leg: leg, Symbol: in.Symbol, FetchedAt: time.Now().UTC()}

		// Try both spellings. Linear uses qtyStep, spot uses basePrecision.
		for _, raw := range []string{in.LotSizeFilter.QtyStep, in.LotSizeFilter.BasePrecision} {
			if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
				f.StepSize = v
				break
			}
		}
		f.MinQty, _ = strconv.ParseFloat(in.LotSizeFilter.MinOrderQty, 64)
		f.TickSize, _ = strconv.ParseFloat(in.PriceFilter.TickSize, 64)

		// minOrderAmt (spot) and minNotionalValue (linear) are both quote-side
		// floors. Take whichever is present and larger.
		for _, raw := range []string{in.LotSizeFilter.MinOrderAmt, in.LotSizeFilter.MinNotionalValue} {
			if v, err := strconv.ParseFloat(raw, 64); err == nil && v > f.MinNotional {
				f.MinNotional = v
			}
		}

		if f.StepSize <= 0 {
			return SymbolFilters{}, fmt.Errorf(
				"execution: bybit %s %s reported neither qtyStep nor basePrecision; "+
					"refusing to size an order blind", leg, symbol)
		}
		b.filters[key] = f
		return f, nil
	}
	return SymbolFilters{}, fmt.Errorf("execution: bybit %s has no symbol %s", leg, symbol)
}

// --- placing ----------------------------------------------------------------

// Place implements OrderPlacer.
//
// Returns a result whose Status is almost always StatusUnknown even on
// success, because the create response carries no fill data. The caller MUST
// call Query to learn what actually happened. That is not a shortcoming worked
// around -- it is the venue's contract, made visible.
func (b *BybitTrader) Place(ctx context.Context, req OrderRequest) (OrderResult, error) {
	if err := req.Validate(); err != nil {
		return OrderResult{}, err
	}

	res := OrderResult{
		Venue:         "bybit",
		Leg:           req.Leg,
		Symbol:        req.Symbol,
		Side:          req.Side,
		ClientOrderID: req.ClientOrderID,
		RequestedQty:  req.Quantity,
		RefPrice:      req.RefPrice,
		SentAt:        time.Now().UTC(),
		Status:        StatusUnknown,
	}

	if b.DryRun {
		res.RawError = "dry run"
		return res, fmt.Errorf("%w: %s", ErrDryRun, req)
	}

	// Bybit capitalises sides as Buy/Sell, not BUY/SELL. Sending the wrong
	// case is retCode 10001 with a message that does not name the field.
	side := "Buy"
	if req.Side == Sell {
		side = "Sell"
	}
	orderType := "Market"
	if req.Type == Limit {
		orderType = "Limit"
	}

	body := map[string]any{
		"category":    bybitCategory(req.Leg),
		"symbol":      req.Symbol,
		"side":        side,
		"orderType":   orderType,
		"qty":         strconv.FormatFloat(req.Quantity, 'f', -1, 64),
		"orderLinkId": req.ClientOrderID,
	}
	if req.Leg == SpotLeg {
		// See the header comment. Without this a spot market buy spends
		// req.Quantity of USDT instead of buying req.Quantity of the base.
		body["marketUnit"] = bybitMarketUnitBase
	}
	if req.ReduceOnly && req.Leg == PerpLeg {
		body["reduceOnly"] = true
	}
	if req.Type == Limit {
		body["price"] = strconv.FormatFloat(req.Price, 'f', -1, 64)
		body["timeInForce"] = "GTC"
	}

	// Marshal ONCE. The signature covers these exact bytes; re-marshalling
	// after signing would reorder the map and invalidate it.
	raw, err := json.Marshal(body)
	if err != nil {
		return res, fmt.Errorf("execution: encoding bybit order body: %w", err)
	}

	httpReq, err := SignBybit(b.creds, http.MethodPost, b.base, "/v5/order/create", nil, string(raw))
	if err != nil {
		return res, err
	}
	httpReq = httpReq.WithContext(ctx)

	resp, err := b.http.Do(httpReq)
	if err != nil {
		res.RawError = "transport error: " + err.Error()
		return res, fmt.Errorf("%w: bybit %s %s (id %s): %v",
			ErrUnconfirmed, req.Leg, req.Symbol, req.ClientOrderID, err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res.ReportedAt = time.Now().UTC()

	if resp.StatusCode != http.StatusOK {
		res.RawError = truncate(string(payload), 300)
		return res, fmt.Errorf("%w: bybit HTTP %d on %s %s: %s",
			ErrUnconfirmed, resp.StatusCode, req.Leg, req.Symbol, res.RawError)
	}

	var env bybitEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		res.RawError = truncate(string(payload), 300)
		return res, fmt.Errorf("%w: parsing bybit create response: %v", ErrUnconfirmed, err)
	}

	if env.RetCode != 0 {
		res.RawError = fmt.Sprintf("retCode %d: %s", env.RetCode, env.RetMsg)

		// 10001 and 110xxx are validation refusals: nothing was placed.
		// Anything else inside a 200 is ambiguous and must be confirmed, not
		// assumed failed.
		if env.RetCode == 10001 || (env.RetCode >= 110000 && env.RetCode < 120000) {
			res.Status = StatusRejected
			return res, fmt.Errorf("execution: bybit rejected %s %s: %s",
				req.Leg, req.Symbol, res.RawError)
		}
		return res, fmt.Errorf("%w: bybit %s %s: %s", ErrUnconfirmed, req.Leg, req.Symbol, res.RawError)
	}

	var created bybitCreateResult
	if err := json.Unmarshal(env.Result, &created); err != nil {
		return res, fmt.Errorf("%w: parsing bybit create result: %v", ErrUnconfirmed, err)
	}
	res.VenueOrderID = created.OrderID

	// Accepted, and that is genuinely all we know. Status stays UNKNOWN and
	// the fill fields stay zero until Query says otherwise.
	return res, nil
}

// Query implements OrderPlacer.
//
// Checks the live-order endpoint first, then history. A market order that
// filled instantly is often already out of /v5/order/realtime by the time this
// runs, and concluding "not found, therefore never placed" from that would be
// the worst possible inference.
func (b *BybitTrader) Query(ctx context.Context, symbol, clientOrderID string, leg Leg) (OrderResult, error) {
	res := OrderResult{
		Venue: "bybit", Leg: leg, Symbol: symbol,
		ClientOrderID: clientOrderID, Status: StatusUnknown,
		ReportedAt: time.Now().UTC(),
	}

	for _, path := range []string{"/v5/order/realtime", "/v5/order/history"} {
		q := url.Values{}
		q.Set("category", bybitCategory(leg))
		q.Set("symbol", symbol)
		q.Set("orderLinkId", clientOrderID)

		var list bybitOrderListResult
		err := b.getSignedOrder(ctx, path, q, &list)
		if err != nil {
			// Try the other endpoint before giving up.
			res.RawError = err.Error()
			continue
		}
		for _, o := range list.List {
			if o.OrderLinkID != clientOrderID {
				continue
			}
			res.VenueOrderID = o.OrderID
			res.Status = bybitStatus(o.OrderStatus)

			// Side comes from the venue, not from the caller. Query is also the
			// path used to find out what an UNCONFIRMED order did, and in that
			// case the caller may have nothing but a client ID.
			if strings.EqualFold(o.Side, "Sell") {
				res.Side = Sell
			} else if strings.EqualFold(o.Side, "Buy") {
				res.Side = Buy
			}

			res.RequestedQty, _ = strconv.ParseFloat(o.Qty, 64)
			res.FilledQty, _ = strconv.ParseFloat(o.CumExecQty, 64)
			res.QuoteSpent, _ = strconv.ParseFloat(o.CumExecValue, 64)
			res.FeePaid, _ = strconv.ParseFloat(o.CumExecFee, 64)

			// Bybit charges the fee in the received asset: the base coin on a
			// buy, USDT on a sell. Naming it here stops the reconciler adding
			// BTC to dollars.
			if leg == SpotLeg && res.Side == Buy {
				res.FeeAsset = strings.TrimSuffix(symbol, "USDT")
			} else {
				res.FeeAsset = "USDT"
			}

			// avgPrice is "" on an unfilled order. Derive from value/qty
			// instead of parsing "" into a confident zero.
			if v, err := strconv.ParseFloat(o.AvgPrice, 64); err == nil && v > 0 {
				res.AvgFillPrice = v
			} else if res.FilledQty > 0 && res.QuoteSpent > 0 {
				res.AvgFillPrice = res.QuoteSpent / res.FilledQty
			}

			if o.RejectReason != "" && o.RejectReason != "EC_NoError" {
				res.RawError = o.RejectReason
			} else {
				res.RawError = ""
			}
			return res, nil
		}
	}

	// Neither endpoint knew about it. That is NOT proof it does not exist --
	// it could be a propagation delay -- so this stays unconfirmed.
	return res, fmt.Errorf("%w: bybit has no order %s on %s %s", ErrUnconfirmed, clientOrderID, leg, symbol)
}

// --- transport --------------------------------------------------------------

// getPublic reads an unauthenticated v5 endpoint.
func (b *BybitTrader) getPublic(ctx context.Context, path string, q url.Values, out any) error {
	full := b.base + path
	if len(q) > 0 {
		full += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("execution: bybit GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("execution: bybit %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	return b.unwrap(body, path, out)
}

// getSignedOrder reads an authenticated v5 endpoint. A GET, so read-only
// credentials can call it -- confirming an order must never require the
// ability to place one.
func (b *BybitTrader) getSignedOrder(ctx context.Context, path string, q url.Values, out any) error {
	req, err := SignBybit(b.creds, http.MethodGet, b.base, path, q, "")
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("execution: bybit GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("execution: bybit %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	return b.unwrap(body, path, out)
}

// unwrap checks retCode before decoding. An HTTP 200 carrying retCode 10004
// is a failure, and decoding its empty result into a struct produces a
// zero-valued success.
func (b *BybitTrader) unwrap(body []byte, path string, out any) error {
	var env bybitEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("execution: parsing bybit %s envelope: %w", path, err)
	}
	if env.RetCode != 0 {
		return fmt.Errorf("execution: bybit %s retCode=%d: %s", path, env.RetCode, env.RetMsg)
	}
	if len(env.Result) == 0 {
		return fmt.Errorf("execution: bybit %s returned an empty result", path)
	}
	return json.Unmarshal(env.Result, out)
}
