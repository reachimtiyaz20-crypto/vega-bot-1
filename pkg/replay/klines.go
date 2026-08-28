// Package replay walks recorded positions through real price history and asks
// whether they would have been liquidated.
//
// # WHY MARK PRICE, AND WHY ONE MINUTE
//
// Two decisions here determine whether the answer means anything.
//
// FIRST: venues liquidate on the MARK price, not the last trade. Mark is
// anchored to an index precisely so that a thin-book wick cannot liquidate
// anyone. Replaying against traded prices would count deaths that never
// happened; both venues publish a dedicated mark-price kline series, and that
// is the one the liquidation engine actually reads.
//
// SECOND: granularity. VEGA's own journal samples every five minutes. A
// position is liquidated by the extreme BETWEEN two samples, so replaying
// against sampled marks would systematically miss deaths. One-minute candles
// carry HIGH and LOW, which is the worst the mark reached inside that minute
// -- the only figure that answers the question.
//
// # WHY EVERYTHING COMES FROM BYBIT
//
// Binance's margin-tier endpoint requires an authenticated request, so its
// maintenance rates cannot be verified without a key. Rather than guess them,
// every leg is modelled on BYBIT's rules and BYBIT's marks, including symbols
// actually held on Binance. Bybit's tiers are the same or slightly stricter on
// the symbols in this book, so the error runs toward caution rather than
// flattery. It is an approximation and it is stated, not hidden.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const bybitAPIBase = "https://api.bybit.com"

// Candle is one minute of mark price.
//
// High and Low are the point of this type. Open and Close cannot tell you
// whether a position died inside the minute.
type Candle struct {
	TsMs  int64
	Open  float64
	High  float64
	Low   float64
	Close float64
}

// Time is the candle's opening instant.
func (c Candle) Time() time.Time { return time.UnixMilli(c.TsMs).UTC() }

// Worst returns the extreme that matters for a given side.
//
// A long dies on the LOW, a short on the HIGH. Using the close for either
// would miss the moment of death entirely.
func (c Candle) Worst(isLong bool) float64 {
	if isLong {
		return c.Low
	}
	return c.High
}

// Series is a symbol's mark price over time, oldest first.
type Series struct {
	Venue  string
	Symbol string
	// Kind is "spot" or "linear". Recorded so a spot series can never be
	// silently used where a mark series was meant -- they differ by the basis,
	// which is the entire quantity being measured.
	Kind    string
	Candles []Candle
}

// Span reports the range covered.
func (s Series) Span() (from, to time.Time, ok bool) {
	if len(s.Candles) == 0 {
		return time.Time{}, time.Time{}, false
	}
	return s.Candles[0].Time(), s.Candles[len(s.Candles)-1].Time(), true
}

// Between returns the candles inside a window, inclusive.
func (s Series) Between(from, to time.Time) []Candle {
	var out []Candle
	fm, tm := from.UnixMilli(), to.UnixMilli()
	for _, c := range s.Candles {
		if c.TsMs >= fm && c.TsMs <= tm {
			out = append(out, c)
		}
	}
	return out
}

// Client fetches mark-price history.
type Client struct {
	HTTP    *http.Client
	BaseURL string

	// Pace is the delay between paged requests.
	Pace time.Duration
}

// New returns a client with sane defaults.
func New() *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		BaseURL: bybitAPIBase,
		Pace:    120 * time.Millisecond,
	}
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return bybitAPIBase
}

type envelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
}

// MarkKlines fetches 1-minute mark-price candles between two instants.
//
// Bybit caps a response at 1000 candles, so a window longer than about 16
// hours is paged. Pages are walked FORWARD from the start; a gap in the
// returned data ends the walk rather than being filled in, because an invented
// candle is an invented price.
// SpotKlines fetches 1-minute SPOT candles.
//
// The second series is what makes single-venue modelling honest. With one
// series both legs cancel exactly, the account can never lose equity, and
// "never liquidated" is a tautology rather than a finding. Two real series
// carry the real spot-perp basis, which is the only thing that can kill a
// netted hedge.
func (c *Client) SpotKlines(ctx context.Context, symbol string, from, to time.Time) (Series, error) {
	return c.klines(ctx, "spot", "/v5/market/kline", symbol, from, to)
}

