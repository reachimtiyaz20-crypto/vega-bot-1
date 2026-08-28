#!/usr/bin/env python3
"""Flip on regimes, not on noise. Does that turn FTX from -48% into +48%?

THE PARAMETER I CHOSE BADLY

An earlier test concluded that harvesting both directions loses money: -43.7%
at 5x, because the book flipped three times a month and paid a round trip each
time. That conclusion was drawn with PERSIST = 3 settlements -- ONE DAY. A book
that changes its mind daily is not trading regimes, it is trading noise, and
noise costs 33 bps a turn.

FTX was not noise. It was a month where funding stayed deeply negative, and a
long-basis book lost 48.51% of capital at 10x. The mirror position -- short
spot, long perp -- would have collected roughly the same magnitude.

So the question is whether a SLOW rule, one that ignores chatter and only
reverses when negative funding has persisted for a week or two, converts the
single worst month into the single best one. If it does, the 54.8% drawdown
that makes 10x uninvestable largely disappears, and the whole risk profile
changes.

NO LOOKAHEAD. The trailing window ends the day BEFORE the day being traded.

BORROW IS CHARGED ON THE SHORT-SPOT SIDE, and this is the assumption most
likely to be wrong. Majors borrow cheaply today -- BTC at 0.4%/yr, ETH 1.0% --
but borrow is not a constant. It spikes when everyone wants to short, which is
exactly when this book wants to be short, and during a genuine crisis the coin
may not be lendable at all. There is no public five-year history of borrow
rates, so today's rates are all we have. The sensitivity table at the bottom
charges 3%, 15% and 50%/yr to show how much the answer depends on it.

IF THE ANSWER ONLY WORKS AT 3%/yr BORROW, IT DOES NOT WORK. The months this
strategy needs to capture are precisely the months borrow is expensive.

STILL UNMEASURED: basis dislocation, liquidation paths within a month, and
whether the venue survives. FTX itself did not.

Run from ~/vega-bot:  python3 slow_switch.py
"""

import collections
import json
import os
import statistics
import sys
import time

CACHE = "data/history/funding_5y.json"
ROUNDTRIP_BPS = 33.0
PERSISTS = [1, 3, 7, 14, 30]
LEVS = [3.0, 5.0, 8.0, 10.0]
BORROW_TIERS = [3.0, 15.0, 50.0]      # annual percent on the short-spot leg
FTX = "2022-11"


def run(series, days, persist, lev, borrow_annual, allow_flip=True):
    """Return (monthly returns dict, flips). side +1 long basis, -1 reverse."""
    borrow_hr = borrow_annual * 100.0 / 8760.0     # bps per hour
    month = collections.defaultdict(float)
    side, flips = 1, 0
    month[days[0][:7]] -= ROUNDTRIP_BPS * lev      # build the initial position

    for i, d in enumerate(days):
        ym = d[:7]
        # Accrue at today's funding, on the side held coming into today.
        earn = series[i] * 24.0 * side
        if side < 0:
            earn -= borrow_hr * 24.0               # short spot pays to borrow
        month[ym] += earn * lev

        if not allow_flip:
            continue
        lo = max(0, i + 1 - persist)
        if i + 1 < persist:
            continue
        trail = statistics.mean(series[lo:i + 1])
        want = 1 if trail >= 0 else -1
        if want != side:
            side = want
            flips += 1
            # Closing one structure and opening the other: two round trips.
            month[ym] -= 2.0 * ROUNDTRIP_BPS * lev
    return month, flips


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
    cagr = (equity ** (1.0 / years) - 1.0) * 100.0
    return equity, cagr, maxdd * 100.0, rets


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
        print("=== %gx leverage, borrow %.0f%%/yr on the short side ==="
              % (lev, BORROW_TIERS[0]))
        print("%-14s %8s %8s %9s %10s %8s" %
              ("rule", "final", "CAGR", "max dd", "FTX month", "flips"))

        m, _ = run(series, days, 0, lev, BORROW_TIERS[0], allow_flip=False)
        eq, cagr, dd, rets = curve(m, days)
        ftx = dict(rets).get(FTX, 0.0) * 100.0
        print("%-14s %7.2fx %7.1f%% %8.1f%% %9.2f%% %8s"
              % ("static long", eq, cagr, dd, ftx, "-"))

        for p in PERSISTS:
            m, fl = run(series, days, p, lev, BORROW_TIERS[0])
            eq, cagr, dd, rets = curve(m, days)
            ftx = dict(rets).get(FTX, 0.0) * 100.0
            print("%-14s %7.2fx %7.1f%% %8.1f%% %9.2f%% %8d"
                  % ("switch %dd" % p, eq, cagr, dd, ftx, fl))
        print()

    print("BORROW SENSITIVITY -- 14-day rule, 5x")
    print("%-14s %8s %8s %9s %10s" %
          ("borrow", "final", "CAGR", "max dd", "FTX month"))
    for b in BORROW_TIERS:
        m, _ = run(series, days, 14, 5.0, b)
        eq, cagr, dd, rets = curve(m, days)
        ftx = dict(rets).get(FTX, 0.0) * 100.0
        print("%-14s %7.2fx %7.1f%% %8.1f%% %9.2f%%"
              % ("%.0f%%/yr" % b, eq, cagr, dd, ftx))

    print("\nHOW TO READ THIS")
    print("  A slow rule wins only if it raises CAGR AND cuts max drawdown AND")
    print("  survives expensive borrow. Winning on one of the three is not")
    print("  enough -- the months it needs to capture are exactly the months")
    print("  borrow is dearest and lenders disappear.")
    print("  FTX month is the single test that matters: static long lost")
    print("  48.51%% of capital at 10x. If a rule turns that positive, the")
    print("  drawdown that makes leverage uninvestable largely goes away.")


if __name__ == "__main__":
    main()
