package crossvenue

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imtiyaz/vega-bot/pkg/hyperliquid"
	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

// THE CANDIDATE BUILDER
//
// One Hyperliquid call returns all three venues' funding rates, which is what
// makes this scan cheap. Order books are NOT included, so they must be fetched
// per coin per venue -- and that is the expensive part.
//
// So the work is done in two stages:
//
//	1. CHEAP: one funding call, form every venue pair, discard anything whose
//	   spread could not repay a round trip regardless of what the book says.
//	2. EXPENSIVE: read books only for what survived.
//
// Reading books for all ~200 Hyperliquid coins across three venues would be
// 600 requests a pass against venue rate limits, to measure hundreds of pairs
// that the funding rate alone already disqualifies.

// BuilderConfig governs measurement, not trading. The entry gate lives in
// Book.assess and is applied afterwards.
type BuilderConfig struct {
	NotionalUSD float64

	// DepthBandBps is how far from the mid still counts as available depth.
	//
	// NOT the touch. Measured 2026-08-12: KAITO's touch held $15 on Binance
	// while $400 filled for 3.176 bps round trip -- the book behind the front
	// level was ample. Sizing off the touch refuses pairs that fill fine.
	DepthBandBps float64

	// MinSpreadBpsHr is the stage-1 filter, applied BEFORE any book is read.
	MinSpreadBpsHr float64

	// MinVolUSD is Hyperliquid's 24h notional volume floor, also stage 1.
	MinVolUSD float64

	// MaxCoinsToMeasure caps stage 2. Venue rate limits are real and a scan
	// that gets itself banned measures nothing at all.
	MaxCoinsToMeasure int

	// Pace is the delay between book requests.
	Pace time.Duration

	// DecayTrackHours is how long a pair keeps being logged AFTER it stops
	// qualifying. Observational only -- it gates nothing and opens nothing.
	DecayTrackHours float64
}

func DefaultBuilderConfig(notionalUSD float64) BuilderConfig {
	if notionalUSD <= 0 {
		notionalUSD = 400
	}
	return BuilderConfig{
		NotionalUSD:       notionalUSD,
		DepthBandBps:      15,
		MinSpreadBpsHr:    1.0,
		MinVolUSD:         10_000_000,
		MaxCoinsToMeasure: 25,
		Pace:              60 * time.Millisecond,
		DecayTrackHours:   6,
	}
}

func (c BuilderConfig) withDefaults() BuilderConfig {
	d := DefaultBuilderConfig(c.NotionalUSD)
	if c.NotionalUSD <= 0 {
		c.NotionalUSD = d.NotionalUSD
	}
	if c.DepthBandBps <= 0 {
		c.DepthBandBps = d.DepthBandBps
	}
	if c.MinSpreadBpsHr <= 0 {
		c.MinSpreadBpsHr = d.MinSpreadBpsHr
	}
	if c.MinVolUSD <= 0 {
		c.MinVolUSD = d.MinVolUSD
	}
	if c.MaxCoinsToMeasure <= 0 {
		c.MaxCoinsToMeasure = d.MaxCoinsToMeasure
	}
	if c.Pace <= 0 {
		c.Pace = d.Pace
	}
	if c.DecayTrackHours <= 0 {
		c.DecayTrackHours = d.DecayTrackHours
	}
	return c
}

// Builder turns live venue data into measured candidates.
type Builder struct {
	HL      *hyperliquid.Client
	Readers map[string]orderbook.PerpReader
	Cfg     BuilderConfig

	// Settled holds recent SETTLED funding per venue+symbol. Entries are
	// decided on it; predictions are not trusted for that job.
	Settled *SettledCache

	// HeldCoins are coins with an OPEN POSITION. They bypass the entry spread
	// floor.
	//
	// Entry criteria and monitoring criteria are not the same thing, and they
	// were sharing one gate. A position whose spread decayed below the floor
	// was filtered out of its own feed, so Update could not match it to a
	// candidate and took the "pair vanished, do not accrue" path -- which is
	// correct for a genuinely missing observation and wrong for one we removed
	// ourselves.
	//
	// Measured 2026-08-18: 1000RATS, a 4-hour pair, was held 34 hours at 1.845
	// bps/hr -- below the 5.0 floor -- and booked 2 of its 8 due settlements.
	// GPS booked 1 of 8. BLUAI booked 0 in ten hours. Every cross-venue P&L
	// figure is understated by whatever those settlements were worth.
	HeldCoins map[string]bool

	// Universe supplies coins with a genuinely wide SETTLED spread, which are
	// examined regardless of what their prediction happens to say.
	//
	// Without it the shortlist is drawn by predicted spread -- the signal
	// measured at 7.30 bps/hr against 0.45 actually paid -- so the book sees
	// roughly ten arbitrary pairs out of 3,400 and correctly refuses them. On
	// 2026-08-20 that was 371 refusals and zero entries in six hours, while a
	// manual scan found three pairs clearing the bar. The book was not wrong;
	// it was looking in the wrong place.
	Universe             *UniverseScanner
	PriorityMinRecent    float64
	PriorityMinSameSign  float64
	PriorityMinIntervals int
	PriorityMax          int

	// lastQualified records when each pair last cleared the spread floor, so
	// it can be followed down afterwards. Observational only.
	mu            sync.Mutex
	lastQualified map[string]time.Time
}