// MarkKlines fetches 1-minute MARK-price candles for the linear perp.
func (c *Client) MarkKlines(ctx context.Context, symbol string, from, to time.Time) (Series, error) {
	return c.klines(ctx, "linear", "/v5/market/mark-price-kline", symbol, from, to)
}

func (c *Client) klines(ctx context.Context, category, path, symbol string, from, to time.Time) (Series, error) {
	s := Series{Venue: "bybit", Symbol: symbol, Kind: category}
	if !to.After(from) {
		return s, fmt.Errorf("replay: window ends before it begins")
	}

	// Bybit returns candles NEWEST FIRST and honours the end bound, so paging
	// must walk BACKWARD from the end. Walking forward fetches the most recent
	// 1000 minutes and then stops -- which silently truncated a four-day
	// window to sixteen hours on 2026-08-14, and dropped 87 of 118 subjects
	// from a replay with no error anywhere.
	fromMs := from.UnixMilli()
	cursor := to.UnixMilli()
	seen := map[int64]bool{}

	for page := 0; page < 500; page++ {
		u := fmt.Sprintf(
			"%s%s?category=%s&symbol=%s&start=%d&end=%d&limit=1000&interval=1",
			c.base(), path, category, url.QueryEscape(symbol), fromMs, cursor)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return s, err
		}
		resp, err := c.client().Do(req)
		if err != nil {
			return s, fmt.Errorf("replay: mark klines %s: %w", symbol, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return s, err
		}
		if resp.StatusCode != http.StatusOK {
			return s, fmt.Errorf("replay: mark klines %s: HTTP %d", symbol, resp.StatusCode)
		}

		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return s, fmt.Errorf("replay: decoding envelope for %s: %w", symbol, err)
		}
		if env.RetCode != 0 {
			return s, fmt.Errorf("replay: mark klines %s: retCode %d: %s",
				symbol, env.RetCode, env.RetMsg)
		}

		var res struct {
			Symbol string     `json:"symbol"`
			List   [][]string `json:"list"`
		}
		if err := json.Unmarshal(env.Result, &res); err != nil {
			return s, fmt.Errorf("replay: decoding result for %s: %w", symbol, err)
		}
		if len(res.List) == 0 {
			break
		}

		added := 0
		oldest := int64(0)
		for _, row := range res.List {
			if len(row) < 5 {
				continue
			}
			ts, ok0 := parseInt(row[0])
			o, ok1 := parseNum(row[1])
			h, ok2 := parseNum(row[2])
			l, ok3 := parseNum(row[3])
			cl, ok4 := parseNum(row[4])
			if !ok0 || !ok1 || !ok2 || !ok3 || !ok4 || h <= 0 || l <= 0 {
				continue
			}
			if oldest == 0 || ts < oldest {
				oldest = ts
			}
			if seen[ts] {
				continue
			}
			seen[ts] = true
			s.Candles = append(s.Candles, Candle{TsMs: ts, Open: o, High: h, Low: l, Close: cl})
			added++
		}

		if added == 0 || oldest == 0 || oldest <= fromMs {
			break
		}
		cursor = oldest - 60_000
		if cursor <= fromMs {
			break
		}
		time.Sleep(c.Pace)
	}

	sort.Slice(s.Candles, func(i, j int) bool { return s.Candles[i].TsMs < s.Candles[j].TsMs })

	if len(s.Candles) == 0 {
		return s, fmt.Errorf("replay: no %s history for %s in that window", category, symbol)
	}

	// The venue must actually have answered the question that was asked.
	//
	// Measured 2026-08-14: a ZECUSDT spot request for 11-14 August came back
	// with a thousand candles from 27 FEBRUARY. Nothing rejected it, and a
	// replay against five-month-old prices would have looked perfectly
	// healthy. A series that does not overlap the requested window is not
	// partial data, it is the wrong data.
	first, last, _ := s.Span()
	if last.Before(from) || first.After(to) {
		return Series{}, fmt.Errorf(
			"replay: %s %s returned %s -> %s, which does not overlap the requested "+
				"%s -> %s; the venue answered a different question",
			category, symbol,
			first.Format("2006-01-02 15:04"), last.Format("2006-01-02 15:04"),
			from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"))
	}
	return s, nil
}

func parseNum(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseInt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Some venues return timestamps as floats.
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return 0, false
		}
		return int64(f), true
	}
	return v, true
}
