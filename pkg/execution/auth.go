// Package execution holds everything that requires credentials.
//
// It is a SEPARATE package from pkg/exchange on purpose. pkg/exchange is the
// read-only public market data layer -- it has no credentials, no signing, and
// no way to reach an authenticated endpoint. Nothing in the paper rig imports
// this package, so the monitor that has been collecting data for months
// literally cannot place an order no matter what it is asked to do.
//
// Three structural guarantees are enforced here rather than left to care:
//
//  1. Credentials are loaded ONLY from environment variables, never from a
//     file inside the repository. The previous project's keys ended up in a
//     public Google Drive folder because they lived in a .env next to the
//     code. systemd's EnvironmentFile= puts them at /etc/vega/credentials,
//     outside the repo and outside every backup tarball.
//
//  2. Secrets are redacted in String(), GoString() and every error message in
//     this package. A key that appears in a log line is a leaked key -- logs
//     get pasted into chat windows, bug reports and screenshots.
//
//  3. A ReadOnly signer REFUSES to sign anything but a GET. Not "should not" --
//     Sign() returns an error. A binary built with read-only credentials
//     cannot place an order even if the calling code is wrong.
package execution

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mode selects which set of servers a client talks to.
//
// Testnet exists so order-placement code can be debugged without money at
// risk. Its books are thin and its fills are not representative, so it proves
// mechanics -- right side, right size, right symbol, error handling -- and
// nothing about execution quality.
type Mode string

const (
	Mainnet Mode = "mainnet"
	Testnet Mode = "testnet"
)

// Capability is what a set of credentials is permitted to do.
//
// This mirrors the permission actually set on the exchange, and is checked
// again here. Belt and braces: the exchange rejects a trade from a read-only
// key, and so does this package, before the request is even built.
type Capability string

const (
	// ReadOnly can query balances, positions and income. It cannot sign a
	// POST, PUT or DELETE, so it cannot place, amend or cancel an order.
	ReadOnly Capability = "read-only"

	// Trade can place and cancel orders. Never grant withdrawal permission on
	// the exchange side -- a key that cannot withdraw cannot drain an account
	// regardless of what goes wrong here.
	Trade Capability = "trade"
)

var (
	// ErrNoCredentials means the environment variables were not set. This is
	// not a failure to be worked around: a live component without credentials
	// must stop, not silently continue in some degraded mode.
	ErrNoCredentials = errors.New("execution: credentials not found in environment")

	// ErrReadOnly is returned when read-only credentials are asked to sign a
	// state-changing request. This is the guarantee that makes phase 1 safe.
	ErrReadOnly = errors.New("execution: credentials are READ-ONLY and cannot sign a state-changing request")
)

// Credentials is an API key pair and what it is allowed to do.
//
// Never construct this literally with hardcoded strings. Use FromEnv.
type Credentials struct {
	Venue      string
	Key        string
	secret     string
	Capability Capability
	Mode       Mode
}

// FromEnv loads credentials for a venue from the environment.
//
// Expects, for venue "binance":
//
//	BINANCE_API_KEY
//	BINANCE_API_SECRET
//
// These are supplied by systemd via EnvironmentFile=/etc/vega/credentials,
// which should be chmod 600 and owned by root. They must never appear in the
// repository, in go source, or in a command line -- a command line is visible
// to every user on the box via ps.
func FromEnv(venue string, cap Capability, mode Mode) (Credentials, error) {
	prefix := strings.ToUpper(venue)
	if mode == Testnet {
		// Testnet keys are different keys entirely. Keeping them under a
		// separate prefix means a misconfiguration cannot accidentally point
		// mainnet credentials at testnet, or worse, the reverse.
		prefix += "_TESTNET"
	}

	key := strings.TrimSpace(os.Getenv(prefix + "_API_KEY"))
	secret := strings.TrimSpace(os.Getenv(prefix + "_API_SECRET"))

	if key == "" || secret == "" {
		return Credentials{}, fmt.Errorf("%w: set %s_API_KEY and %s_API_SECRET (systemd EnvironmentFile=/etc/vega/credentials)",
			ErrNoCredentials, prefix, prefix)
	}

	return Credentials{
		Venue:      venue,
		Key:        key,
		secret:     secret,
		Capability: cap,
		Mode:       mode,
	}, nil
}

// String redacts. Everything that might print a Credentials -- a log line, an
// error, a %v in a debug statement -- goes through here.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{venue:%s mode:%s capability:%s key:%s secret:REDACTED}",
		c.Venue, c.Mode, c.Capability, redact(c.Key))
}

// GoString redacts too, so %#v is safe.
func (c Credentials) GoString() string { return c.String() }

// redact shows enough of a key to identify which one it is, and not enough to
// use it. Four leading characters is sufficient to tell two keys apart in a
// log without being useful to anyone reading over your shoulder.
func redact(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "..." + strings.Repeat("*", 4)
}

// CanTrade reports whether these credentials may place orders.
func (c Credentials) CanTrade() bool { return c.Capability == Trade }

