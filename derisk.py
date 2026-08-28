#!/usr/bin/env python3
"""Don't reverse. Just get out of the way.

WHERE THE RISK ACTUALLY LIVES

Five years, static long-basis at 10x: 35.9% CAGR, 54.8% maximum drawdown. And
the FTX month alone was -48.51%. Essentially the ENTIRE five-year drawdown is
one month. The other fifty-nine were mild.

Reversing into that month works on paper -- it turns -48.51% into +22.93% --
and then loses everything back in friction: a sign-change rule flips about
thirty times in five years, two round trips each, 6.6% of capital per flip at
10x. Roughly 198% of capital consumed by turning.

So the reversal was the wrong shape. The book does not need to be RIGHT about
direction during a crisis. It only needs to be ABSENT.

WHY FLAT BEATS REVERSED

  triggers far less    "sign turned negative" fires ~30 times in five years.
                       "deeply negative for two weeks" fires a handful.
  needs no borrow      the assumption that killed the reversal -- borrow at
                       50%/yr in a crisis, or no lender at all -- simply does
                       not apply to a book holding cash.
  lower bar            being flat requires recognising DANGER, not predicting
                       direction. A much easier thing to be right about.

HYSTERESIS, deliberately

Exit when trailing funding drops below the threshold; re-enter only once it is
back above zero. Without that gap the book oscillates around a single level and
pays to do it -- the same defect that made the 1-day switch lose 100%.

NO LOOKAHEAD. Trailing windows end on the day being evaluated, and the position
change applies from that day forward.

WHAT THIS STILL DOES NOT MODEL: basis dislocation, liquidation paths inside a
month, and venue failure. FTX did not merely have bad funding; it stopped
existing. A book flat in cash on a dead exchange is not safe either.

Run from ~/vega-bot:  python3 derisk.py
"""

import collections
import json
import os
import statistics
import sys
import time

CACHE = "data/history/funding_5y.json"
ROUNDTRIP_BPS = 33.0
LEVS = [5.0, 8.0, 10.0]
THRESHOLDS = [-0.02, -0.05, -0.10, -0.20]
PERSISTS = [7, 14]
FTX = "2022-11"


def simulate(series, days, lev, thresh, persist):
    """Long-basis by default; flat while trailing funding is below thresh."""
    month = collections.defaultdict(float)
    month[days[0][:7]] -= ROUNDTRIP_BPS * lev      # build initial position
    invested = True
    exits = 0
    flat_days = 0

    for i, d in enumerate(days):
        ym = d[:7]
        if invested:
            month[ym] += series[i] * 24.0 * lev
        else:
            flat_days += 1

        if i + 1 < persist:
            continue
        trail = statistics.mean(series[i + 1 - persist:i + 1])
        if invested and trail < thresh:
            invested = False
            exits += 1
            month[ym] -= ROUNDTRIP_BPS * lev        # cost to get out
        elif not invested and trail > 0.0:
            invested = True
            month[ym] -= ROUNDTRIP_BPS * lev        # cost to get back in
    return month, exits, flat_days


def curve(month, days):
    keys = sorted(month)
    equity, peak, maxdd = 1.0, 1.0, 0.0
    rets = []
    for k in keys:
        r = month[k] / 10000.0
        rets.append((k, r))
        equity *= (1.0 + r)
        if equity <= 0:
            return 0.0, -100.0, 100.0, rets
        peak = max(peak, equity)
        maxdd = max(maxdd, (peak - equity) / peak)
    years = len(days) / 365.0
    return equity, (equity ** (1.0 / years) - 1.0) * 100.0, maxdd * 100.0, rets


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
    print("%d days, %s to %s\n" % (len(days), days[0], days[-1]))

    for lev in LEVS:
        print("=== %gx leverage ===" % lev)
        print("%-20s %8s %8s %9s %10s %7s %7s" %
              ("rule", "final", "CAGR", "max dd", "FTX month", "exits", "%flat"))

        # baseline: never leave
        m, _, _ = simulate(series, days, lev, -9e9, 7)
        eq, cagr, dd, rets = curve(m, days)
        base_cagr, base_dd = cagr, dd
        print("%-20s %7.2fx %7.1f%% %8.1f%% %9.2f%% %7s %7s" %
              ("static long", eq, cagr, dd, dict(rets).get(FTX, 0) * 100, "-", "0%"))

        for p in PERSISTS:
            for th in THRESHOLDS:
                m, ex, fd = simulate(series, days, lev, th, p)
                eq, cagr, dd, rets = curve(m, days)
                ftx = dict(rets).get(FTX, 0.0) * 100.0
                better = ""
                if cagr >= base_cagr and dd <= base_dd:
                    better = "  <-- better on both"
                print("%-20s %7.2fx %7.1f%% %8.1f%% %9.2f%% %7d %6.0f%%%s" %
                      ("flat<%.2f, %dd" % (th, p), eq, cagr, dd, ftx, ex,
                       100.0 * fd / len(days), better))
        print()

    print("A rule earns its place only by raising CAGR AND cutting max")
    print("drawdown. Cutting drawdown alone is available for free by holding")
    print("less leverage, and that costs nothing in complexity or in trades")
    print("that can go wrong at the worst moment.")
    print("\nCompare any winner against simply running LOWER LEVERAGE with no")
    print("rule at all: 5x static gave 18.7%/yr at 29.0%% drawdown. If a")
    print("10x-with-rule lands near that, the rule bought nothing but risk of")
    print("its own -- a signal that can misfire in a crisis.")


if __name__ == "__main__":
    main()
