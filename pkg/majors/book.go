package majors

// THE MAJORS BOOK: static long-basis on deep coins, flat when funding turns.
//
// This is a different animal from the funding book and deliberately so. That
// one hunts individual opportunities across 1,555 thin coins and was measured
// on 2026-08-25 to hold about $171 in total -- a real edge with no room for
// money. This one holds a fixed basket of the deepest coins in crypto, makes a
// single portfolio-level decision, and is bounded by nothing we could measure:
// $12M of notional is 0.18% of one day's volume on binance and bybit alone.
//
// WHAT FIVE YEARS OF SETTLED FUNDING SAID (2021-08 to 2026-08, 6 majors)
//
//	static long, 5x        18.7%/yr   29.0% max drawdown
//	static long, 10x       35.9%/yr   54.8% max drawdown
//	flat<-0.10 14d, 5x     22.8%/yr   15.9% max drawdown
//	flat<-0.10 14d, 10x    48.4%/yr   30.1% max drawdown
//
// The de-risk rule beat holding on BOTH return and drawdown at every leverage
// tested, and across the whole parameter neighbourhood rather than at one
// lucky setting.
//
// WHY THE RULE IS "GO FLAT" AND NOT "REVERSE"
//
// Reversing into negative funding looks better on the worst month -- FTX goes
// from -48.51% to +22.93% at 10x -- and then loses it all back in friction. A
// sign-change rule flips ~30 times in five years at two round trips each,
// consuming about 198% of capital at 10x. Worse, the reverse leg needs
// borrowed spot, and borrow is dearest exactly when the book wants to short:
// at 50%/yr the whole edge inverts. Being ABSENT requires no lender and fires
// three times in five years instead of thirty.
//
// WHAT THIS BOOK DOES NOT KNOW, and it is a great deal
//
//	three events    the entire benefit of the rule comes from sidestepping
//	                three crises in five years. "It works" and "it was lucky
//	                three times" are not statistically distinguishable at n=3.
//	crisis access   the rule fires when funding has been deeply negative for
//	                two weeks -- precisely when venues freeze, halt, or fail.
//	                FTX did not merely pay bad funding; it ceased to exist.
//	basis risk      spot and perp diverging can liquidate a levered hedge
//	                regardless of what funding does. Not modelled anywhere.
//	leverage        MODELLED, not real. Running it needs unified margin with
//	                spot hedging enabled, or the perp leg liquidates on price
//	                moves alone while the solvent spot leg watches.
//
// So this book exists to find out whether the backtest survives contact with
// live data. It is paper. It places no orders.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Config is fixed at startup and recorded in the journal so a run can always
// be reproduced from its own record.
type Config struct {
	// Basket is base assets, e.g. BTC, ETH. Symbols are formed as BASE+Quote.
	Basket []string `json:"basket"`
	Quote  string   `json:"quote"`
	Venue  string   `json:"venue"`

	// NotionalUSD is the whole basket, split equally across usable symbols.
	NotionalUSD float64 `json:"notional_usd"`

	// Leverage is notional per dollar of capital. 5 means $100 of capital
	// carries $500 of notional. MODELLED: no order is placed and no margin
	// is posted, so this is an accounting assumption, not a position.
	Leverage float64 `json:"leverage"`

	// TrailDays is the window for the de-risk signal. 14 measured best, and
	// 7 and 30 also beat holding, which is why 14 is a choice rather than a
	// fitted parameter.
	TrailDays int `json:"trail_days"`

	// ExitBpsHr: go flat when trailing mean funding falls below this.
	// ReenterBpsHr: return only once it climbs back above this. The gap
	// between them is hysteresis, and without it the book oscillates around
	// one level and pays a round trip each time.
	ExitBpsHr    float64 `json:"exit_bps_hr"`
	ReenterBpsHr float64 `json:"reenter_bps_hr"`

	// RoundTripBps is charged on notional at every entry and every exit.
	RoundTripBps float64 `json:"round_trip_bps"`

	// MinSpotTopUSD and MinVol24hUSD refuse a symbol whose book cannot carry
	// its share. A symbol that fails is DROPPED from the basket for that
	// poll, and the remaining ones are not re-weighted to compensate --
	// pretending the money went somewhere it could not go is how a paper
	// book invents returns.
	MinSpotTopUSD float64 `json:"min_spot_top_usd"`
	MinVol24hUSD  float64 `json:"min_vol_24h_usd"`

	// TouchFraction is how much of a symbol's share must rest at the touch.
	//
	// 1.0 demands the WHOLE position instantly, which is right for a book
	// that executes in one shot on a two-day hold. It is wrong here. This is
	// a thirty-day position accumulated over hours, and $12M of majors
	// notional is 0.18% of one day's volume on binance and bybit -- the touch
	// is not the constraint, the day is.
	//
	// Set to 1.0 it refused BNB and XRP, two of the most liquid markets in
	// crypto, because their touch happened to hold less than $167 at that
	// instant. That is measuring the wrong thing.
	TouchFraction float64 `json:"touch_fraction"`
}

