# VEGA changelog

## 2026-08-11 — live execution, adaptive sizing, cross-venue measurement

### New: live execution (16 files, built and tested, NOT enabled)

| File | What |
|---|---|
| `pkg/execution/auth.go` | HMAC signing; credentials only from env; `checkMethod` refuses non-GET on read-only keys |
| `pkg/execution/account.go` | `Balance`, `PerpPosition`, `FundingPayment`, `AccountReader`, `Divergence` |
| `pkg/execution/binance_account.go` | Balances, positions, funding income ledger |
| `pkg/execution/bybit_account.go` | Same for Bybit v5 |
| `pkg/risk/liquidation.go` + test | Severity model. `Unknown` outranks `Watch`. A RALLY kills a short. 11 tests |
| `pkg/execution/order.go` | `OrderRequest/Result`, `NakedLeg`, idempotency keys, measured slippage |
| `pkg/execution/binance_order.go` | Symbol filters, `MatchQuantity`, 4xx vs 5xx |
| `pkg/execution/bybit_order.go` | `marketUnit=baseCoin` trap; create-then-query |
| `pkg/live/manager.go` | Leg ordering, unwind, `placeConfirmed`, halt latch |
| `pkg/live/reconcile.go` | Fully reconciled PnL. Nothing persists |
| `cmd/live/main.go` | Watch / testnet / mainnet modes |
| `pkg/report/excel.go` + `cmd/report` | Live workbook, 5 sheets |
| `pkg/report/paper.go` + `cmd/paper-report` | Paper workbook, 7 sheets |

### Strategy: exit rule rewritten

- `PaperConfig` gained `MinHoldDays`, `StopLossBps`
- `NegativeIntervalsBeforeExit` 2 -> 6; `MinHoldDays` 7 -> 2
- `exitReason`: HOLD WHILE UNDERWATER. Stop loss (-60 bps) and the 30-day cap
  both override it. 5 new tests
- Cause: 6 positions closed after an average 0.48-day hold against a ~15-day
  break-even, paying a full round trip each time. -186 bps. That is bug 3 of the
  predecessor (rotation churn) arriving through the exit rule

### The biggest bug of the session

`Book.Update` built its entry constraints from HARDCODED literals:
`NotionalUSD: 10000`, `MinQuoteVolume24hUSD: 50_000_000`, `MinTopOfBookFraction: 0.25`.

Every filter change made to `cmd/scan` and `cmd/monitor` had been landing in the
scanner and NOWHERE ELSE. The scanner reported 16 candidates while the book was
still refusing anything under a $50M floor at $10k notional.

Fixed: `PaperConfig.constraints()` with those four values as config, zero-guarded
against older files, wired from the monitor's flags, plus 3 tests.

### Filters and sizing

- OKX dropped from monitor and scan
- `scan` default hold 7 -> 30 days (this is why it reported 0 passing for a week)
- `Constraints.MaxRoundTripSlippageBps` + `GateSlippage`
- `min-vol` $50M -> $10M
- `pkg/exchange/sizing.go` + 11 tests: adaptive sizing to book depth.
  The SMALLER leg binds. `MinNotionalUSD` 200 (exchange minimum, not preference)
- `scan` gained `-adaptive`, `size/leg`, `net $/30d`, totals line
- notional $10,000 -> $400

### New: cross-venue perp-perp

- `cmd/dispersion/main.go` — Hyperliquid `predictedFundings` gives Binance,
  Hyperliquid and Bybit funding per coin in one public call
- Rates normalised to bps/HOUR. HlPerp quotes hourly, Bin/Bybit quote 8-hourly.
  A venue with an unknown interval is REFUSED, not guessed
- Impact spread charged into the round trip; unmeasured coins refused
- `-json` + hourly cron logging persistence

### Dashboard

- `pkg/dashboard/positions.go`: Capital & P&L, Open positions, Closed positions,
  Cross-venue dispersion — all added ABOVE the existing sections, nothing removed
- Dispersion sorted by PERSISTENCE, not size. Green once hours-seen > break-even

### Operations

- `vega` CLI: start stop restart status pnl positions brief report reports scan
  dispersion dash logs tail pause resume update test disk help
- `vega update` runs `go test` and REFUSES to build on failure
- `vega-report.timer` daily 23:55 UTC
- `vega.service` ExecStart made explicit

