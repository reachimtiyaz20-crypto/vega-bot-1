#!/usr/bin/env python3
"""Lever into the spread, not into the leverage.

THE MISTAKE THIS CORRECTS

Two separate errors, both mine, pointing the same direction.

First: every leverage figure I produced assumed borrowing was free. It is not.
The spot leg of a cash-and-carry must be BOUGHT, so holding more notional than
you have capital means borrowing dollars. Measured on Bybit 2026-08-26: USDT
borrows at 4.45%/yr.

Second: I tested a regime rule that REDUCED exposure in bad conditions, and
concluded regime timing was worthless. The interesting question is the mirror
of it -- whether to INCREASE exposure in good conditions.

Both matter because of one number:

	funding  5.27%/yr, borrow 4.45%/yr  ->  spread  0.82%  ->  leverage is pointless
	funding 20.00%/yr, borrow 4.45%/yr  ->  spread 15.55%  ->  leverage is the trade

Ethena's realised yield across 2024-25 ran 4% to 30%, and funding peaked near
+75% in early 2024 against -6% in late 2022. The 35-40% returns people quote
are not a different strategy. They are THIS strategy during a wide-spread
regime, and today's compressed reading is the same strategy in a thin one.

Funding has lag-1 monthly autocorrelation of +0.592, so regimes persist and are
recognisable while they are happening. That is what makes scaling into them
possible rather than merely desirable.

THE DOMINANT UNCERTAINTY, STATED PLAINLY

There is no public history of USDT borrow rates. We have exactly ONE
measurement: 4.45%/yr today, when funding was 5.27%. Borrow and funding are
both prices for dollar leverage in crypto and are driven by the same demand --
so when funding spiked to 75%, borrow almost certainly rose too, and assuming
it stayed at 4.45% would invent an edge that never existed.

So borrow is SWEPT, not assumed:

	fixed        4.45% regardless of regime -- the optimistic bound
	half-beta    rises by half of any excess funding above today's level
	proportional keeps today's ratio of 0.84 x funding -- the pessimistic bound
	fixed 10%    a plainly higher flat rate

If the strategy only works under 'fixed', it does not work. The regimes it needs
are precisely the regimes in which borrowing gets dearer.

BENCHMARK: doing nothing. USDT lending at 4.45% is the opportunity cost, and
any result below it means the whole exercise was a way of taking risk for free.

Run from ~/vega-bot:  python3 spread_leverage.py
"""

import collections
import json
import os
import statistics
import sys
import time

CACHE = "data/history/funding_5y.json"
ROUNDTRIP_BPS = 33.0
TRAIL_DAYS = 14
BORROW_TODAY = 4.45          # %/yr, measured on Bybit 2026-08-26
FUNDING_TODAY = 5.27         # %/yr on notional, measured over 5 years
LEND_RATE = 4.45             # %/yr, the do-nothing benchmark

# Leverage as a function of trailing SPREAD (funding minus borrow), %/yr.
# Flat below zero: a negative spread means paying to hold the position.
TIERS = [(25.0, 8.0), (10.0, 5.0), (2.0, 3.0), (0.0, 1.0), (-1e9, 0.0)]


def lev_for(spread):
    for th, l in TIERS:
        if spread >= th:
            return l
    return 0.0


def borrow_for(mode, funding_pct):
    """Annual borrow %, given annualised funding % and a modelling choice."""
    if mode == "fixed":
        return BORROW_TODAY
    if mode == "fixed10":
        return 10.0
    if mode == "half":
        # Rises by half of whatever funding exceeds today's level.
        return BORROW_TODAY + 0.5 * max(0.0, funding_pct - FUNDING_TODAY)
    if mode == "prop":
        # Keeps today's observed ratio. Pessimistic and probably closest to
        # how a lending market actually behaves.
        return max(BORROW_TODAY, (BORROW_TODAY / FUNDING_TODAY) * funding_pct)
    raise ValueError(mode)


