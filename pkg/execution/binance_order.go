package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BinanceOrderShapesVerified is false until someone has placed a real testnet
// order and confirmed every field below against the actual response.
//
// The JSON shapes here are written from Binance's published documentation, not
// from a response anyone in this project has seen. Documentation and reality
// diverge -- that is not cynicism, it is the reason /fapi/v1/bookTicker cost us
// an afternoon earlier in this build when the documented path returned HTML.
//
// Flip this to true only after a testnet order has been placed and its raw
// response inspected. Until then LiveTradingReady() refuses.
const BinanceOrderShapesVerified = false

// --- symbol filters ---------------------------------------------------------

// SymbolFilters are an exchange's rules about what quantities are even
// sendable for a symbol.
//
// These matter more than they look. An order rejected for violating a step
// size is not a harmless failure: if it is the SECOND leg, the first leg has
// already filled and the position is naked. Filters must therefore be applied
// BEFORE either order is sent, not discovered by rejection afterwards.
type SymbolFilters struct {
	Venue  string `json:"venue"`
	Leg    Leg    `json:"leg"`
	Symbol string `json:"symbol"`

	// StepSize is the quantity increment. 0.001 means 0.0015 is invalid.
	StepSize float64 `json:"step_size"`

	// MinQty and MinNotional are the floors. MinNotional is the one that bites
	// on a $100 test account: Binance futures requires $5 (sometimes $20) of
	// notional, so a position sized below it is unplaceable regardless of how
	// good the funding rate looks.
	MinQty      float64 `json:"min_qty"`
	MinNotional float64 `json:"min_notional"`

	TickSize float64 `json:"tick_size"`

	FetchedAt time.Time `json:"fetched_at"`
}

// RoundQuantity floors a quantity to the step size.
//
// FLOOR, never round-to-nearest. Rounding up can push a quantity past the
// balance available, and a leg that fails for insufficient funds after the
// other leg filled is the naked-leg case. Rounding down at worst leaves dust.
//
// The 1e-9 nudge before flooring handles binary representation: 0.3/0.1 is
// 2.9999999999999996 in IEEE-754, and flooring that gives 2 instead of 3,
// silently halving an order.
func (f SymbolFilters) RoundQuantity(q float64) float64 {
	if f.StepSize <= 0 {
		return q
	}
	steps := math.Floor(q/f.StepSize + 1e-9)
	rounded := steps * f.StepSize

	// Re-round to the step's own decimal precision. Multiplying a float step
	// back out reintroduces trailing garbage (0.30000000000000004) that some
	// venues reject outright.
	if dp := decimalsOf(f.StepSize); dp >= 0 {
		pow := math.Pow(10, float64(dp))
		rounded = math.Round(rounded*pow) / pow
	}
	return rounded
}

// Acceptable reports whether a quantity can actually be sent, and why not.
func (f SymbolFilters) Acceptable(qty, price float64) error {
	if qty <= 0 {
		return fmt.Errorf("execution: %s %s quantity %.8f rounds to zero at step %.8f",
			f.Symbol, f.Leg, qty, f.StepSize)
	}
	if f.MinQty > 0 && qty < f.MinQty {
		return fmt.Errorf("execution: %s %s quantity %.8f below venue minimum %.8f",
			f.Symbol, f.Leg, qty, f.MinQty)
	}
	if f.MinNotional > 0 && price > 0 && qty*price < f.MinNotional {
		return fmt.Errorf("execution: %s %s notional $%.2f below venue minimum $%.2f -- "+
			"this position is too small to place on this venue",
			f.Symbol, f.Leg, qty*price, f.MinNotional)
	}
	return nil
}

// decimalsOf counts decimal places in a step size like 0.001 -> 3.
func decimalsOf(step float64) int {
	s := strconv.FormatFloat(step, 'f', -1, 64)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return 0
	}
	return len(s) - i - 1
}