func DefaultConfig() Config {
	return Config{
		Basket:        []string{"BTC", "ETH", "SOL", "BNB", "XRP", "DOGE"},
		Quote:         "USDT",
		Venue:         "binance",
		NotionalUSD:   1000,
		Leverage:      5,
		TrailDays:     14,
		ExitBpsHr:     -0.10,
		ReenterBpsHr:  0.0,
		RoundTripBps:  33.0,
		MinSpotTopUSD: 100,
		MinVol24hUSD:  10_000_000,
		TouchFraction: 0.25,
	}
}

// day is one calendar day of funding observations, kept so the trailing signal
// survives restarts without re-reading the journal.
type day struct {
	Day    string  `json:"day"`
	SumBps float64 `json:"sum_bps_hr"`
	N      int     `json:"n"`
}

func (d day) mean() float64 {
	if d.N == 0 {
		return 0
	}
	return d.SumBps / float64(d.N)
}

// State is everything that must survive a restart.
type State struct {
	Invested   bool      `json:"invested"`
	SinceMs    int64     `json:"since_ms"`
	LastSeenMs int64     `json:"last_seen_ms"`
	Days       []day     `json:"days"`
	Entries    int       `json:"entries"`
	Exits      int       `json:"exits"`
	StartedAt  time.Time `json:"started_at"`

	// All in bps on NOTIONAL, so leverage is applied only when reporting on
	// capital. Keeping them separate means a leverage change does not
	// retroactively rewrite history.
	FundingBps float64 `json:"funding_bps"`
	CostBps    float64 `json:"cost_bps"`
}

type Book struct {
	cfg   Config
	st    State
	path  string
	dirty bool
}

// New loads an existing book or starts one. A corrupt state file is an error,
// never a fresh book: silently restarting from zero would erase a P&L record
// and look like a clean start.
func New(dataDir string, cfg Config) (*Book, error) {
	if cfg.Leverage <= 0 {
		return nil, fmt.Errorf("leverage must be positive, got %g", cfg.Leverage)
	}
	if cfg.TrailDays < 1 {
		return nil, fmt.Errorf("trail_days must be at least 1, got %d", cfg.TrailDays)
	}
	if cfg.ExitBpsHr >= cfg.ReenterBpsHr {
		// Without a gap the book flips back and forth at one level.
		return nil, fmt.Errorf("exit %.3f must be below reenter %.3f",
			cfg.ExitBpsHr, cfg.ReenterBpsHr)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	b := &Book{cfg: cfg, path: filepath.Join(dataDir, "majors_state.json")}
	raw, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		b.st = State{Invested: false, StartedAt: time.Now().UTC()}
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &b.st); err != nil {
		return nil, fmt.Errorf("majors_state.json is corrupt (%w) -- refusing to "+
			"start fresh and erase the record; move it aside deliberately", err)
	}
	return b, nil
}

func (b *Book) Config() Config { return b.cfg }
func (b *Book) State() State   { return b.st }

// Symbol returns the venue symbol for a base asset.
func (c Config) Symbol(base string) string { return base + c.Quote }

