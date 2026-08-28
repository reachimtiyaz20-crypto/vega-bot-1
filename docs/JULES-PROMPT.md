# PROJECT VEGA — brief for an engineering agent

## What this is

A funding-rate arbitrage **measurement engine** in Go, running paper-only on a single VPS. It scans perpetual futures across seven venues, finds where the same asset is funded differently, prices what capturing that difference would cost against live order books, and books results against real settlement timestamps.

No real capital has ever been deployed.

## The one rule

**Measure, never assume. Refuse, never default.**

Every venue parameter — contract size, funding interval, settlement calendar, taker fee, minimum notional — is read from the venue. Where a value cannot be read the symbol is **refused**, not given a plausible default. Fifteen named gates enforce this.

This is not stylistic. Every serious defect here has been a defaulted value:

| defect | magnitude |
|---|---|
| Funding interval assumed 8h venue-wide; Binance and Bybit set it per symbol | one pair read **75x richer** |
| OKX book depth read as base units, actually contracts | **100x** overstatement |
| MEXC same, contractSize 0.0001 | **10,000x** overstatement |
| Lighter quotes per 8h, settles hourly | **8x** rate error |
| OKX taker taken from the SPOT fee table (25 bps vs 5) | every OKX round trip **+40 bps** |
| An 8-hour default inside the tool built to measure interval errors | manufactured fake 2.0x and 8.1x venue "spreads" |

The last was introduced and caught within two hours, in a diagnostic written because of the first. **Assume this class of error is still present. Look for it.**

Three unit tests were found *asserting* defects rather than catching them. A green suite is not evidence.

## What is measured and trustworthy

| | |
|---|---|
| Fill cost | **median 42 bps** round trip at $400/leg, n=489 live book sweeps |
| Fill cost p90 (ex-OKX) | **102 bps** |
| Settled funding | six venues cross-validated; BTC reads 0.031-0.078 bps/hr on all |
| Cash-and-carry | **2.83%/yr gross**, ~**1%/yr net** at 30-day holds |
| Cross-venue settled spreads | p50 ~0.03 bps/hr; only 2-3 pairs of ~3,400 exceed 3 bps/hr at any moment |
| Spread half-life | ~12h nominal, **1.7h measured** on closed positions |

## What has been disproven

**Predicted funding is not a tradeable signal.** Across fifteen closed positions:

Funding is a time-weighted average premium over its interval. Early in an interval that average comes from a short noisy window, so the published prediction is an extreme that converges as the interval fills. Screening thousands of pairs for the WIDEST spread selects precisely where that estimator's error is largest, then differences two of them. There was no decaying edge; there was estimation error, and the screen sorted for it.

**Settled funding alone also fails — it lags.** BICO averaged 3.99 bps/hr across twelve settlements while the live spread had already fallen to 0.50. The episode ended before the mean revealed it.

Entry now requires the two to **agree**: wide across the last three settled intervals, still pointing the same way in the current prediction, holding sign across >=75% of settlements.

**The new-listing backtest failed out-of-sample.** It showed +44.9%/yr, but 99% of profit came from 3 coins of 408, and a split-half test gave +22.0% vs +0.8%. Reported as a negative result. Do not resurrect without new evidence.

## Your tasks, in priority order

### 1. Conservative net-edge gate
Replace the fixed threshold (`Book.MinSettledSpreadBpsHr`, default 3.0) with one expected-net test:

Use exactly this formula, with these CONSTANTS (not flags -- nothing here is
tunable):

`cost_total_bps` is ALREADY the complete round trip: both legs, in and out,
plus four taker fees. One cost term, not two.

Use `SettledRecentSpreadBpsHr` (last three settlements), NOT
`SettledSpreadBpsHr` (twelve-settlement mean). The mean lags: BICO averaged
3.99 bps/hr across twelve settlements while its live spread had already fallen
to 0.50.

Use exactly this formula, with these CONSTANTS. Nothing here is a flag and
nothing here is tunable:

    const expectedHoldHours = 8.0    // two 4-hour settlements
    const basisReserveBps   = 15.0   // drift tails -30.6..+22.5, median +0.9
    const borrowReserveBps  = 0.0    // cross-venue perp-perp borrows nothing
    const minCostSamples    = 40     // a p95 from fewer is an order statistic

    net = SettledRecentSpreadBpsHr * expectedHoldHours
        - p95(cost_total_bps for this venue pair)
        - basisReserveBps
        - borrowReserveBps

cost_total_bps is ALREADY the complete round trip: both legs, in and out, plus
four taker fees. One cost term, not two.

Use SettledRecentSpreadBpsHr (last three settlements), NOT SettledSpreadBpsHr
(twelve-settlement mean). The mean lags: BICO averaged 3.99 bps/hr across
twelve settlements while its live spread had already fallen to 0.50.

DO NOT multiply by the 1.7h half-life. It is neither a decay integral (that
would be halflife/ln2 = 2.45x) nor a holding period (1.7h is shorter than one
settlement interval, so it models collecting funding that never settles). And
any decay model is the wrong shape: of fifteen closed positions, SEVEN had
their spread reverse rather than decay. At a 42 bps p95 cost, a 1.7x multiplier
requires a 24.7 bps/hr spread to break even; the widest ever measured across
3,389 pairs was 8.5. That gate would refuse everything forever and read like a
finding.

Below `minCostSamples`, REFUSE with a distinct reason
(`GateCostUnmeasured = "COST_UNMEASURED"`) so it is countable. Do not
substitute a global figure -- the fix for a missing measurement is to go and
measure it.