// checkMethod is the read-only guarantee. Called by every signer before any
// signature is computed.
func (c Credentials) checkMethod(method string) error {
	if c.Capability == Trade {
		return nil
	}
	if strings.EqualFold(method, http.MethodGet) {
		return nil
	}
	return fmt.Errorf("%w: refused to sign %s (venue %s, mode %s)",
		ErrReadOnly, method, c.Venue, c.Mode)
}

// --- Binance ---------------------------------------------------------------

// Binance hosts. Testnet is a completely separate deployment with its own
// keys, its own balances and its own order book.
const (
	BinanceFuturesMainnet = "https://fapi.binance.com"
	BinanceFuturesTestnet = "https://testnet.binancefuture.com"
	BinanceSpotMainnet    = "https://api.binance.com"
	BinanceSpotTestnet    = "https://testnet.binance.vision"
)

// BinanceFuturesBase returns the USDT-M futures host for a mode.
func BinanceFuturesBase(m Mode) string {
	if m == Testnet {
		return BinanceFuturesTestnet
	}
	return BinanceFuturesMainnet
}

// BinanceSpotBase returns the spot host for a mode.
func BinanceSpotBase(m Mode) string {
	if m == Testnet {
		return BinanceSpotTestnet
	}
	return BinanceSpotMainnet
}

// SignBinance builds a signed Binance request.
//
// Binance's scheme: take the query string exactly as it will be sent, HMAC it
// with SHA-256 using the secret, hex-encode, and append as a `signature`
// parameter. The API key travels in the X-MBX-APIKEY header.
//
// Two details that cause hard-to-diagnose failures:
//
//   - The signature covers the query string BYTE FOR BYTE as transmitted.
//     Re-encoding the parameters after signing invalidates it. This function
//     therefore signs the encoded string and appends the signature to that
//     exact string rather than re-serialising a map.
//
//   - `timestamp` is required and must be within recvWindow of the exchange's
//     clock. A VPS with drifting time gets -1021 "Timestamp for this request
//     is outside of the recvWindow", which looks like an auth failure but is
//     a clock problem. recvWindow is set generously below for that reason.
func SignBinance(c Credentials, method, host, path string, params url.Values) (*http.Request, error) {
	if err := c.checkMethod(method); err != nil {
		return nil, err
	}
	if params == nil {
		params = url.Values{}
	}

	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	if params.Get("recvWindow") == "" {
		params.Set("recvWindow", "10000")
	}

	query := params.Encode()
	sig := hmacHex(c.secret, query)
	full := host + path + "?" + query + "&signature=" + sig

	req, err := http.NewRequest(method, full, nil)
	if err != nil {
		// Deliberately does not wrap err with the URL: the URL contains the
		// signature, and a signature in a log is a partial secret leak.
		return nil, fmt.Errorf("execution: building binance %s %s request", method, path)
	}
	req.Header.Set("X-MBX-APIKEY", c.Key)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// --- Bybit -----------------------------------------------------------------

const (
	BybitMainnet = "https://api.bybit.com"
	BybitTestnet = "https://api-testnet.bybit.com"
)

// BybitBase returns the Bybit host for a mode.
func BybitBase(m Mode) string {
	if m == Testnet {
		return BybitTestnet
	}
	return BybitMainnet
}

// SignBybit builds a signed Bybit v5 request.
//
// Bybit's v5 scheme is DIFFERENT from Binance's and the difference is easy to
// get wrong. The signed payload is:
//
//	timestamp + apiKey + recvWindow + (queryString for GET, or body for POST)
//
// concatenated with no separators, HMAC-SHA256 with the secret, hex-encoded.
// It travels in headers, not in the query string:
//
//	X-BAPI-API-KEY, X-BAPI-TIMESTAMP, X-BAPI-RECV-WINDOW, X-BAPI-SIGN
//
// Getting the concatenation order wrong produces retCode 10004 "error sign",
// which says nothing about which part was wrong.
func SignBybit(c Credentials, method, host, path string, params url.Values, body string) (*http.Request, error) {
	if err := c.checkMethod(method); err != nil {
		return nil, err
	}

	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	const recvWindow = "10000"

	var query string
	if params != nil {
		query = params.Encode()
	}

	payload := body
	if strings.EqualFold(method, http.MethodGet) {
		payload = query
	}

	sig := hmacHex(c.secret, ts+c.Key+recvWindow+payload)

	full := host + path
	if query != "" && strings.EqualFold(method, http.MethodGet) {
		full += "?" + query
	}

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequest(method, full, reader)
	} else {
		req, err = http.NewRequest(method, full, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("execution: building bybit %s %s request", method, path)
	}

	req.Header.Set("X-BAPI-API-KEY", c.Key)
	req.Header.Set("X-BAPI-TIMESTAMP", ts)
	req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)
	req.Header.Set("X-BAPI-SIGN", sig)
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// hmacHex is HMAC-SHA256, hex encoded. Both venues use the same primitive and
// differ only in what they feed it.
func hmacHex(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// ScrubURL removes a signature from a URL so it can be logged.
//
// Any code in this package that wants to log a request path must pass it
// through here first. The signature is derived from the secret; publishing
// enough signatures alongside their payloads is a cryptographic gift.
func ScrubURL(raw string) string {
	i := strings.Index(raw, "signature=")
	if i < 0 {
		return raw
	}
	return raw[:i] + "signature=REDACTED"
}