// NewBuilder wires the three venues.
func NewBuilder(cfg BuilderConfig) *Builder {
	return &Builder{
		HL:      hyperliquid.New(),
		Readers: orderbook.Readers(),
		Cfg:     cfg.withDefaults(),
	}
}

// DecayObs is a pair being followed DOWN, after it stopped qualifying.
//
// # WHY THIS EXISTS
//
// passes.jsonl only records pairs ABOVE the entry floor. A pair that decays
// below it does not appear as a small number -- it VANISHES. So the decay
// curve measured on 2026-08-15 was censored: the fastest-dying pairs left the
// sample first, and what remained were survivors. The curve came out
// non-monotonic (91.6% remaining at 2h but 82.5% at 1h), which a real decay
// cannot do.
//
// Following a pair down for a few hours after it drops out fixes that. It
// changes NO GATE and opens NO POSITION -- it only writes rows. The entry
// logic cannot see this list.
type DecayObs struct {
	Coin        string  `json:"coin"`
	LongVenue   string  `json:"long_venue"`
	ShortVenue  string  `json:"short_venue"`
	SpreadBpsHr float64 `json:"spread_bps_hr"`
	// HoursSinceQualified is how long ago this pair last cleared the floor.
	HoursSinceQualified float64 `json:"hours_since_qualified"`
}

// BuildStats explains what a pass did, including what it refused and why.
// "0 candidates" must always be explainable.
type BuildStats struct {
	Coins           int
	PairsFormed     int
	UnknownVenues   int
	DroppedSpread   int
	DroppedVolume   int
	DroppedDelisted int
	// Venues dropped because the symbol's settlement interval could not be
	// established. Never guessed.
	UnresolvedInterval int
	// Intervals taken from a venue-wide default rather than published for the
	// symbol. Usable, but unverified -- watch this number.
	DefaultedInterval int
	CoinsShortlist    int
	CoinsMeasured     int
	BooksRead         int
	BookFailures      int
	SymbolRefusals    int
	Candidates        int
	Elapsed           time.Duration
	Decaying          []DecayObs
	Warnings          []string
}

// LoadSymbols primes the CEX instrument lists. Call once at startup, and
// occasionally after -- listings change.
func (b *Builder) LoadSymbols(ctx context.Context) error {
	for name, r := range b.Readers {
		if err := r.LoadSymbols(ctx); err != nil {
			return fmt.Errorf("loading %s symbols: %w", name, err)
		}
	}
	return nil
}

type pairKey struct{ coin, long, short string }