Read costs from a path flag defaulting to `docs/evidence/fill-costs.jsonl`,
which exists in every checkout. Production points it at the live sampler
output.

Use exactly this formula, with these CONSTANTS (not flags -- nothing here is
tunable):

`cost_total_bps` is ALREADY the complete round trip: both legs, in and out,
plus four taker fees. One cost term, not two.

Use `SettledRecentSpreadBpsHr` (last three settlements), NOT
`SettledSpreadBpsHr` (twelve-settlement mean). The mean lags: BICO averaged
3.99 bps/hr across twelve settlements while its live spread had already fallen
to 0.50.

DO NOT multiply by the 1.7h half-life. It is neither a decay integral (that
would be halflife/ln2 = 2.45x) nor a holding period (1.7h is shorter than one
settlement interval, so it models collecting funding that never settles). And
any decay model is the wrong shape: of fifteen closed positions, SEVEN had
their spread reverse rather than decay. At a 42 bps p95 cost, a 1.7x multiplier
requires a 24.7 bps/hr spread to break even; the widest ever measured across
3,389 pairs was 8.5. That gate would refuse everything forever and read like a
finding.

Below `minCostSamples`, REFUSE with a distinct reason
(`GateCostUnmeasured = "COST_UNMEASURED"`) so it is countable. Do not
substitute a global figure -- the fix for a missing measurement is to go and
measure it.

Read costs from a path flag defaulting to `docs/evidence/fill-costs.jsonl`,
which exists in every checkout. Production points it at the live sampler
output.

Cost percentiles must come from measured data (`docs/evidence/fill-costs.jsonl`, 489+ live sweeps), per venue pair where sample size allows. Refuse a pair with too few measurements rather than falling back to a global figure.
Files: `pkg/crossvenue/book.go` (assess), `pkg/crossvenue/settled.go`.

### 2. Daily immutable P&L snapshot
No artifact currently answers "is this approaching its target". Write one append-only record per day: equity, allocated collateral, **idle collateral**, realised funding, fees, measured slippage, borrow, basis P&L, open-risk reserve, and **net return on total locked equity**. A 40%/yr target is roughly **9.2 bps/day net on total equity** — state it that way so it is checkable daily.

### 3. Portfolio capacity curve
Current reasoning is `40 candidates x $400 x 2 legs ~= $32,000`, which **double-counts** — the same book, asset and collateral appear across multiple pairs. Build: executable cost curves at $100/$250/$500/$1k/$2k/$5k; p50/p90/p99 costs, not visible depth; caps per market as a share of reliable depth and open interest; aggregate caps by venue, settlement asset, underlying, and correlated listing cohort. **Capacity = the largest capital level where the lower-confidence-bound portfolio return stays positive**, not where orders can be placed.

### 4. Price basis rather than gating it
Basis is measured and excluded from NetBps. Do NOT add a blunt max-basis filter — funding and basis are the same phenomenon measured two ways. Model three components: settled funding net; executable basis P&L from entry fills vs realistic exit fills; margin and failure cost. Bucket by asset, venue pair, listing age, liquidity, time-to-funding. Require funding plus expected basis P&L to clear costs at conservative percentiles. Note venues fund against different reference prices (Hyperliquid oracle, Lighter mark) — currently unhandled.

### 5. Correlated liquidation stress
Legs are evaluated independently. Stress them jointly: simultaneous basis blowout, mark/index divergence, funding debit, delayed exit, 30-120s venue outage. Per-venue collateral buffers must assume transfers are unavailable during stress.

### 6. Complete the held/entry split
`Builder.Build` mixes two concerns. Split into `BuildHeldPositions()` and `BuildNewEntries()`. **Entry criteria must never decide what is watched** — a position below the entry floor was once filtered out of its own feed and booked 2 of 8 due settlements. Behaviour is correct today via a `HeldCoins` bypass, but the coupling remains.

### 7. Config-drift alarms
A changed fee tier, funding interval, margin tier or contract multiplier must **halt new entries** until reviewed. `config/fees.json` is the fee source of truth and unit files assert against it; extend that pattern to every venue parameter.

## Anti-goals — read carefully

**Do not tune parameters to reach a target return.** The owner's goal is 35-40%/yr. Nothing measured supports it. Your task is NOT to produce that number; it is to determine whether any positive edge survives all costs and make it checkable. A configuration producing 40% in a backtest is a warning, not a result.

**Do not replace a refusal with a default.**

**Do not report a number you cannot trace to a venue's own record.** Predicted rates, quoted prices and estimated depths are inputs to a decision, never evidence of a result.

**Do not aggregate pre-fix and post-fix results.** The measurement clock resets at **2026-08-18 15:31 UTC** for cross-venue. `data/crossvenue-RETIRED-*` is audit only.

**Do not enable live execution.** `pkg/live` has partial-fill handling, a durable intent log and restart reconciliation, but has never touched a real venue. Treat `Config.Enabled` as off.

**Prefer deleting a strategy to rescuing it.** The most valuable output of the last three days was proving a signal did not work.

## How to verify your work

1. `go test ./...` must pass — but a green suite proved nothing here three times. For any behaviour you change, **write the test that fails against the old code first**.
2. Cross-check any venue parameter across venues. BTC funding reads 0.03-0.08 bps/hr everywhere; 8x or 100x off that band is a units error, not a market observation.
3. Integer-looking ratios between venues (2.00x, 3.00x, 8.00x) are almost always a normalisation bug. Two were found that way.
4. State confidence and sample size for every number. n=8 and n=800 are different claims.

## Layout
