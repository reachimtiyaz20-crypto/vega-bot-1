#!/usr/bin/env python3
"""Scale leverage with the regime instead of switching in and out.

THE SIGNAL WE ALREADY FOUND AND THEN THREW AWAY

Monthly funding has lag-1 autocorrelation +0.592 -- strongly persistent. The
regime filter proved the signal is tradeable: at a 0.10 bps/hr threshold it
returned 63.7%/yr WHILE DEPLOYED. But it deployed only 8% of the time, so the
overall figure collapsed to 4.9%. Binary in-or-out wastes a real signal by
sitting in cash through everything below the threshold.

Scaling leverage keeps you deployed always -- no idle drag -- and largest
exactly when the carry is richest. This tests whether that bridges 17% toward
30%, or whether the extra leverage simply amplifies the bad months enough to
cancel the gain.

COMPOUNDING, NOT AVERAGES

Every earlier analysis reported median or mean annualised return. That flatters
a volatile book: -50% followed by +50% averages zero and leaves you down 25%.
This one runs an equity curve, so a schedule that earns more on average and
still ends poorer is visible as exactly that. Final multiple and max drawdown
are the numbers that matter; the averages are shown only for continuity.

WHAT CHANGING LEVERAGE COSTS

Moving from 3x to 8x means buying 5x your capital in new notional, and that
trades. Charged at 33 bps of the notional CHANGE, which is 33 x delta-leverage
in bps of capital. A schedule that flips tiers constantly pays for it here --
the same mechanism that turned 26% gross into -43.7% in the switching test.

WHAT IS STILL NOT MODELLED, and at 10x it dominates everything above

  liquidation        a basis dislocation at 10x ends the book. Nothing here
                     models the tail that actually kills levered basis desks.
  margin calls       drawdowns force deleveraging at the worst moment, which
                     locks in losses this smooth simulation never takes.
  funding caps       exchanges cap funding in extremes, truncating exactly the
                     upside the high tiers exist to capture.

Treat high-leverage rows as an upper bound on a strategy nobody should run at
that size without a risk system this project does not have.

Run from ~/vega-bot:  python3 variable_leverage.py
"""

import collections
import json
import os
import statistics
import sys
import time

CACHE = "data/history/funding_2y.json"
ROUNDTRIP_BPS = 33.0
TRAIL_DAYS = 30

# (name, [(trailing_funding_threshold, leverage), ...]) -- first match from top
SCHEDULES = [
    ("fixed 3x", [(-9e9, 3.0)]),
    ("fixed 5x", [(-9e9, 5.0)]),
    ("fixed 8x", [(-9e9, 8.0)]),
    ("fixed 10x", [(-9e9, 10.0)]),
    ("var 3/5/8", [(0.06, 8.0), (0.02, 5.0), (-9e9, 3.0)]),
    ("var 2/5/10", [(0.06, 10.0), (0.02, 5.0), (-9e9, 2.0)]),
    ("var 3/6/12", [(0.08, 12.0), (0.03, 6.0), (-9e9, 3.0)]),
    ("var 0/5/10", [(0.06, 10.0), (0.02, 5.0), (-9e9, 0.0)]),
]


def lev_for(sched, trailing):
    for th, lev in sched:
        if trailing >= th:
            return lev
    return 0.0


def main():
    if not os.path.exists(CACHE):
        sys.exit("run fee_leverage_grid.py first to build %s" % CACHE)
    data = json.load(open(CACHE))

    daily = collections.defaultdict(list)
    for sym, rows in data.items():
        for t, rate in rows:
            d = time.strftime("%Y-%m-%d", time.gmtime(t / 1000))
            daily[d].append(rate * 10000.0 / 8.0)
    days = sorted(daily)
    series = [statistics.mean(daily[d]) for d in days]
    if len(days) < 200:
        sys.exit("not enough daily data")

    print("%d days, %s to %s, %d symbols" % (len(days), days[0], days[-1], len(data)))
    print("median daily funding %.4f bps/hr\n" % statistics.median(series))

    print("%-12s %9s %8s %10s %11s %9s %8s %8s" %
          ("schedule", "final", "CAGR", "worst mo", "max dd", "changes",
           "mo>=18%", "mo>=35%"))

    for name, sched in SCHEDULES:
        equity = 1.0
        peak = 1.0
        maxdd = 0.0
        cur_lev = None
        month_bps = collections.defaultdict(float)
        changes = 0

        for i, d in enumerate(days):
            lo = max(0, i - TRAIL_DAYS)
            trailing = statistics.mean(series[lo:i]) if i > 0 else 0.0
            lev = lev_for(sched, trailing)
            if cur_lev is None:
                # Building the initial position is a real cost too.
                month_bps[d[:7]] -= ROUNDTRIP_BPS * lev
                cur_lev = lev
                changes += 1
            elif lev != cur_lev:
                month_bps[d[:7]] -= ROUNDTRIP_BPS * abs(lev - cur_lev)
                cur_lev = lev
                changes += 1
            month_bps[d[:7]] += series[i] * 24.0 * lev

        rets = []
        for k in sorted(month_bps):
            r = month_bps[k] / 10000.0        # fraction of capital, that month
            rets.append(r)
            equity *= (1.0 + r)
            peak = max(peak, equity)
            if peak > 0:
                maxdd = max(maxdd, (peak - equity) / peak)

        years = len(days) / 365.0
        cagr = (equity ** (1.0 / years) - 1.0) * 100.0 if equity > 0 else -100.0
        ann = [r * 12.0 * 100.0 for r in rets]
        print("%-12s %8.2fx %7.1f%% %9.1f%% %10.1f%% %8d %8d %8d" %
              (name, equity, cagr, min(ann), maxdd * 100.0, changes,
               sum(1 for x in ann if x >= 18.0),
               sum(1 for x in ann if x >= 35.0)))

    print("\nfinal   = equity multiple after 2 years, compounded, from 1.00x")
    print("CAGR    = compound annual growth. THE number. Beats any average.")
    print("worst mo/max dd are on capital, and at high leverage a max drawdown")
    print("near 100%% means the book was gone -- CAGR beside it is fiction.")
    print("\nA schedule is only better if it raises CAGR WITHOUT pushing max")
    print("drawdown somewhere you could not sit through. 40%% CAGR with a 60%%")
    print("drawdown is not a strategy anyone finances with borrowed money.")


if __name__ == "__main__":
    main()
