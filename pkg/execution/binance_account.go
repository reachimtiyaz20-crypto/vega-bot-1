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
	"strings"
	"time"
)

// BinanceShapesVerified records whether the JSON shapes below have been
// checked against a real response from a real account.
//
// FALSE means these structs were written from documentation, not measured.
// Every other parser in this project was built only after curling the live
// endpoint and reading the bytes -- that discipline is what caught OKX
// reporting swap volume in base coin while spot volume was in dollars, and
// OKX naming the NEXT settlement `fundingTime` and the one after that
// `nextFundingTime`.
//
// Those traps were invisible from the documentation. Assume this file
// contains one until proven otherwise.
//
// TO VERIFY: with read-only keys installed, run
//
//	vega-account dump binance
//
// and compare every field below against the raw JSON. Then set this to true
// and record the date.
const BinanceShapesVerified = false

const BinanceShapesVerifiedOn = ""

// BinanceAccount is a read-only view of a Binance account.
//
// Holds Credentials, which is why this type lives in package execution and
// not in package exchange. Nothing in the paper monitor can construct it.
type BinanceAccount struct {
	creds       Credentials
	http        *http.Client
	futuresBase string
	spotBase    string
}

// NewBinanceAccount builds a read-only Binance account reader.
//
// Returns an error rather than a half-working client if credentials are
// missing: a component that is supposed to reconcile a ledger and silently
// cannot is worse than one that refuses to start.
func NewBinanceAccount(creds Credentials) (*BinanceAccount, error) {
	if creds.Venue == "" {
		return nil, fmt.Errorf("execution: binance account needs credentials with a venue set")
	}
	return &BinanceAccount{
		creds:       creds,
		http:        &http.Client{Timeout: 20 * time.Second},
		futuresBase: BinanceFuturesBase(creds.Mode),
		spotBase:    BinanceSpotBase(creds.Mode),
	}, nil
}

// Venue implements AccountReader.
func (b *BinanceAccount) Venue() string { return "binance" }

// Mode implements AccountReader.
func (b *BinanceAccount) Mode() Mode { return b.creds.Mode }

// --- wire shapes -----------------------------------------------------------

// binFuturesAccount is /fapi/v2/account. UNVERIFIED -- see BinanceShapesVerified.
type binFuturesAccount struct {
	TotalWalletBalance    string `json:"totalWalletBalance"`
	TotalUnrealizedProfit string `json:"totalUnrealizedProfit"`
	AvailableBalance      string `json:"availableBalance"`
	Assets                []struct {
		Asset            string `json:"asset"`
		WalletBalance    string `json:"walletBalance"`
		AvailableBalance string `json:"availableBalance"`
	} `json:"assets"`
}

// binPositionRisk is /fapi/v2/positionRisk. UNVERIFIED.
//
// This endpoint is used instead of the positions array inside /fapi/v2/account
// because it is the one that carries liquidationPrice -- the field that decides
// whether a hedged position survives a rally.
type binPositionRisk struct {
	Symbol           string `json:"symbol"`
	PositionAmt      string `json:"positionAmt"`
	EntryPrice       string `json:"entryPrice"`
	MarkPrice        string `json:"markPrice"`
	UnRealizedProfit string `json:"unRealizedProfit"`
	LiquidationPrice string `json:"liquidationPrice"`
	Leverage         string `json:"leverage"`
	MarginType       string `json:"marginType"`
	IsolatedMargin   string `json:"isolatedMargin"`
	PositionSide     string `json:"positionSide"`
}

// binIncome is /fapi/v1/income. UNVERIFIED.
//
// NOTE tranId is a NUMBER on this endpoint while every price is a string.
// json.Number accepts either, so a shape change in that field cannot break
// the read.
type binIncome struct {
	Symbol     string      `json:"symbol"`
	IncomeType string      `json:"incomeType"`
	Income     string      `json:"income"`
	Asset      string      `json:"asset"`
	Time       int64       `json:"time"`
	TranID     json.Number `json:"tranId"`
}