### Built, tested, deliberately NOT wired

`pkg/funding/exitwatch.go` + 12 tests. Exit-cost drift rule: hold while funding
per day exceeds the rate at which the exit is deteriorating. Fixes the fact that
`NetBps()` uses a stale entry-cost estimate for open positions, so the stop loss
cannot currently see a worsening exit.

### Measured findings

- Cash-and-carry, Binance/Bybit: round trip ~30 bps (two-thirds is the spot
  taker leg charged twice). **~2.2% annualised**
- Cross-venue perp-perp, liquid names: **~2-6% annualised**
- Taking all 46 dispersion pairs and holding ONE DAY: **-$720**. The strategy is
  selective or it is negative
- High funding and thin books are the SAME FACT. RPLUSDT paid 0.1446%/4h with
  $2.13 resting at the touch, and refilled to $449 when the rate fell to 0.005%

### Still open

- `BinanceShapesVerified`, `BybitShapesVerified`, `BinanceOrderShapesVerified`,
  `BybitOrderShapesVerified` all FALSE. Mainnet refused by design
- No API keys anywhere
- Binance runs spot and futures testnet as separate systems with separate keys;
  `auth.go` has one prefix
- `funding.Stats` counts open positions as wins/losses
- `excel.go` (live workbook) has only ever run on empty data

## 2026-08-12 — cross-venue perp-perp, and the interval bug

### New packages

- `pkg/orderbook` — one Book, one SweepCost. Binance and Bybit perp depth via
  public endpoints. Slippage MEASURED by walking resting orders at the intended
  size, replacing the venue-supplied impact-price proxy
- `pkg/hyperliquid` — funding, asset contexts, L2 book, 16 tests
- `pkg/crossvenue` — perp-perp positions, book, entry gate, settlement calendar,
  candidate builder, 30 tests
- `cmd/cross` + `vega-cross.service` — runs alongside the cash-and-carry monitor,
  writes cross_positions.json, cannot disturb it

### THE INTERVAL BUG

`venueIntervalHours` hardcoded Binance and Bybit at 8 hours. BOTH PUBLISH IT PER
SYMBOL. KAITOUSDT settles every FOUR on each, confirmed from Binance's own
settlement history and /fapi/v1/fundingInfo.

Consequences, measured:

- KAITO hyperliquid/binance read **+24.8 bps/hr**. True value **+0.33**. 75x.
- Two positions opened on that number and closed at the stop loss, -$7.49
- The pair no longer clears the 1.0 bps/hr floor at all

Fixed: intervals come from `orderbook.PerpReader.FundingIntervalHours`, per
symbol, from each venue's own instrument data. Binance publishes only its
overrides, so an absent symbol is the documented 8h default and is flagged
NOT EXPLICIT. Bybit states it for every instrument, so a missing value is a
refusal. `PredictedFundings` now returns CEX rates RAW and unusable until an
interval is supplied.

The irony is on the record: the package doc directly above that map said "a
venue whose interval is not known is REFUSED rather than assumed."

### THE SETTLEMENT CALENDAR

`roundTrip / spread` assumes funding trickles in. It arrives as LUMPS on two
unaligned clocks. The first loss, reconstructed:

	22:15  open                                  net  -28.95
	23:00  hyperliquid +36.75                    net   +7.80
	00:00  hyperliquid +36.75, BINANCE -126.88   net  -82.33   STOPPED OUT

Held 1.75 hours, charged a FULL FOUR HOURS of Binance funding. Continuous
break-even said 4.9h. The same trade opened at 00:05 instead reaches -4.3 bps
at worst and turns profitable at hour 4.9.

`pkg/crossvenue/schedule.go` simulates the calendar forward from each venue's
own next-funding timestamp and refuses anything whose worst point breaches the
stop before it breaks even -- `STOPS_OUT_BEFORE_PROFIT`. Confirmed firing on
live data 2026-08-12 13:28 against a pair with a positive spread and a 6.5h
break-even that would have gone 88 bps underwater.

Tests replay both real losses from the calendar alone and reproduce -82.3 bps
against the -82.34 the live book recorded.

### PRICE BASIS

