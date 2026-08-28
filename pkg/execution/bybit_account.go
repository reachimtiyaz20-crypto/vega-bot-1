package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// BybitShapesVerified records whether the JSON shapes below were checked
// against a real response. FALSE means documentation, not measurement.
// See BinanceShapesVerified for why that distinction has already saved this
// project twice.
//
// TO VERIFY: with read-only keys installed, run
//
//	vega-account dump bybit
const BybitShapesVerified = false

const BybitShapesVerifiedOn = ""

// BybitAccount is a read-only view of a Bybit unified account.
type BybitAccount struct {
	creds Credentials
	http  *http.Client
	base  string
}

// NewBybitAccount builds a read-only Bybit account reader.
func NewBybitAccount(creds Credentials) (*BybitAccount, error) {
	if creds.Venue == "" {
		return nil, fmt.Errorf("execution: bybit account needs credentials with a venue set")
	}
	return &BybitAccount{
		creds: creds,
		http:  &http.Client{Timeout: 20 * time.Second},
		base:  BybitBase(creds.Mode),
	}, nil
}

// Venue implements AccountReader.
func (b *BybitAccount) Venue() string { return "bybit" }

// Mode implements AccountReader.
func (b *BybitAccount) Mode() Mode { return b.creds.Mode }

// --- wire shapes -----------------------------------------------------------

// bybitEnvelope wraps every v5 response. retCode 0 means success; anything
// else arrives inside an HTTP 200, so the status code alone proves nothing.
type bybitEnvelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
}

// bybitWalletResult is /v5/account/wallet-balance?accountType=UNIFIED.
// UNVERIFIED.
type bybitWalletResult struct {
	List []struct {
		TotalEquity           string `json:"totalEquity"`
		TotalWalletBalance    string `json:"totalWalletBalance"`
		TotalAvailableBalance string `json:"totalAvailableBalance"`
		TotalPerpUPL          string `json:"totalPerpUPL"`
		AccountType           string `json:"accountType"`
		Coin                  []struct {
			Coin                string `json:"coin"`
			WalletBalance       string `json:"walletBalance"`
			AvailableToWithdraw string `json:"availableToWithdraw"`
			Locked              string `json:"locked"`
			UnrealisedPnl       string `json:"unrealisedPnl"`
		} `json:"coin"`
	} `json:"list"`
}

// bybitPositionResult is /v5/position/list?category=linear. UNVERIFIED.
//
// TWO TRAPS HERE, both different from Binance:
//
//  1. liqPrice is a string that can be EMPTY (""), not "0". Binance sends "0"
//     for "no liquidation risk right now"; Bybit sends nothing at all. Parsing
//     "" naively yields 0.0, which HasLiquidationPrice() would then report as
//     unknown -- correct by luck. But an empty string and a genuine zero mean
//     the same thing here only because both are treated as UNKNOWN downstream.
//     Never let either become "safe".
//
//  2. side is "Buy"/"Sell" and size is UNSIGNED. Binance gives a signed
//     positionAmt where negative means short. Bybit gives a positive size plus
//     a side string, so the sign must be reconstructed. Getting this backwards
//     turns a short hedge into a long and doubles the directional exposure
//     instead of cancelling it.
type bybitPositionResult struct {
	List []struct {
		Symbol        string `json:"symbol"`
		Side          string `json:"side"`
		Size          string `json:"size"`
		AvgPrice      string `json:"avgPrice"`
		MarkPrice     string `json:"markPrice"`
		LiqPrice      string `json:"liqPrice"`
		UnrealisedPnl string `json:"unrealisedPnl"`
		Leverage      string `json:"leverage"`
		PositionIM    string `json:"positionIM"`
		TradeMode     int    `json:"tradeMode"`
	} `json:"list"`
}

// bybitTxLogResult is /v5/account/transaction-log. UNVERIFIED.
//
// Bybit has NO dedicated funding-income endpoint. Funding arrives in the
// general transaction log with type "SETTLEMENT", mixed in with trades, fees
// and transfers. The filter is therefore ours to apply, and getting it wrong
// means either missing funding or counting trading fees as revenue.
type bybitTxLogResult struct {
	NextPageCursor string `json:"nextPageCursor"`
	List           []struct {
		Symbol          string `json:"symbol"`
		Type            string `json:"type"`
		Funding         string `json:"funding"`
		Change          string `json:"change"`
		Currency        string `json:"currency"`
		TransactionTime string `json:"transactionTime"`
		ID              string `json:"id"`
	} `json:"list"`
}

// --- reads -----------------------------------------------------------------