// Usable reports whether an observation can carry its share of the basket.
func (b *Book) Usable(o Obs, perSymbolUSD float64) (bool, string) {
	if o.Venue != b.cfg.Venue {
		return false, "WRONG_VENUE"
	}
	if !o.SpotAvailable {
		return false, "NO_SPOT_PAIR"
	}
	if !o.LiquidityMeasured {
		// Unknown is not free. A book we could not read is not a book we can
		// trade against.
		return false, "LIQUIDITY_UNKNOWN"
	}
	frac := b.cfg.TouchFraction
	if frac <= 0 {
		frac = 1.0 // a zero-value config must be the STRICT one, never the loose one
	}
	if o.SpotTopUSD < math.Max(b.cfg.MinSpotTopUSD, perSymbolUSD*frac) {
		return false, "SPOT_TOO_THIN"
	}
	if o.SpotVol24hUSD < b.cfg.MinVol24hUSD || o.PerpVol24hUSD < b.cfg.MinVol24hUSD {
		return false, "VOLUME_TOO_LOW"
	}
	if o.IntervalHours <= 0 {
		return false, "NO_INTERVAL"
	}
	return true, ""
}

// Obs is the slice of an exchange observation this book needs. Declared here
// rather than importing the exchange package so the accounting can be tested
// without a venue.
type Obs struct {
	Venue             string
	Symbol            string
	FundingRatePct    float64
	IntervalHours     float64
	SpotAvailable     bool
	LiquidityMeasured bool
	SpotHalfSpreadBps float64
	PerpHalfSpreadBps float64
	SpotTopUSD        float64
	PerpTopUSD        float64
	SpotVol24hUSD     float64
	PerpVol24hUSD     float64
}

// PollResult is what one cycle did, for the journal and the log.
type PollResult struct {
	Type       string    `json:"type"`
	TsMs       int64     `json:"ts_ms"`
	At         time.Time `json:"at"`
	Usable     []string  `json:"usable"`
	Refused    map[string]string `json:"refused,omitempty"`
	BasketBps  float64   `json:"basket_bps_hr"`
	TrailBps   float64   `json:"trail_bps_hr"`
	TrailDays  int       `json:"trail_days_seen"`
	Invested   bool      `json:"invested"`
	Changed    string    `json:"changed,omitempty"`
	AccruedBps float64   `json:"accrued_bps"`
	NetBps     float64   `json:"net_bps"`
	NetUSD     float64   `json:"net_usd"`
	CapitalUSD float64   `json:"capital_usd"`
	ReturnPct  float64   `json:"return_on_capital_pct"`
}