// Build fetches, measures and returns candidates ready for Book.Update.
func (b *Builder) Build(ctx context.Context) ([]Candidate, BuildStats, error) {
	start := time.Now()
	now := time.Now().UTC()
	cfg := b.Cfg.withDefaults()
	var st BuildStats

	rates, unknown, err := b.HL.PredictedFundings(ctx)
	if err != nil {
		return nil, st, fmt.Errorf("predicted fundings: %w", err)
	}
	st.UnknownVenues = unknown
	st.Coins = len(rates)

	// Merge OKX in as a fourth venue.
	//
	// Hyperliquid's predictedFundings covers hyperliquid, binance and bybit in
	// one call. OKX is not in it, so its rates come from OKX's own bulk
	// endpoint -- and its interval is MEASURED from the gap between its
	// settlement timestamps rather than assumed, which is how KAITO's 4-hour
	// clock was found on all three of the other venues.
	//
	// A coin OKX lists but Hyperliquid does not is skipped: with no rate for
	// the other venues there is nothing to pair it against.
	if okx, ok := b.Readers["okx"].(*orderbook.OKXPerp); ok {
		merged := 0
		for instID, f := range okx.Fundings() {
			coin := strings.SplitN(instID, "-", 2)[0]
			if rates[coin] == nil {
				// UNION, not Hyperliquid-anchored. Seeding the universe from
				// HL's 232 coins made the book structurally blind to the
				// pairs the listing backtest actually found -- RE bitget/mexc,
				// SSPC bitget/bybit, DOS bitget/okx. None touch Hyperliquid.
				rates[coin] = map[string]hyperliquid.VenueRate{}
			}
			if f.IntervalHours <= 0 {
				st.UnresolvedInterval++
				continue
			}
			rates[coin]["okx"] = hyperliquid.VenueRate{
				Venue:         "okx",
				RawRate:       f.Rate,
				NextFundingMs: f.NextFundingMs,
			}.WithInterval(f.IntervalHours)
			merged++
		}
		st.Warnings = append(st.Warnings,
			fmt.Sprintf("okx: merged %d coins as a fourth venue", merged))
	}

	// Merge Bitget in as a sixth venue.
	//
	// Bitget publishes fundInterval PER SYMBOL -- 8h for BTC, 4h for KAITO --
	// so the interval is read from the venue rather than assumed. What it does
	// not publish in bulk is the next settlement time, so the calendar is
	// hydrated symbol by symbol, capped per pass, and cached until it expires.
	//
	// A symbol whose next settlement is unknown is NOT merged. simulatePlan
	// cannot walk a calendar it does not have, and GateNoSchedule would refuse
	// it downstream regardless -- better to exclude it here than to present it
	// as a candidate and reject it later.
	//
	// Bitget is absent from TakerBps on purpose. Until a real taker fee is
	// entered, GateFeesUnverified refuses every Bitget pair.
	if bg, ok := b.Readers["bitget"].(*orderbook.BitgetPerp); ok {
		var want []string
		for coin := range rates {
			if sym, ok := bg.ResolveCoin(coin); ok {
				want = append(want, sym)
			}
		}
		hydrated, mismatched := bg.EnsureFundingTimes(ctx, want, 60*time.Millisecond, 400)
		merged, noSchedule := 0, 0
		for sym, f := range bg.Fundings() {
			coin := strings.TrimSuffix(sym, "USDT")
			if rates[coin] == nil {
				// UNION, not Hyperliquid-anchored. Seeding the universe from
				// HL's 232 coins made the book structurally blind to the
				// pairs the listing backtest actually found -- RE bitget/mexc,
				// SSPC bitget/bybit, DOS bitget/okx. None touch Hyperliquid.
				rates[coin] = map[string]hyperliquid.VenueRate{}
			}
			if f.IntervalHours <= 0 {
				st.UnresolvedInterval++
				continue
			}
			if f.NextFundingMs <= 0 {
				noSchedule++
				continue
			}
			rates[coin]["bitget"] = hyperliquid.VenueRate{
				Venue:         "bitget",
				RawRate:       f.Rate,
				NextFundingMs: f.NextFundingMs,
			}.WithInterval(f.IntervalHours)
			merged++
		}
		st.Warnings = append(st.Warnings, fmt.Sprintf(
			"bitget: merged %d coins (%d calendars hydrated, %d still unscheduled, %d interval mismatches)",
			merged, hydrated, noSchedule, mismatched))
	}

	// Merge MEXC in as a seventh venue.
	//
	// MEXC publishes collectCycle per symbol -- 8h for BTC, 4h for KAITO --
	// but its bulk ticker carries neither the cycle nor the next settlement,
	// so both hydrate per symbol and cache until they expire. Fundings()
	// returns ONLY hydrated symbols, so an un-hydrated rate is absent rather
	// than silently normalised against a guessed interval.
	//
	// Only coins already known from another venue are hydrated. A MEXC-only
	// coin has nothing to pair against, so fetching its calendar is waste.
	//
	// MEXC is absent from TakerBps on purpose. GateFeesUnverified refuses
	// every MEXC pair until a real taker fee is entered -- the same discipline
	// that kept Bitget out until this evening.
	if mx, ok := b.Readers["mexc"].(*orderbook.MEXCPerp); ok {
		var want []string
		for coin := range rates {
			if sym, ok := mx.ResolveCoin(coin); ok {
				want = append(want, sym)
			}
		}
		hydrated := mx.EnsureFundingMeta(ctx, want, 0, 200)
		merged := 0
		for sym, f := range mx.Fundings() {
			coin := strings.TrimSuffix(sym, "_USDT")
			if rates[coin] == nil {
				continue // nothing to pair a MEXC-only listing against
			}
			rates[coin]["mexc"] = hyperliquid.VenueRate{
				Venue:         "mexc",
				RawRate:       f.Rate,
				NextFundingMs: f.NextFundingMs,
			}.WithInterval(f.IntervalHours)
			merged++
		}
		st.Warnings = append(st.Warnings, fmt.Sprintf(
			"mexc: merged %d coins (%d calendars hydrated this pass)", merged, hydrated))
	}

	// Merge Lighter in as a fifth venue.
	//
	// TWO PERIODS, AND THEY ARE NOT THE SAME NUMBER. Lighter QUOTES per 8
	// hours and SETTLES every hour -- both from its own docs, cross-checked
	// against the relayed Hyperliquid rate which arrives at exactly 8.00x.
	// WithInterval gets the QUOTING period so RawRate converts correctly;
	// the settlement cadence reaches the calendar via
	// LighterPerp.FundingIntervalHours, which reports 1.
	if lt, ok := b.Readers["lighter"].(*orderbook.LighterPerp); ok {
		merged := 0
		for sym, f := range lt.Fundings() {
			if rates[sym] == nil {
				// UNION, not Hyperliquid-anchored. Seeding the universe from
				// HL's 232 coins made the book structurally blind to the
				// pairs the listing backtest actually found -- RE bitget/mexc,
				// SSPC bitget/bybit, DOS bitget/okx. None touch Hyperliquid.
				rates[sym] = map[string]hyperliquid.VenueRate{}
			}
			// Stored PER HOUR, deliberately.
			//
			// VenueRate.IntervalHours serves two masters: BpsPerHour divides
			// by it, and the settlement calendar multiplies by it. On every
			// other venue those are the same number. On Lighter the quote is
			// per 8h and the payment is hourly, so they differ by 8x.
			//
			// Converting the rate to hourly first makes both readings agree:
			// RawRate/1 gives the right BpsPerHour, and IntervalHours=1 tells
			// the calendar to expect eight small payments rather than one
			// lump. Storing the 8h rate with an interval of 1 would inflate
			// the spread 8x; storing it with an interval of 8 would model the
			// wrong settlement shape -- which is what stopped out the first
			// two KAITO positions on 2026-08-12.
			hourly := f.RatePer8h / orderbook.LighterRatePeriodHours

			// "Funding payments occur at each hour mark" -- docs.lighter.xyz,
			// read 2026-08-15. So the next settlement is the top of the next
			// UTC hour. Derived from the venue's stated schedule, not guessed.
			nextHour := now.Truncate(time.Hour).Add(time.Hour)

			rates[sym]["lighter"] = hyperliquid.VenueRate{
				Venue:         "lighter",
				RawRate:       hourly,
				NextFundingMs: nextHour.UnixMilli(),
			}.WithInterval(orderbook.LighterSettlementHours)
			merged++
		}
		st.Warnings = append(st.Warnings,
			fmt.Sprintf("lighter: merged %d coins as a fifth venue (zero fees, $10 minimum, hourly settlement)", merged))
	}

	assets, err := b.HL.Assets(ctx)
	if err != nil {
		// Not fatal, but it removes the volume filter, and the caller must be
		// told rather than silently handed unfiltered results.
		st.Warnings = append(st.Warnings,
			fmt.Sprintf("hyperliquid asset contexts unavailable (%v); volume filter is OFF", err))
	}

	// --- stage 0: resolve each venue's SYMBOL and SETTLEMENT INTERVAL ---
	//
	// This must happen before any spread is computed. Binance and Bybit publish
	// the funding interval PER SYMBOL. Until it is known, a raw rate cannot be
	// turned into bps per hour, and two rates on different clocks cannot be
	// compared at all.
	//
	// A venue whose interval cannot be established is DROPPED for that coin.
	// It is not defaulted, and it is counted so the drop is visible.
	resolve := func(coin, venue string, r hyperliquid.VenueRate) (hyperliquid.VenueRate, bool, bool) {
		if venue == "hyperliquid" || venue == "lighter" {
			// Both settle hourly for every asset, and both arrive already
			// normalised -- Hyperliquid from PredictedFundings, Lighter from
			// the merge above. Passing either through the per-symbol reader
			// would re-divide an already-divided rate.
			return r, true, r.Known()
		}
		rd, ok := b.Readers[venue]
		if !ok {
			return r, false, false
		}
		sym, ok := rd.ResolveCoin(coin)
		if !ok {
			st.SymbolRefusals++
			return r, false, false
		}
		fi := rd.FundingIntervalHours(sym)
		if !fi.Ok || fi.Hours <= 0 {
			st.UnresolvedInterval++
			return r, false, false
		}
		if !fi.Explicit {
			st.DefaultedInterval++
		}
		return r.WithInterval(fi.Hours), fi.Explicit, true
	}

	// Merge Binance and Bybit from their OWN bulk endpoints.
	//
	// Their rates previously arrived only via Hyperliquid's predictedFundings,
	// which covers the 232 coins HYPERLIQUID lists -- not the 527 and 713 they
	// actually list. That made every bitget/bybit and bitget/binance pair the
	// listing backtest found structurally unobservable here.
	//
	// GAP-FILL ONLY. Where Hyperliquid already relayed a rate it is left alone,
	// so the measurement running since the interval fix is not perturbed by a
	// change of source mid-experiment.
	//
	// KNOWN LIMITATION, not yet fixed: Hyperliquid writes kPEPE where Binance
	// writes 1000PEPE. Symbols already reachable from an existing coin key are
	// skipped via ResolveCoin, but a coin Hyperliquid does NOT list can still
	// land under two spellings across venues and fail to pair with itself. The
	// okx, bitget and lighter merges above have the same hazard.
	for _, vname := range []string{"binance", "bybit"} {
		rd, ok := b.Readers[vname]
		if !ok {
			continue
		}
		var fs map[string]orderbook.CEXFunding
		var ferr error
		switch v := rd.(type) {
		case *orderbook.BinancePerp:
			fs, ferr = v.Fundings(ctx)
		case *orderbook.BybitPerp:
			fs, ferr = v.Fundings(ctx)
		default:
			continue
		}
		if ferr != nil {
			st.Warnings = append(st.Warnings, fmt.Sprintf("%s bulk funding: %v", vname, ferr))
			continue
		}
		// RESOLVE, don't skip.
		//
		// The first version marked a symbol "covered" if any existing coin key
		// resolved to it and skipped it. But OKX had already added 431 keys
		// with no Binance rate attached, so this skipped exactly the coins that
		// most needed one -- 994 coins produced 1490 pairs, three more than
		// before the union existed.
		//
		// Mapping symbol -> EXISTING key instead means Binance's 1000PEPE rate
		// lands on Hyperliquid's kPEPE entry rather than creating a second,
		// permanently unpairable spelling of the same coin.
		keyOf := map[string]string{}
		for coin := range rates {
			if sym, ok := rd.ResolveCoin(coin); ok {
				keyOf[sym] = coin
			}
		}
		added := 0
		for sym, f := range fs {
			coin, aligned := keyOf[sym]
			if !aligned {
				if !strings.HasSuffix(sym, "USDT") {
					continue
				}
				coin = strings.TrimSuffix(sym, "USDT")
			}
			if rates[coin] == nil {
				rates[coin] = map[string]hyperliquid.VenueRate{}
			}
			if _, exists := rates[coin][vname]; exists {
				continue
			}
			rates[coin][vname] = hyperliquid.VenueRate{
				Venue:         vname,
				RawRate:       f.Rate,
				NextFundingMs: f.NextFundingMs,
			}.WithInterval(f.IntervalHours)
			added++
		}
		st.Warnings = append(st.Warnings,
			fmt.Sprintf("%s: %d coins added beyond Hyperliquid's relay", vname, added))
	}

	// Counted AFTER the merges. Reporting Hyperliquid's 232 here was a stale
	// display that hid the entire point of the union.
	st.Coins = len(rates)

	// Coins the universe scan says are worth a look regardless of prediction.
	priority := map[string]bool{}
	if b.Universe != nil {
		priority = b.Universe.Coins(b.PriorityMinRecent, b.PriorityMinSameSign,
			b.PriorityMinIntervals, b.PriorityMax)
		n, at, scanning, fetched, planned := b.Universe.Status()
		age := "never"
		if !at.IsZero() {
			age = time.Since(at).Round(time.Minute).String() + " ago"
		}
		scanStr := fmt.Sprintf("%v", scanning)
		if scanning && planned > 0 {
			scanStr = fmt.Sprintf("true, %d/%d fetched", fetched, planned)
		}
		st.Warnings = append(st.Warnings, fmt.Sprintf(
			"universe: %d pairs scanned %s, %d coins prioritised (scanning=%s)",
			n, age, len(priority), scanStr))
	}

	// --- stage 1: cheap ---
	type prospect struct {
		key         pairKey
		long, short hyperliquid.VenueRate
		longExp     bool
		shortExp    bool
		spread      float64
		volUSD      float64
	}
	var shortlist []prospect

	for coin, byVenue := range rates {
		volUSD := 0.0
		if a, ok := assets[coin]; ok {
			// A delisted market still publishes a funding rate and still has an
			// EMPTY BOOK. BNT answered successfully with zero bids and zero
			// asks on 2026-08-11 while quoting a rate.
			if a.Delisted {
				st.DroppedDelisted++
				continue
			}
			volUSD = a.DayNtlVlmUSD
		}

		// Normalise every venue for this coin before pairing anything.
		type norm struct {
			rate hyperliquid.VenueRate
			exp  bool
		}
		usable := map[string]norm{}
		for v, r := range byVenue {
			nr, exp, ok := resolve(coin, v, r)
			if !ok || !nr.Known() {
				continue
			}
			usable[v] = norm{rate: nr, exp: exp}
		}

		names := make([]string, 0, len(usable))
		for v := range usable {
			names = append(names, v)
		}
		sort.Strings(names)

		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				a, bb := usable[names[i]], usable[names[j]]
				st.PairsFormed++

				// Short the venue paying more, long the venue paying less.
				long, short := a, bb
				if a.rate.BpsPerHour > bb.rate.BpsPerHour {
					long, short = bb, a
				}
				spread := short.rate.BpsPerHour - long.rate.BpsPerHour
				pkey := coin + "|" + long.rate.Venue + "|" + short.rate.Venue

				if spread < cfg.MinSpreadBpsHr && !b.HeldCoins[coin] && !priority[coin] {
					// Below the floor: no trade, no book read, no gate. But if
					// it qualified recently, follow it DOWN -- that is the half
					// of the decay curve passes.jsonl has never contained, and
					// its absence made the measured curve non-monotonic.
					b.mu.Lock()
					if t, seen := b.lastQualified[pkey]; seen {
						age := now.Sub(t).Hours()
						if age <= cfg.DecayTrackHours {
							st.Decaying = append(st.Decaying, DecayObs{
								Coin: coin, LongVenue: long.rate.Venue,
								ShortVenue:  short.rate.Venue,
								SpreadBpsHr: spread, HoursSinceQualified: age,
							})
						} else {
							delete(b.lastQualified, pkey)
						}
					}
					b.mu.Unlock()
					st.DroppedSpread++
					continue
				}

				b.mu.Lock()
				if b.lastQualified == nil {
					b.lastQualified = map[string]time.Time{}
				}
				b.lastQualified[pkey] = now
				b.mu.Unlock()
				if volUSD > 0 && volUSD < cfg.MinVolUSD {
					st.DroppedVolume++
					continue
				}
				shortlist = append(shortlist, prospect{
					key:      pairKey{coin, long.rate.Venue, short.rate.Venue},
					long:     long.rate,
					short:    short.rate,
					longExp:  long.exp,
					shortExp: short.exp,
					spread:   spread,
					volUSD:   volUSD,
				})
			}
		}
	}

	// Widest spread first, then cap by COIN so the cap counts book requests
	// rather than pairs -- three pairs of one coin need only three books.
	sort.Slice(shortlist, func(i, j int) bool { return shortlist[i].spread > shortlist[j].spread })

	allowed := map[string]bool{}
	// Held coins are seeded in ahead of the cap.
	//
	// The shortlist is sorted widest-spread-first, so a position that has
	// decayed to 1.8 bps/hr sorts to the bottom and gets cut the moment 25
	// coins clear the floor above it -- which is precisely when spreads are
	// wide and you least want to stop watching what you own. Held coins are
	// not competing for measurement budget; they are already spending capital,
	// and an unmeasured position books none of its settlements.
	for coin := range b.HeldCoins {
		allowed[coin] = true
	}
	// Priority coins are seeded ahead of the cap for the same reason held ones
	// are: the shortlist is sorted widest-PREDICTED-first, so a coin with a real
	// settled spread and an unremarkable prediction sorts to the bottom and is
	// cut -- which is exactly the coin worth examining.
	for coin := range priority {
		allowed[coin] = true
	}
	var kept []prospect
	for _, p := range shortlist {
		if !allowed[p.key.coin] {
			if len(allowed) >= cfg.MaxCoinsToMeasure {
				continue
			}
			allowed[p.key.coin] = true
		}
		kept = append(kept, p)
	}
	st.CoinsShortlist = len(allowed)

	// --- stage 2: expensive ---
	books := map[string]orderbook.Book{} // venue|coin
	getBook := func(venue, coin string) (orderbook.Book, bool) {
		k := venue + "|" + coin
		if bk, ok := books[k]; ok {
			return bk, bk.Measured
		}

		var bk orderbook.Book
		var err error

		if venue == "hyperliquid" {
			bk, err = b.HL.L2Book(ctx, coin)
		} else {
			r, ok := b.Readers[venue]
			if !ok {
				books[k] = orderbook.Book{}
				return orderbook.Book{}, false
			}
			sym, ok := r.ResolveCoin(coin)
			if !ok {
				// No VERIFIED symbol. Not an error -- most Hyperliquid coins
				// simply are not listed on a given CEX.
				st.SymbolRefusals++
				books[k] = orderbook.Book{}
				return orderbook.Book{}, false
			}
			bk, err = r.Book(ctx, sym)
		}

		time.Sleep(cfg.Pace)
		st.BooksRead++
		if err != nil {
			st.BookFailures++
			st.Warnings = append(st.Warnings, fmt.Sprintf("%s %s: %v", venue, coin, err))
			books[k] = orderbook.Book{}
			return orderbook.Book{}, false
		}
		books[k] = bk
		return bk, bk.Measured
	}

	var out []Candidate
	measured := map[string]bool{}

	for _, p := range kept {
		coin := p.key.coin
		longBook, okL := getBook(p.key.long, coin)
		shortBook, okS := getBook(p.key.short, coin)
		measured[coin] = true

		c := Candidate{
			Coin:                  coin,
			LongVenue:             p.key.long,
			ShortVenue:            p.key.short,
			LongBpsHr:             p.long.BpsPerHour,
			ShortBpsHr:            p.short.BpsPerHour,
			LongIntervalHours:     p.long.IntervalHours,
			ShortIntervalHours:    p.short.IntervalHours,
			LongIntervalExplicit:  p.longExp,
			ShortIntervalExplicit: p.shortExp,
			LongNextFundingMs:     p.long.NextFundingMs,
			ShortNextFundingMs:    p.short.NextFundingMs,
			LongMeasured:          okL,
			ShortMeasured:         okS,
			VolUSD:                p.volUSD,
		}

		n := cfg.NotionalUSD

		if okL {
			// Long leg: BUY to open (cross asks), SELL to close (cross bids).
			in := longBook.SweepCost(n, true)
			outS := longBook.SweepCost(n, false)
			c.LongEntrySlipBps = in.SlippageBps
			c.LongExitSlipBps = outS.SlippageBps
			c.LongExhausted = in.Exhausted || outS.Exhausted || !in.Ok || !outS.Ok

			bid, ask, _ := longBook.DepthWithinBps(cfg.DepthBandBps)
			c.LongDepthUSD = minf(bid, ask)
		}

		if okS {
			// Short leg: SELL to open, BUY to close. The mirror of the above.
			in := shortBook.SweepCost(n, false)
			outS := shortBook.SweepCost(n, true)
			c.ShortEntrySlipBps = in.SlippageBps
			c.ShortExitSlipBps = outS.SlippageBps
			c.ShortExhausted = in.Exhausted || outS.Exhausted || !in.Ok || !outS.Ok

			bid, ask, _ := shortBook.DepthWithinBps(cfg.DepthBandBps)
			c.ShortDepthUSD = minf(bid, ask)
		}

		// Price basis: the position's entire price P&L.
		if okL && okS {
			midL, ok1 := longBook.Mid()
			midS, ok2 := shortBook.Mid()
			if ok1 && ok2 && midL > 0 && midS > 0 {
				avg := (midL + midS) / 2
				c.BasisBps = (midL - midS) / avg * 10000
				c.BasisMeasured = true
			}
		}

		out = append(out, c)
	}

	st.CoinsMeasured = len(measured)
	st.Candidates = len(out)
	st.Elapsed = time.Since(start)
	// --- stage 3: SETTLED history for the survivors ---
	//
	// Only the shortlist reaches here -- tens of pairs, not thousands -- so the
	// cost is bounded and the predicted rate keeps its one honest job: a cheap
	// pre-filter that says "look here", never "trade this".
	//
	// Bounded by TIME. A venue that throttles must not stall a pass.
	if b.Settled != nil {
		ivl := map[string]float64{}
		symOf := map[string]string{}
		want := map[string][]string{}
		seen := map[string]bool{}
		for _, c := range out {
			for _, leg := range []struct {
				venue string
				hours float64
			}{{c.LongVenue, c.LongIntervalHours}, {c.ShortVenue, c.ShortIntervalHours}} {
				rd, ok := b.Readers[leg.venue]
				if !ok {
					continue
				}
				sym, ok := rd.ResolveCoin(c.Coin)
				if !ok {
					continue
				}
				k := leg.venue + "|" + c.Coin
				symOf[k] = sym
				ivl[leg.venue+"|"+sym] = leg.hours
				if dk := leg.venue + "|" + sym; !seen[dk] {
					seen[dk] = true
					want[leg.venue] = append(want[leg.venue], sym)
				}
			}
		}
		fetched, failed := 0, 0
		for v, syms := range want {
			f, bad := b.Settled.Ensure(ctx, v, syms,
				func(sym string) float64 { return ivl[v+"|"+sym] },
				12*time.Second, 90*time.Millisecond)
			fetched += f
			failed += bad
		}
		stamped, reoriented := 0, 0
		for i := range out {
			l, lok := b.Settled.Get(out[i].LongVenue, symOf[out[i].LongVenue+"|"+out[i].Coin])
			sh, sok := b.Settled.Get(out[i].ShortVenue, symOf[out[i].ShortVenue+"|"+out[i].Coin])
			if !lok || !sok || !l.Known() || !sh.Known() {
				continue
			}
			out[i].SettledLongBpsHr = l.BpsPerHour
			out[i].SettledShortBpsHr = sh.BpsPerHour
			out[i].SettledSpreadBpsHr = sh.BpsPerHour - l.BpsPerHour
			out[i].SettledRecentSpreadBpsHr = sh.RecentBpsPerHour - l.RecentBpsPerHour
			out[i].SettledIntervals = l.Intervals
			if sh.Intervals < out[i].SettledIntervals {
				out[i].SettledIntervals = sh.Intervals
			}
			out[i].SettledSameSign = l.SameSignFrac
			if sh.SameSignFrac < out[i].SettledSameSign {
				out[i].SettledSameSign = sh.SameSignFrac
			}
			out[i].SettledKnown = true
			// Orient by the RECENT spread: what is happening now decides the
			// direction, not what happened four days ago.
			if out[i].SettledRecentSpreadBpsHr < 0 {
				// SETTLED FUNDING FAVOURS THE OTHER DIRECTION.
				//
				// Candidates are oriented by the PREDICTED spread, which is the
				// signal we no longer trust. Where settled funding disagrees,
				// take the trade the other way round rather than discarding it:
				// on 2026-08-19 two of ten candidates a pass were being refused
				// as REVERSED, and those are the ones where the prediction was
				// most wrong -- which is where the real edge is likeliest.
				//
				// reversed() handles the parts that are not a straight swap:
				// slippage maps cross-wise (entry to exit), and basis negates.
				out[i] = out[i].reversed()
				reoriented++
			}
			stamped++
		}
		st.Warnings = append(st.Warnings, fmt.Sprintf(
			"settled: %d of %d candidates priced from settled history, %d re-oriented (%d fetched, %d failed, %d cached)",
			stamped, len(out), reoriented, fetched, failed, b.Settled.Size()))
	}

	return out, st, nil
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