// binSpotAccount is /api/v3/account. UNVERIFIED.
type binSpotAccount struct {
	Balances []struct {
		Asset  string `json:"asset"`
		Free   string `json:"free"`
		Locked string `json:"locked"`
	} `json:"balances"`
}

// --- reads -----------------------------------------------------------------

// Snapshot implements AccountReader: futures wallet, spot balances, and every
// open position with its liquidation price.
func (b *BinanceAccount) Snapshot(ctx context.Context) (AccountSnapshot, error) {
	snap := AccountSnapshot{
		Venue:      "binance",
		Mode:       b.creds.Mode,
		ObservedAt: time.Now().UTC(),
	}

	var acct binFuturesAccount
	if err := b.getSigned(ctx, b.futuresBase, "/fapi/v2/account", nil, &acct); err != nil {
		return snap, fmt.Errorf("binance futures account: %w", err)
	}
	snap.WalletUSD, _ = strconv.ParseFloat(acct.TotalWalletBalance, 64)
	snap.AvailableUSD, _ = strconv.ParseFloat(acct.AvailableBalance, 64)
	snap.UnrealizedUSD, _ = strconv.ParseFloat(acct.TotalUnrealizedProfit, 64)

	for _, a := range acct.Assets {
		wal, _ := strconv.ParseFloat(a.WalletBalance, 64)
		avail, _ := strconv.ParseFloat(a.AvailableBalance, 64)
		if wal == 0 && avail == 0 {
			continue
		}
		snap.Balances = append(snap.Balances, Balance{
			Venue:  "binance",
			Asset:  a.Asset + " (futures)",
			Free:   avail,
			Locked: wal - avail,
		})
	}

	var risks []binPositionRisk
	if err := b.getSigned(ctx, b.futuresBase, "/fapi/v2/positionRisk", nil, &risks); err != nil {
		return snap, fmt.Errorf("binance positionRisk: %w", err)
	}
	for _, r := range risks {
		amt, _ := strconv.ParseFloat(r.PositionAmt, 64)
		if amt == 0 {
			// Binance returns every symbol ever touched, most of them flat.
			// Carrying hundreds of zero rows would drown the real positions.
			continue
		}
		p := PerpPosition{
			Venue:      "binance",
			Symbol:     r.Symbol,
			MarginType: r.MarginType,
			ObservedAt: snap.ObservedAt,
		}
		p.PositionAmt = amt
		p.EntryPrice, _ = strconv.ParseFloat(r.EntryPrice, 64)
		p.MarkPrice, _ = strconv.ParseFloat(r.MarkPrice, 64)
		p.UnrealizedPnl, _ = strconv.ParseFloat(r.UnRealizedProfit, 64)
		p.Leverage, _ = strconv.ParseFloat(r.Leverage, 64)
		p.IsolatedMargin, _ = strconv.ParseFloat(r.IsolatedMargin, 64)

		// Binance sends "0" for liquidationPrice on cross-margin positions
		// that are not currently at risk. Zero is stored as zero and
		// HasLiquidationPrice() reports false, so downstream code treats it
		// as UNKNOWN. It must never be read as "cannot be liquidated".
		p.LiquidationPrice, _ = strconv.ParseFloat(r.LiquidationPrice, 64)

		snap.Positions = append(snap.Positions, p)
	}

	// Spot balances live on a different host entirely. A failure here is not
	// fatal to the snapshot -- the futures side carries the liquidation risk,
	// and losing spot visibility should not blind us to that.
	var spot binSpotAccount
	if err := b.getSigned(ctx, b.spotBase, "/api/v3/account", nil, &spot); err == nil {
		for _, s := range spot.Balances {
			free, _ := strconv.ParseFloat(s.Free, 64)
			locked, _ := strconv.ParseFloat(s.Locked, 64)
			if free == 0 && locked == 0 {
				continue
			}
			snap.Balances = append(snap.Balances, Balance{
				Venue:  "binance",
				Asset:  s.Asset + " (spot)",
				Free:   free,
				Locked: locked,
			})
		}
	}

	return snap, nil
}