A cross-venue position's price P&L is entirely `basis_at_exit - basis_at_entry`.
KAITO showed 136.6 bps between Binance and Bybit; three venue pairs closed the
triangle to the decimal. Median over 252 readings: -75.2 bps, worst -210.1.

Measured, journaled, shown on the dashboard, and deliberately NOT summed into
NET -- funding is settled and banked, basis is unrealised and reverses. Gate
`MaxEntryBasisBps` exists and DEFAULTS TO OFF until the log covers more than
one coin.

### Also

- `ExitWatch` wired for the first time, in crossvenue. Seeded at entry with the
  measured exit rather than assuming symmetry with the entry cost
- Settlements book the rate that APPLIED during the interval, not the next
  interval's prediction
- Dashboard: cross-venue positions and ledger, above the existing sections
- Dispersion `Cleared` fixed: judged on wall-clock SPAN, not distinct-hour count
- Dispersion's cost model overstates by ~6x vs a measured sweep (42.5 vs 23.1
  bps on the same pair, same instant) -- superseded by cmd/cross
- Delisted Hyperliquid markets dropped; BNT quotes a rate with an empty book
- `vega update` now builds dispersion and cross
- `vega brief`, `vega cross`, `DEPLOY.md`

## 2026-08-15 — margin, replay, borrow, OKX

### THE SETTLEMENT CALENDAR (2026-08-13)

`roundTrip / spread` assumes funding trickles in. It arrives as LUMPS on two
unaligned clocks. `pkg/crossvenue/schedule.go` simulates the calendar forward
from each venue's own next-funding timestamp and refuses any path that breaches
the stop loss before breaking even -- STOPS_OUT_BEFORE_PROFIT. Tests replay both
real losses and reproduce -82.3 bps against the -82.34 the book recorded.

Also fixed: settlements booked ONE interval per poll however many had elapsed.
A position held 23.03 hours had booked 19 hourly settlements instead of 23, and
every dropped one was on the leg that PAYS -- so it read ~38 bps richer than it
was.

### FORECAST VS ACTUAL

The plan is frozen at entry and checked at every settlement. First real datum,
ACE 2026-08-15: forecast +36.6 bps, actual +8.28, **error -28.27**. The spread
decayed 9.139 -> 5.324 bps/hr between entry and settlement. The entry gate
assumes the entry spread persists and it does not. Needs ~10 samples before a
correction is justified.

### pkg/margin + pkg/replay + cmd/leverage

Liquidation modelling from Bybit's PUBLIC risk-limit tiers. Replays recorded
positions against real 1-minute MARK-price candles -- mark, because that is what
liquidates; 1-minute high/low, because a 5-minute sample misses the wick.

Measured, 115 subjects over 3 days:

	cross-venue isolated    3x  0% liquidated    5x  33%    10x  69%
	cash-and-carry portfolio 10x 0%, worst margin ratio 0.9%

Cross-venue caps at 3x. Single-venue is dramatically safer because the exchange
SEES the hedge.

Bugs found and fixed in the process: kline pagination walked forward when Bybit
returns newest-first (truncated 4 days to 16 hours and silently dropped 87 of
118 subjects); a ZECUSDT spot request for August returned February data; the MMR
plausibility bound was set at 0.5 and refused Bybit's legitimate 60% top tier.

### pkg/borrow + cmd/borrow + vega borrow

Measured 2026-08-15: USDT borrows at **2.803% on OKX**, 3.934% on Bybit. Bybit
quotes HOURLY, OKX and Binance DAILY -- the same figure means 24 different things
and an unknown period is refused.

Leverage multiplies (funding - borrow), not funding:

	LINKUSDT  net f 6.55%  ->  40.3% at 10x
	BTCUSDT   net f 4.21%  ->  16.9% at 10x
	DOGEUSDT  net f 1.37%  ->  -11.5% at 10x   LOSES money levered
	HYPEUSDT  net f -3.79% ->  -63.2% at 10x

Cross-venue needs no borrowing at all -- both legs are perps on margin, so
return = base x L with no interest drag.

### OKX ADDED, AND WHAT IT TAUGHT

Fourth venue: 431 live USDT swaps, 140 merged, pairs 504 -> 910. Contract sizes
are in CONTRACTS not base units (ctVal 0.01 for BTC) -- reading them raw
overstates BTC depth by 100x. Funding intervals derived from the venue's own
settlement gap.