// MatchQuantity returns a quantity valid on BOTH legs.
//
// THIS IS NOT COSMETIC. Spot and futures publish DIFFERENT step sizes for the
// same asset -- on Binance, BTCUSDT spot steps at 0.00001 while the perp steps
// at 0.001. Sizing each leg against its own filter independently produces two
// legs of different sizes, which is a partially hedged position that looks
// fully hedged in every log line.
//
// So: floor against the COARSER step, then confirm the result is acceptable on
// both. The residual is dust, deliberately left unhedged rather than papered
// over.
func MatchQuantity(spot, perp SymbolFilters, desired, price float64) (float64, error) {
	coarse := spot
	if perp.StepSize > spot.StepSize {
		coarse = perp
	}

	qty := coarse.RoundQuantity(desired)

	if err := spot.Acceptable(qty, price); err != nil {
		return 0, err
	}
	if err := perp.Acceptable(qty, price); err != nil {
		return 0, err
	}

	// Belt and braces: the coarser step should already satisfy the finer one,
	// but if the two steps are not multiples of each other (rare, but it
	// happens on newly listed symbols) this catches it rather than sending
	// mismatched legs.
	if spot.RoundQuantity(qty) != qty || perp.RoundQuantity(qty) != qty {
		return 0, fmt.Errorf("execution: %s step sizes are incompatible (spot %.8f, perp %.8f); "+
			"refusing to send legs that would differ in size", spot.Symbol, spot.StepSize, perp.StepSize)
	}
	return qty, nil
}

// --- binance exchangeInfo ---------------------------------------------------

type binFilter struct {
	FilterType  string `json:"filterType"`
	StepSize    string `json:"stepSize"`
	MinQty      string `json:"minQty"`
	TickSize    string `json:"tickSize"`
	MinNotional string `json:"minNotional"`
	Notional    string `json:"notional"`
}

type binSymbolInfo struct {
	Symbol  string      `json:"symbol"`
	Status  string      `json:"status"`
	Filters []binFilter `json:"filters"`
}

type binExchangeInfo struct {
	Symbols []binSymbolInfo `json:"symbols"`
}

// --- the trader -------------------------------------------------------------

// BinanceTrader places orders on Binance. It holds Trade credentials, which is
// the only difference from BinanceAccount -- and the reason it is a separate
// type rather than more methods on the reader.
type BinanceTrader struct {
	creds       Credentials
	http        *http.Client
	futuresBase string
	spotBase    string

	// DryRun refuses to send anything. Every code path still runs -- signing,
	// filter checks, request construction -- so a dry run exercises everything
	// except the part that costs money.
	DryRun bool

	filters map[string]SymbolFilters
}

// NewBinanceTrader builds an order placer.
//
// Refuses read-only credentials up front. The signer would refuse each POST
// individually anyway, but failing here means a misconfiguration is caught at
// startup rather than at the moment the first leg needs its partner.
func NewBinanceTrader(creds Credentials) (*BinanceTrader, error) {
	if !creds.CanTrade() {
		return nil, fmt.Errorf("%w: BinanceTrader requires Trade capability, got %s",
			ErrReadOnly, creds.Capability)
	}
	return &BinanceTrader{
		creds:       creds,
		http:        &http.Client{Timeout: 20 * time.Second},
		futuresBase: BinanceFuturesBase(creds.Mode),
		spotBase:    BinanceSpotBase(creds.Mode),
		filters:     make(map[string]SymbolFilters, 8),
	}, nil
}

// Venue implements OrderPlacer.
func (b *BinanceTrader) Venue() string { return "binance" }

// Mode implements OrderPlacer.
func (b *BinanceTrader) Mode() Mode { return b.creds.Mode }

// LiveTradingReady reports whether this venue's order shapes have been
// verified against a real response. The live manager calls it before mainnet.
func (b *BinanceTrader) LiveTradingReady() error {
	if !BinanceOrderShapesVerified {
		return errors.New("execution: binance order response shapes are UNVERIFIED -- " +
			"place a testnet order and confirm the JSON before trusting fill prices")
	}
	return nil
}

