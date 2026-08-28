package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// httpJSON is the shared fetcher for venue clients.
//
// Every venue gets the same defences, because every venue has found a way to
// return something other than the JSON it promised:
//
//   - HTML error pages served with HTTP 200 (Binance did this on a wrong path
//     and produced CSS that jq choked on thirty lines from the cause)
//   - 429 rate limits, which must be surfaced rather than blindly retried
//   - 418 IP bans, where retrying extends the ban
//
// There is no POST, no auth header, no signing. A venue client physically
// cannot place an order because this is the only way it can reach the network.
func httpJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("exchange: building request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("exchange: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("exchange: reading %s: %w", url, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// keep going
	case http.StatusTooManyRequests:
		return fmt.Errorf("exchange: rate limited (429) on %s, Retry-After=%q; BACK OFF", url, resp.Header.Get("Retry-After"))
	case http.StatusTeapot:
		return fmt.Errorf("exchange: IP BANNED (418) on %s; do not retry", url)
	default:
		return fmt.Errorf("exchange: %s returned HTTP %d: %.200s", url, resp.StatusCode, body)
	}

	if strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
		return fmt.Errorf("exchange: %s returned HTML, not JSON -- wrong path? (first 120 bytes: %.120s)", url, body)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("exchange: decoding %s: %w (first 200 bytes: %.200s)", url, err, body)
	}
	return nil
}

// newVenueHTTP returns an HTTP client with bounded timeouts. A hung connection
// must never wedge the poll loop; the monitor's own context caps the total.
func newVenueHTTP() *http.Client {
	return &http.Client{Timeout: 20 * time.Second}
}

// parseF parses a numeric string, reporting failure rather than returning a
// silent zero. Venues quote every price and rate as a string, and a zero price
// is not the same thing as an unparseable one.
func parseF(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseMsString parses milliseconds-since-epoch supplied as a STRING.
// Bybit does this where Binance uses a number; the difference is invisible
// until the unmarshal fails at runtime.
func parseMsString(s string) time.Time {
	ms, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// halfSpreadBps computes the cost of crossing from mid to the far touch.
// A FLOOR on execution cost, not the cost: it assumes the whole order fills
// at the touch.
func halfSpreadBps(bid, ask float64) float64 {
	if bid <= 0 || ask <= 0 || ask < bid {
		return 0
	}
	mid := (bid + ask) / 2
	if mid <= 0 {
		return 0
	}
	return (ask - bid) / mid * 10000 / 2
}

// topOfBookUSD is the smaller resting side at the touch, in quote currency.
func topOfBookUSD(bid, bidQty, ask, askQty float64) float64 {
	b := bid * bidQty
	a := ask * askQty
	if b < a {
		return b
	}
	return a
}