// FundingSince implements AccountReader: settled FUNDING_FEE transfers.
//
// This is the reconciled revenue source. It reads what the exchange ACTUALLY
// paid or charged, not what the published rate implied. The previous bot got
// this half right and the cost half absent; here both exist.
//
// Binance caps a single income query at 1000 rows and 7 days, so this pages
// backwards until it reaches `since` or runs out.
func (b *BinanceAccount) FundingSince(ctx context.Context, since time.Time) ([]FundingPayment, error) {
	var out []FundingPayment
	cursor := since

	for page := 0; page < 20; page++ { // hard cap: 20 pages is ~140 days
		params := url.Values{}
		params.Set("incomeType", "FUNDING_FEE")
		params.Set("startTime", strconv.FormatInt(cursor.UnixMilli(), 10))
		params.Set("limit", "1000")

		var rows []binIncome
		if err := b.getSigned(ctx, b.futuresBase, "/fapi/v1/income", params, &rows); err != nil {
			return out, fmt.Errorf("binance income: %w", err)
		}
		if len(rows) == 0 {
			break
		}

		var newest time.Time
		for _, r := range rows {
			if !strings.EqualFold(r.IncomeType, "FUNDING_FEE") {
				continue
			}
			amt, err := strconv.ParseFloat(r.Income, 64)
			if err != nil {
				continue
			}
			t := time.UnixMilli(r.Time).UTC()
			out = append(out, FundingPayment{
				Venue:    "binance",
				Symbol:   r.Symbol,
				Amount:   amt, // SIGNED: negative means we PAID
				Asset:    r.Asset,
				SettleAt: t,
				TranID:   r.TranID.String(),
			})
			if t.After(newest) {
				newest = t
			}
		}

		if len(rows) < 1000 || newest.IsZero() {
			break
		}
		// Advance past the newest row seen. +1ms avoids re-reading it and
		// double-counting a payment, which would inflate revenue.
		cursor = newest.Add(time.Millisecond)
	}

	return out, nil
}

// --- transport -------------------------------------------------------------

// getSigned performs a signed GET and decodes JSON.
//
// Only GET. With read-only credentials SignBinance refuses anything else, so
// this whole file is incapable of changing account state.
func (b *BinanceAccount) getSigned(ctx context.Context, host, path string, params url.Values, out any) error {
	req, err := SignBinance(b.creds, http.MethodGet, host, path, params)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)

	resp, err := b.http.Do(req)
	if err != nil {
		// Never include the URL: it carries the signature.
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	if resp.StatusCode != http.StatusOK {
		// Binance error bodies are small and contain no secrets, so they are
		// safe to surface -- and -2015 (bad key / IP not whitelisted) versus
		// -1021 (clock drift) are the two you will actually hit, so seeing the
		// code matters.
		return fmt.Errorf("%s returned HTTP %d: %.300s", path, resp.StatusCode, body)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s: %w (first 300 bytes: %.300s)", path, err, body)
	}
	return nil
}

// DumpRaw fetches an endpoint and returns the raw JSON, for shape verification.
//
// This exists because BinanceShapesVerified is false. Until a human has read
// the real bytes and compared them against the structs above, nothing in this
// file should be trusted -- every other parser in this project earned its
// trust by being written after the bytes were seen, and this one has not.
func (b *BinanceAccount) DumpRaw(ctx context.Context, which string) (string, error) {
	host := b.futuresBase
	var path string
	params := url.Values{}

	switch which {
	case "account":
		path = "/fapi/v2/account"
	case "positions":
		path = "/fapi/v2/positionRisk"
	case "income":
		path = "/fapi/v1/income"
		params.Set("incomeType", "FUNDING_FEE")
		params.Set("limit", "5")
	case "spot":
		host = b.spotBase
		path = "/api/v3/account"
	default:
		return "", fmt.Errorf("unknown dump target %q (want: account, positions, income, spot)", which)
	}

	var raw json.RawMessage
	if err := b.getSigned(ctx, host, path, params, &raw); err != nil {
		return "", err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return string(raw), nil
	}
	return pretty.String(), nil
}