// Filters fetches and caches a symbol's trading rules for one leg.
//
// exchangeInfo is a public endpoint, so this needs no signature. It is cached
// because the futures payload is roughly 2 MB and re-fetching per order on a
// 1 GB box is wasteful -- but it is cached per process, not persisted: filters
// do change, and a stale minNotional causes a rejection at the worst moment.
func (b *BinanceTrader) Filters(ctx context.Context, symbol string, leg Leg) (SymbolFilters, error) {
	key := string(leg) + ":" + symbol
	if f, ok := b.filters[key]; ok {
		return f, nil
	}

	host, path := b.spotBase, "/api/v3/exchangeInfo"
	if leg == PerpLeg {
		host, path = b.futuresBase, "/fapi/v1/exchangeInfo"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		host+path+"?symbol="+url.QueryEscape(symbol), nil)
	if err != nil {
		return SymbolFilters{}, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return SymbolFilters{}, fmt.Errorf("execution: binance exchangeInfo %s: %w", leg, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return SymbolFilters{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return SymbolFilters{}, fmt.Errorf("execution: binance exchangeInfo %s %s: HTTP %d: %s",
			leg, symbol, resp.StatusCode, truncate(string(body), 200))
	}

	var info binExchangeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return SymbolFilters{}, fmt.Errorf("execution: parsing binance exchangeInfo: %w", err)
	}

	for _, s := range info.Symbols {
		if !strings.EqualFold(s.Symbol, symbol) {
			continue
		}
		f := SymbolFilters{Venue: "binance", Leg: leg, Symbol: s.Symbol, FetchedAt: time.Now().UTC()}
		for _, flt := range s.Filters {
			switch flt.FilterType {
			case "LOT_SIZE", "MARKET_LOT_SIZE":
				// MARKET_LOT_SIZE, where present, is the stricter one for a
				// market order and must win.
				if v, err := strconv.ParseFloat(flt.StepSize, 64); err == nil && v > 0 {
					if f.StepSize == 0 || flt.FilterType == "MARKET_LOT_SIZE" {
						f.StepSize = v
					}
				}
				if v, err := strconv.ParseFloat(flt.MinQty, 64); err == nil && v > f.MinQty {
					f.MinQty = v
				}
			case "PRICE_FILTER":
				f.TickSize, _ = strconv.ParseFloat(flt.TickSize, 64)
			case "MIN_NOTIONAL", "NOTIONAL":
				// Spot calls the field "minNotional" under filterType
				// MIN_NOTIONAL, then renamed the filter to NOTIONAL and kept
				// the field. Futures uses "notional". Try all of them.
				for _, raw := range []string{flt.MinNotional, flt.Notional} {
					if v, err := strconv.ParseFloat(raw, 64); err == nil && v > f.MinNotional {
						f.MinNotional = v
					}
				}
			}
		}
		if f.StepSize <= 0 {
			return SymbolFilters{}, fmt.Errorf(
				"execution: binance %s %s reported no usable step size; refusing to size an order blind",
				leg, symbol)
		}
		b.filters[key] = f
		return f, nil
	}
	return SymbolFilters{}, fmt.Errorf("execution: binance %s has no symbol %s", leg, symbol)
}

// --- order responses --------------------------------------------------------

// binSpotFill is one book level consumed by a market order. Spot returns these
// only when newOrderRespType=FULL, which is why Place sets it explicitly --
// without it the commission is invisible and the fee would have to be guessed.
type binSpotFill struct {
	Price           string `json:"price"`
	Qty             string `json:"qty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
}

// binSpotOrder is the spot response.
//
// Note CummulativeQuoteQty: Binance spells it with a doubled m. It is a typo
// in their API that has been there for years and will not be fixed, because
// fixing it would break every client. Spelling it correctly here yields a
// silent zero.
type binSpotOrder struct {
	Symbol              string        `json:"symbol"`
	OrderID             int64         `json:"orderId"`
	ClientOrderID       string        `json:"clientOrderId"`
	TransactTime        int64         `json:"transactTime"`
	OrigQty             string        `json:"origQty"`
	ExecutedQty         string        `json:"executedQty"`
	CummulativeQuoteQty string        `json:"cummulativeQuoteQty"`
	Status              string        `json:"status"`
	Side                string        `json:"side"`
	Fills               []binSpotFill `json:"fills"`
}

// binFuturesOrder is the futures response.
//
// AvgPrice is a STRING and is "0" or "0.00000" until the order fills. It is
// also absent from the response to a query on a resting order. Treating "0" as
// a fill price would book a position at zero, which propagates into every PnL
// number downstream -- so it is checked, not trusted.
//
// There is NO commission field here. Futures fees must be read separately from
// /fapi/v1/userTrades. Until that is done, FeePaid stays zero and the
// reconciler flags the gap rather than assuming the fee was free.
type binFuturesOrder struct {
	Symbol        string `json:"symbol"`
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	Status        string `json:"status"`
	OrigQty       string `json:"origQty"`
	ExecutedQty   string `json:"executedQty"`
	AvgPrice      string `json:"avgPrice"`
	CumQuote      string `json:"cumQuote"`
	Side          string `json:"side"`
	UpdateTime    int64  `json:"updateTime"`
}

// --- placing ----------------------------------------------------------------

// Place implements OrderPlacer.
func (b *BinanceTrader) Place(ctx context.Context, req OrderRequest) (OrderResult, error) {
	if err := req.Validate(); err != nil {
		return OrderResult{}, err
	}

	res := OrderResult{
		Venue:         "binance",
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

	params := url.Values{}
	params.Set("symbol", req.Symbol)
	params.Set("side", string(req.Side))
	params.Set("type", string(req.Type))
	params.Set("quantity", strconv.FormatFloat(req.Quantity, 'f', -1, 64))
	params.Set("newClientOrderId", req.ClientOrderID)

	host, path := b.spotBase, "/api/v3/order"
	if req.Leg == PerpLeg {
		host, path = b.futuresBase, "/fapi/v1/order"
		if req.ReduceOnly {
			params.Set("reduceOnly", "true")
		}
	} else {
		// FULL is what makes the fills array -- and therefore the commission
		// and the true average price -- appear at all.
		params.Set("newOrderRespType", "FULL")
	}
	if req.Type == Limit {
		params.Set("price", strconv.FormatFloat(req.Price, 'f', -1, 64))
		params.Set("timeInForce", "GTC")
	}

	httpReq, err := SignBinance(b.creds, http.MethodPost, host, path, params)
	if err != nil {
		return res, err
	}
	httpReq = httpReq.WithContext(ctx)

	resp, err := b.http.Do(httpReq)
	if err != nil {
		// THE IMPORTANT BRANCH. A transport error after the request left this
		// machine does not mean the order did not reach Binance. The outcome is
		// unknown, and the caller must Query by client ID rather than retry.
		res.RawError = "transport error: " + err.Error()
		return res, fmt.Errorf("%w: binance %s %s (id %s): %v",
			ErrUnconfirmed, req.Leg, req.Symbol, req.ClientOrderID, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res.ReportedAt = time.Now().UTC()

	if resp.StatusCode != http.StatusOK {
		res.RawError = truncate(string(body), 300)
		// A 4xx is a genuine rejection: Binance validated and refused, so
		// nothing was placed. A 5xx is NOT -- the order may have been accepted
		// before the failure.
		if resp.StatusCode >= 500 {
			res.Status = StatusUnknown
			return res, fmt.Errorf("%w: binance HTTP %d on %s %s: %s",
				ErrUnconfirmed, resp.StatusCode, req.Leg, req.Symbol, res.RawError)
		}
		res.Status = StatusRejected
		return res, fmt.Errorf("execution: binance rejected %s %s: HTTP %d: %s",
			req.Leg, req.Symbol, resp.StatusCode, res.RawError)
	}

	if err := b.decode(body, req.Leg, &res); err != nil {
		res.RawError = truncate(string(body), 300)
		return res, err
	}
	return res, nil
}

// decode fills an OrderResult from a venue response body.
func (b *BinanceTrader) decode(body []byte, leg Leg, res *OrderResult) error {
	if leg == PerpLeg {
		var o binFuturesOrder
		if err := json.Unmarshal(body, &o); err != nil {
			return fmt.Errorf("%w: parsing binance futures order response: %v", ErrUnconfirmed, err)
		}
		res.VenueOrderID = strconv.FormatInt(o.OrderID, 10)
		res.Status = OrderStatus(o.Status)
		res.FilledQty, _ = strconv.ParseFloat(o.ExecutedQty, 64)
		res.QuoteSpent, _ = strconv.ParseFloat(o.CumQuote, 64)

		if v, err := strconv.ParseFloat(o.AvgPrice, 64); err == nil && v > 0 {
			res.AvgFillPrice = v
		} else if res.FilledQty > 0 && res.QuoteSpent > 0 {
			// Derive rather than report zero. cumQuote/executedQty is the same
			// volume-weighted number avgPrice would have carried.
			res.AvgFillPrice = res.QuoteSpent / res.FilledQty
		}
		// FeePaid deliberately left at zero: futures does not report it here.
		// The reconciler reads it from userTrades and flags the difference.
		return nil
	}

	var o binSpotOrder
	if err := json.Unmarshal(body, &o); err != nil {
		return fmt.Errorf("%w: parsing binance spot order response: %v", ErrUnconfirmed, err)
	}
	res.VenueOrderID = strconv.FormatInt(o.OrderID, 10)
	res.Status = OrderStatus(o.Status)
	res.FilledQty, _ = strconv.ParseFloat(o.ExecutedQty, 64)
	res.QuoteSpent, _ = strconv.ParseFloat(o.CummulativeQuoteQty, 64)

	if res.FilledQty > 0 && res.QuoteSpent > 0 {
		res.AvgFillPrice = res.QuoteSpent / res.FilledQty
	}

	// Sum commissions across fills. If they arrive in more than one asset --
	// part BNB, part base -- the total is meaningless, so the asset is marked
	// MIXED and the reconciler is forced to read the trade history properly
	// rather than add unlike things together.
	var fee float64
	asset := ""
	for _, f := range o.Fills {
		c, err := strconv.ParseFloat(f.Commission, 64)
		if err != nil {
			continue
		}
		fee += c
		if asset == "" {
			asset = f.CommissionAsset
		} else if asset != f.CommissionAsset {
			asset = "MIXED"
		}
	}
	res.FeePaid, res.FeeAsset = fee, asset
	return nil
}

// Query implements OrderPlacer. This is the confirmation path after an
// unconfirmed Place.
func (b *BinanceTrader) Query(ctx context.Context, symbol, clientOrderID string, leg Leg) (OrderResult, error) {
	res := OrderResult{
		Venue: "binance", Leg: leg, Symbol: symbol,
		ClientOrderID: clientOrderID, Status: StatusUnknown,
		ReportedAt: time.Now().UTC(),
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("origClientOrderId", clientOrderID)

	host, path := b.spotBase, "/api/v3/order"
	if leg == PerpLeg {
		host, path = b.futuresBase, "/fapi/v1/order"
	}

	// A GET, so read-only credentials could run this too -- which is the point:
	// confirming what happened must never require the ability to trade.
	httpReq, err := SignBinance(b.creds, http.MethodGet, host, path, params)
	if err != nil {
		return res, err
	}
	httpReq = httpReq.WithContext(ctx)

	resp, err := b.http.Do(httpReq)
	if err != nil {
		return res, fmt.Errorf("%w: querying binance %s %s: %v", ErrUnconfirmed, leg, symbol, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		res.RawError = truncate(string(body), 300)
		return res, fmt.Errorf("%w: binance order query HTTP %d: %s",
			ErrUnconfirmed, resp.StatusCode, res.RawError)
	}

	if err := b.decode(body, leg, &res); err != nil {
		return res, err
	}
	// A query on a filled order returns no fills array, so the commission is
	// not available here. Left at zero rather than invented.
	res.RequestedQty = res.FilledQty
	return res, nil
}

// truncate shortens a venue error for a log line without losing the code at
// the front, which is the part that identifies the problem.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