def run(days, series, mode, fixed_lev=None):
    """series: daily mean funding in bps/hr (signed). Returns monthly returns."""
    month = collections.defaultdict(float)
    cur = None
    changes = 0
    tier_days = collections.Counter()

    for i, d in enumerate(days):
        ym = d[:7]
        # Trailing window ends the day BEFORE the day traded. No lookahead.
        if i < TRAIL_DAYS:
            trail_bps_hr = series[i]
        else:
            trail_bps_hr = statistics.mean(series[i - TRAIL_DAYS:i])
        trail_pct = trail_bps_hr * 8760.0 / 100.0        # -> %/yr on notional
        b_pct = borrow_for(mode, max(trail_pct, 0.0))
        spread = trail_pct - b_pct

        lev = fixed_lev if fixed_lev is not None else lev_for(spread)
        tier_days[lev] += 1

        if cur is None:
            month[ym] -= ROUNDTRIP_BPS * lev
            cur = lev
            changes += 1
        elif lev != cur:
            month[ym] -= ROUNDTRIP_BPS * abs(lev - cur)
            cur = lev
            changes += 1

        # Today's actual funding, on notional; borrow charged on the part of
        # the notional that is not covered by capital.
        f_day_bps = series[i] * 24.0
        b_day_bps = borrow_for(mode, max(series[i] * 8760.0 / 100.0, 0.0)) * 100.0 / 365.0
        month[ym] += lev * f_day_bps - max(lev - 1.0, 0.0) * b_day_bps

    return month, changes, tier_days


def curve(month, ndays):
    keys = sorted(month)
    eq, peak, dd = 1.0, 1.0, 0.0
    rets = []
    for k in keys:
        r = month[k] / 10000.0
        rets.append((k, r))
        eq *= (1.0 + r)
        if eq <= 0:
            return 0.0, -100.0, 100.0, rets
        peak = max(peak, eq)
        dd = max(dd, (peak - eq) / peak)
    yrs = ndays / 365.0
    return eq, (eq ** (1.0 / yrs) - 1.0) * 100.0, dd * 100.0, rets


def main():
    if not os.path.exists(CACHE):
        sys.exit("run longrun_stress.py first to build %s" % CACHE)
    data = json.load(open(CACHE))

    daily = collections.defaultdict(list)
    for sym, rows in data.items():
        for t, rate in rows:
            d = time.strftime("%Y-%m-%d", time.gmtime(t / 1000))
            daily[d].append(rate * 10000.0 / 8.0)
    days = sorted(daily)
    series = [statistics.mean(daily[d]) for d in days]
    print("%d days, %s to %s, %d symbols" % (len(days), days[0], days[-1], len(data)))
    ann = [s * 8760.0 / 100.0 for s in series]
    print("funding %%/yr on notional: median %.1f  p90 %.1f  max %.1f  min %.1f\n"
          % (statistics.median(ann), sorted(ann)[int(0.9 * len(ann))],
             max(ann), min(ann)))

    print("BENCHMARK: USDT lending at %.2f%%/yr, no leverage, no drawdown\n" % LEND_RATE)

    for mode, label in (("fixed", "borrow fixed 4.45%"),
                        ("half", "borrow +half of excess funding"),
                        ("prop", "borrow proportional (0.84 x funding)"),
                        ("fixed10", "borrow fixed 10%")):
        print("=== %s ===" % label)
        print("%-22s %8s %8s %9s %8s" % ("strategy", "final", "CAGR", "max dd", "changes"))
        for name, fl in (("fixed 1x", 1.0), ("fixed 5x", 5.0), ("spread-scaled", None)):
            m, ch, tiers = run(days, series, mode, fixed_lev=fl)
            eq, cagr, dd, _ = curve(m, len(days))
            beats = "  <-- beats lending" if cagr > LEND_RATE else ""
            print("%-22s %7.2fx %7.1f%% %8.1f%% %8d%s"
                  % (name, eq, cagr, dd, ch, beats))
            if fl is None:
                tot = sum(tiers.values())
                dist = "  ".join("%gx:%.0f%%" % (k, 100.0 * v / tot)
                                 for k, v in sorted(tiers.items()))
                print("%-22s %s" % ("  time at each tier", dist))
        print()

    print("HOW TO READ THIS")
    print("  The only row that matters is whether spread-scaled beats LENDING")
    print("  under the PESSIMISTIC borrow assumptions, not the optimistic one.")
    print("  Borrow and funding are the same price viewed from two sides; a")
    print("  model where funding triples and borrowing does not is a model of")
    print("  a market that does not exist.")
    print("\n  If it only wins under 'fixed 4.45%%', the honest conclusion is")
    print("  that the 2024-style returns belonged to people who borrowed before")
    print("  the regime arrived, not to a rule anyone can run forward.")


if __name__ == "__main__":
    main()