FEE CORRECTION: hardcoded 5.0 bps from memory. Actual, read off
okx.com/en-ae/fees on 2026-08-15: **Futures regular taker 0.2500% = 25 bps**,
spot taker 0.60%. Five times too low. The 0.05% figure is the VIP 9 rate, which
needs 1.8 BILLION AED in assets.

THE FINDING: 910 pairs, still only ONE shortlisted. Not one OKX pair cleared the
stage-one spread filter, which runs BEFORE fees. OKX's funding tracks Binance's
and Bybit's. **Every dislocation ever recorded has Hyperliquid on one side.**
Adding another CEX adds nothing -- the gap is CEX vs DEX. If more venues are
wanted, they must be DEXs.

### Dashboard rebuilt

Four groups instead of twelve interleaved sections: summary, cash-and-carry,
cross-venue, reference, diagnostics. Every column header carries its unit.
New: live candidates (all four venues), borrow rates, leverage per position for
both strategies, venue reference with fee sources and dates. Removed the
superseded dispersion table.

### Still open

- ExitWatch drift on ACE: +42.41 bps/day, 1 adverse window
- The forecast decay correction, waiting on samples
- Cash-and-carry realised numbers: **Aug 22**

## 2026-08-16 — Lighter, and two venue experiments that failed

### VENUES TESTED, WITH RESULTS

	OKX      140 coins merged, 910 pairs   ->  ZERO candidates
	dYdX     11 pairs on paper             ->  ~1 tradeable
	Lighter  93 coins merged, 1269 pairs   ->  2 candidates, HALF the cost

OKX taught the important one: its funding tracks Binance's and Bybit's, and not
one OKX pair cleared the stage-one spread filter, which runs BEFORE fees. Adding
another CEX adds nothing -- the gap is CEX vs DEX. Also corrected its taker fee
from a remembered 5 bps to the actual 25 bps (okx.com/en-ae, regular tier); the
0.05% figure is VIP 9, which needs 1.8 billion AED.

dYdX had 296 markets and no books on the ones that mattered: MNT 80 bps wide,
IOTA/XAI/KAITO empty. The rate and the fillable book are almost never the same
market.

### LIGHTER

Zero fees on all 210 markets, read from the API not a page. $10 minimum order.
Books $20k-$400k where dYdX held nothing.

TWO PERIODS, AND THEY ARE NOT THE SAME NUMBER:

	quoted   per 8 HOURS   (docs: fundingRate = clamp(...) / 8)
	settled  every HOUR    (docs: "payments occur at each hour mark")

Cross-checked: /funding-rates relays Hyperliquid's hourly rate at exactly 8.00x,
Binance's 4-hour symbols at 2.00x, its 8-hour symbols at 1.00x.

Getting this wrong showed KAITO at +13.465 bps/hr when the true figure was
+1.366. The gate refused with SETTLEMENT_SCHEDULE_UNKNOWN rather than guessing,
which is what forced the check. Fourth interval error caught this way.

Stored per HOUR so BpsPerHour and the settlement calendar agree -- one field
serves both and they differ by 8x on this venue alone.

WHAT IT DELIVERS: not bigger spreads, CHEAPER ACCESS to the same ones.

	KAITO binance/hyperliquid   25.83 bps round trip   22.8h break-even
	KAITO lighter/hyperliquid   13.00 bps              13.0h

### ALSO

- cmd/decay: spread half-life ~12.1h median, but the curve was CENSORED --
  pairs vanish below the floor instead of appearing small. Non-monotonic as a
  result (91.6% remaining at 2h, 82.5% at 1h). Fixed by following pairs down
  for 6h after they stop qualifying: decay.jsonl. Changes no gate.
- Gate tally: 705 of 1068 assessments PASS. The filters were never the
  bottleneck -- ONE threshold at stage one removes 98.6% of pairs, and it was
  never reported until now.
- Maker-fee test: cutting fees and dropping the floor to 0.3 rescued 92 pairs,
  most of which then hit the volume floor. 88.5% of pairs are below 0.3 bps/hr.
  The distribution is flat with a thin tail; cheaper execution moves the
  threshold but there is nothing stacked behind it.
- Dashboard: book-level leverage scenarios for both strategies, with the
  liquidation rate shown beside every cross-venue row.