// Poll folds one round of observations into the book and returns what it did.
//
// Accrual uses the rate observed BEFORE the elapsed interval, never after: a
// rate read now says nothing about the hours already gone.
func (b *Book) Poll(obs []Obs, now time.Time) PollResult {
	nowMs := now.UTC().UnixMilli()
	perSymbol := b.cfg.NotionalUSD / math.Max(float64(len(b.cfg.Basket)), 1)

	want := map[string]bool{}
	for _, base := range b.cfg.Basket {
		want[b.cfg.Symbol(base)] = true
	}

	var usable []string
	refused := map[string]string{}
	sum, n := 0.0, 0
	for _, o := range obs {
		if !want[o.Symbol] {
			continue
		}
		if ok, why := b.Usable(o, perSymbol); !ok {
			// Keyed by venue AND symbol. Keying by symbol alone let the other
			// venue's WRONG_VENUE overwrite the real reason a symbol on the
			// traded venue was refused, hiding the only refusal that mattered
			// behind five that did not.
			refused[o.Venue+":"+o.Symbol] = why
			continue
		}
		usable = append(usable, o.Symbol)
		sum += o.FundingRatePct * 100.0 / o.IntervalHours
		n++
	}
	sort.Strings(usable)

	res := PollResult{
		Type: "majors_poll", TsMs: nowMs, At: now.UTC(),
		Usable: usable, Refused: refused, Invested: b.st.Invested,
	}
	if n == 0 {
		// No usable symbols is not zero funding. Accrue nothing and say so.
		res.Changed = "NO_USABLE_SYMBOLS"
		b.finish(&res)
		return res
	}

	basket := sum / float64(n)
	res.BasketBps = basket

	// --- accrue on the side held coming into this interval ---
	if b.st.LastSeenMs > 0 && b.st.Invested {
		hours := float64(nowMs-b.st.LastSeenMs) / 3600000.0
		// A gap wider than a day is a hole in the record, not a position we
		// certainly held. Skip it rather than invent income.
		if hours > 0 && hours <= 24 {
			earned := basket * hours
			b.st.FundingBps += earned
			res.AccruedBps = earned
		}
	}
	b.st.LastSeenMs = nowMs

	// --- fold into the daily series that drives the signal ---
	d := now.UTC().Format("2006-01-02")
	if k := len(b.st.Days); k > 0 && b.st.Days[k-1].Day == d {
		b.st.Days[k-1].SumBps += basket
		b.st.Days[k-1].N++
	} else {
		b.st.Days = append(b.st.Days, day{Day: d, SumBps: basket, N: 1})
	}
	if keep := b.cfg.TrailDays + 10; len(b.st.Days) > keep {
		b.st.Days = b.st.Days[len(b.st.Days)-keep:]
	}

	// --- the de-risk signal, over COMPLETE days only ---
	//
	// Today is still accumulating, so including it would let a few hours of
	// noise move a fourteen-day average and trigger a real trade.
	var trail float64
	seen := 0
	if len(b.st.Days) > 1 {
		hist := b.st.Days[:len(b.st.Days)-1]
		if len(hist) > b.cfg.TrailDays {
			hist = hist[len(hist)-b.cfg.TrailDays:]
		}
		for _, h := range hist {
			trail += h.mean()
			seen++
		}
		if seen > 0 {
			trail /= float64(seen)
		}
	}
	res.TrailBps = trail
	res.TrailDays = seen

	// The signal only speaks once it has a full window. Acting on four days
	// of data with a rule measured on fourteen is not the same strategy.
	if seen >= b.cfg.TrailDays {
		if b.st.Invested && trail < b.cfg.ExitBpsHr {
			b.st.Invested = false
			b.st.Exits++
			b.st.CostBps += b.cfg.RoundTripBps
			b.st.SinceMs = nowMs
			res.Changed = "EXIT_DERISK"
		} else if !b.st.Invested && trail > b.cfg.ReenterBpsHr {
			b.st.Invested = true
			b.st.Entries++
			b.st.CostBps += b.cfg.RoundTripBps
			b.st.SinceMs = nowMs
			res.Changed = "ENTER"
		}
	} else if !b.st.Invested && b.st.Entries == 0 {
		// FIRST ENTRY, unconditionally.
		//
		// The strategy is long-basis BY DEFAULT; the rule only ever takes the
		// book OUT. An earlier version gated this on trail > reenter, which on
		// a first run compares 0.0 > 0.0 and is false -- leaving the book flat
		// and measuring nothing while waiting for a signal that cannot exist
		// until fourteen complete days have passed.
		//
		// Recorded distinctly so it is never mistaken for the rule firing.
		b.st.Invested = true
		b.st.Entries++
		b.st.CostBps += b.cfg.RoundTripBps
		b.st.SinceMs = nowMs
		res.Changed = "ENTER_INITIAL_NO_SIGNAL"
	}

	res.Invested = b.st.Invested
	b.dirty = true
	b.finish(&res)
	return res
}

func (b *Book) finish(res *PollResult) {
	net := b.st.FundingBps - b.st.CostBps
	res.NetBps = net
	res.NetUSD = net / 10000.0 * b.cfg.NotionalUSD
	res.CapitalUSD = b.cfg.NotionalUSD / b.cfg.Leverage
	if res.CapitalUSD > 0 {
		res.ReturnPct = res.NetUSD / res.CapitalUSD * 100.0
	}
}

// Save writes state atomically. fsync before rename, because a torn state file
// is indistinguishable from a corrupt one and New refuses both.
func (b *Book) Save() error {
	if !b.dirty {
		return nil
	}
	raw, err := json.MarshalIndent(b.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return err
	}
	b.dirty = false
	return nil
}

// Summary is one line for the log.
func (b *Book) Summary() string {
	pos := "FLAT"
	if b.st.Invested {
		pos = "LONG BASIS"
	}
	net := b.st.FundingBps - b.st.CostBps
	cap := b.cfg.NotionalUSD / b.cfg.Leverage
	ret := 0.0
	if cap > 0 {
		ret = net / 10000.0 * b.cfg.NotionalUSD / cap * 100.0
	}
	return fmt.Sprintf("%s | %s $%.0f notional at %gx (capital $%.0f) | "+
		"funding %+.2f cost %.2f net %+.2f bps = %+.3f%% on capital | %d in, %d out",
		strings.Join(b.cfg.Basket, ","), pos, b.cfg.NotionalUSD, b.cfg.Leverage,
		cap, b.st.FundingBps, b.st.CostBps, net, ret, b.st.Entries, b.st.Exits)
}