// Snapshot implements AccountReader.
func (b *BybitAccount) Snapshot(ctx context.Context) (AccountSnapshot, error) {
	snap := AccountSnapshot{
		Venue:      "bybit",
		Mode:       b.creds.Mode,
		ObservedAt: time.Now().UTC(),
	}

	wp := url.Values{}
	wp.Set("accountType", "UNIFIED")
	var wallet bybitWalletResult
	if err := b.getSigned(ctx, "/v5/account/wallet-balance", wp, &wallet); err != nil {
		return snap, fmt.Errorf("bybit wallet-balance: %w", err)
	}
	for _, acct := range wallet.List {
		snap.WalletUSD, _ = strconv.ParseFloat(acct.TotalWalletBalance, 64)
		snap.AvailableUSD, _ = strconv.ParseFloat(acct.TotalAvailableBalance, 64)
		snap.UnrealizedUSD, _ = strconv.ParseFloat(acct.TotalPerpUPL, 64)
		for _, c := range acct.Coin {
			bal, _ := strconv.ParseFloat(c.WalletBalance, 64)
			avail, _ := strconv.ParseFloat(c.AvailableToWithdraw, 64)
			if bal == 0 && avail == 0 {
				continue
			}
			snap.Balances = append(snap.Balances, Balance{
				Venue:  "bybit",
				Asset:  c.Coin,
				Free:   avail,
				Locked: bal - avail,
			})
		}
	}

	pp := url.Values{}
	pp.Set("category", "linear")
	pp.Set("settleCoin", "USDT")
	var pos bybitPositionResult
	if err := b.getSigned(ctx, "/v5/position/list", pp, &pos); err != nil {
		return snap, fmt.Errorf("bybit position/list: %w", err)
	}
	for _, r := range pos.List {
		size, err := strconv.ParseFloat(r.Size, 64)
		if err != nil || size == 0 {
			continue
		}

		// Reconstruct the sign Binance gives us for free. "Sell" is a short,
		// which is the side a cash-and-carry holds on the perp leg.
		amt := size
		if r.Side == "Sell" {
			amt = -size
		}

		p := PerpPosition{
			Venue:       "bybit",
			Symbol:      r.Symbol,
			PositionAmt: amt,
			ObservedAt:  snap.ObservedAt,
		}
		p.EntryPrice, _ = strconv.ParseFloat(r.AvgPrice, 64)
		p.MarkPrice, _ = strconv.ParseFloat(r.MarkPrice, 64)
		p.UnrealizedPnl, _ = strconv.ParseFloat(r.UnrealisedPnl, 64)
		p.Leverage, _ = strconv.ParseFloat(r.Leverage, 64)
		p.IsolatedMargin, _ = strconv.ParseFloat(r.PositionIM, 64)

		// liqPrice may be "". ParseFloat fails, p.LiquidationPrice stays 0,
		// and HasLiquidationPrice() reports false -- UNKNOWN, never safe.
		if lp, err := strconv.ParseFloat(r.LiqPrice, 64); err == nil {
			p.LiquidationPrice = lp
		}

		if r.TradeMode == 1 {
			p.MarginType = "isolated"
		} else {
			p.MarginType = "cross"
		}

		snap.Positions = append(snap.Positions, p)
	}

	return snap, nil
}

// FundingSince implements AccountReader.
//
// Bybit funding lives in the general transaction log under type SETTLEMENT.
// The `funding` field carries the amount; `change` is the net wallet movement
// for the row and may include other components, so `funding` is the one to
// read. Signed: negative means the position PAID.
func (b *BybitAccount) FundingSince(ctx context.Context, since time.Time) ([]FundingPayment, error) {
	var out []FundingPayment
	cursor := ""

	for page := 0; page < 20; page++ {
		params := url.Values{}
		params.Set("accountType", "UNIFIED")
		params.Set("category", "linear")
		params.Set("type", "SETTLEMENT")
		params.Set("startTime", strconv.FormatInt(since.UnixMilli(), 10))
		params.Set("limit", "50")
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		var log bybitTxLogResult
		if err := b.getSigned(ctx, "/v5/account/transaction-log", params, &log); err != nil {
			return out, fmt.Errorf("bybit transaction-log: %w", err)
		}
		if len(log.List) == 0 {
			break
		}

		for _, r := range log.List {
			if r.Type != "SETTLEMENT" {
				continue
			}
			amt, err := strconv.ParseFloat(r.Funding, 64)
			if err != nil {
				continue
			}
			ms, err := strconv.ParseInt(r.TransactionTime, 10, 64)
			if err != nil {
				continue
			}
			out = append(out, FundingPayment{
				Venue:    "bybit",
				Symbol:   r.Symbol,
				Amount:   amt,
				Asset:    r.Currency,
				SettleAt: time.UnixMilli(ms).UTC(),
				TranID:   r.ID,
			})
		}

		if log.NextPageCursor == "" || log.NextPageCursor == cursor {
			break
		}
		cursor = log.NextPageCursor
	}

	return out, nil
}

// --- transport -------------------------------------------------------------

// getSigned performs a signed GET, checks retCode, and decodes result.
func (b *BybitAccount) getSigned(ctx context.Context, path string, params url.Values, out any) error {
	req, err := SignBybit(b.creds, http.MethodGet, b.base, path, params, "")
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d: %.300s", path, resp.StatusCode, body)
	}

	var env bybitEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decoding %s envelope: %w (first 300 bytes: %.300s)", path, err, body)
	}
	if env.RetCode != 0 {
		// 10004 is "error sign" and means the signature payload was assembled
		// wrongly -- timestamp + key + recvWindow + query, in that order, no
		// separators. 10003 is a bad key. 10010 is an IP not on the whitelist.
		return fmt.Errorf("%s retCode=%d: %s", path, env.RetCode, env.RetMsg)
	}

	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decoding %s result: %w (first 300 bytes: %.300s)", path, err, env.Result)
	}
	return nil
}

// DumpRaw fetches an endpoint and returns raw JSON, for shape verification.
func (b *BybitAccount) DumpRaw(ctx context.Context, which string) (string, error) {
	var path string
	params := url.Values{}

	switch which {
	case "wallet":
		path = "/v5/account/wallet-balance"
		params.Set("accountType", "UNIFIED")
	case "positions":
		path = "/v5/position/list"
		params.Set("category", "linear")
		params.Set("settleCoin", "USDT")
	case "funding":
		path = "/v5/account/transaction-log"
		params.Set("accountType", "UNIFIED")
		params.Set("category", "linear")
		params.Set("type", "SETTLEMENT")
		params.Set("limit", "5")
	default:
		return "", fmt.Errorf("unknown dump target %q (want: wallet, positions, funding)", which)
	}

	var raw json.RawMessage
	if err := b.getSigned(ctx, path, params, &raw); err != nil {
		return "", err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return string(raw), nil
	}
	return pretty.String(), nil
}
