#!/usr/bin/env python3
"""Is high funding predictable -- or do you only know a good month afterwards?

THE LAST QUESTION ON THE ONLY SURVIVING STRUCTURE

Static long-basis on majors at 5x returns 14.5%/yr median, with months ranging
from -21.8% to well over +40%, and 10 of 25 clearing 18%. If the good months
were foreseeable, a book that deploys only when funding is elevated and sits
flat otherwise would skip the losses and change the answer completely.

That hinges on one property: PERSISTENCE. If this month's funding says
something about next month's, a trailing filter works. If funding is
memoryless, the filter is an expensive way to be late -- it deploys after a
regime has already arrived and exits after it has already gone, paying a round
trip at both ends for the privilege.

NO LOOKAHEAD ANYWHERE. Every deploy decision uses a trailing window that ends
the day BEFORE the day being traded. That is the whole point: a filter tested
with even one day of hindsight will look brilliant and be worthless.

WHAT IS CHARGED

Entering or leaving the market costs a full round trip on notional, multiplied
by leverage. A filter that flips in and out weekly pays for the privilege, and
that cost is what separates a real regime filter from curve-fitting -- the
switching analysis already showed 3 flips a month turning a 26% gross into
-43.7%.

MEASURED FIRST: the lag-1 autocorrelation of monthly funding. If that is near
zero, no threshold will help and the table below is a formality.

Run from ~/vega-bot:  python3 regime_filter.py
"""

import collections
import json
import math
import os
import statistics
import sys
import time

CACHE = "data/history/funding_2y.json"
ROUNDTRIP_BPS = 33.0
LEV = 5.0
TARGET = 18.0
TRAIL_DAYS = 30
THRESHOLDS = [None, 0.00, 0.02, 0.04, 0.06, 0.08, 0.10]


def main():
    if not os.path.exists(CACHE):
        sys.exit("run fee_leverage_grid.py first to build %s" % CACHE)
    data = json.load(open(CACHE))

    # Daily mean SIGNED funding in bps/hr, averaged across the basket.
    # Signed, not absolute: a static long-basis book earns the sign.
    daily = collections.defaultdict(list)
    for sym, rows in data.items():
        for t, rate in rows:
            d = time.strftime("%Y-%m-%d", time.gmtime(t / 1000))
            daily[d].append(rate * 10000.0 / 8.0)
    days = sorted(daily)
    if len(days) < 200:
        sys.exit("not enough daily data")
    series = [statistics.mean(daily[d]) for d in days]

    # --- does funding remember anything? ---
    monthly = collections.defaultdict(list)
    for d, v in zip(days, series):
        monthly[d[:7]].append(v)
    mkeys = sorted(monthly)
    mvals = [statistics.mean(monthly[k]) for k in mkeys]
    if len(mvals) > 3:
        a, b = mvals[:-1], mvals[1:]
        ma, mb = statistics.mean(a), statistics.mean(b)
        num = sum((x - ma) * (y - mb) for x, y in zip(a, b))
        den = math.sqrt(sum((x - ma) ** 2 for x in a)
                        * sum((y - mb) ** 2 for y in b))
        ac = num / den if den else 0.0
        print("monthly funding, lag-1 autocorrelation: %+.3f" % ac)
        print("  (near 0 = memoryless, no filter can help;"
              " above ~0.4 = real persistence)")

    print("\n%d days, %s to %s, %d symbols"
          % (len(days), days[0], days[-1], len(data)))
    print("median daily funding: %.4f bps/hr\n" % statistics.median(series))

    print("%-11s %9s %13s %12s %11s %9s" %
          ("threshold", "deployed", "ret when in", "overall", "worst mo", "mo>=18%"))

    for th in THRESHOLDS:
        in_mkt = False
        cycles = 0
        # month -> [bps accrued, days deployed, days total]
        per_month = collections.defaultdict(lambda: [0.0, 0, 0])

        for i, d in enumerate(days):
            m = per_month[d[:7]]
            m[2] += 1
            if th is None:
                want = True
            else:
                lo = max(0, i - TRAIL_DAYS)
                if i == 0:
                    want = False
                else:
                    want = statistics.mean(series[lo:i]) >= th
            if want and not in_mkt:
                m[0] -= ROUNDTRIP_BPS       # pay to get in
                cycles += 1
                in_mkt = True
            elif not want and in_mkt:
                in_mkt = False              # exit cost folded into the entry
            if in_mkt:
                m[0] += series[i] * 24.0    # a day of funding, on notional
                m[1] += 1

        # Aggregate by TOTAL ACCRUAL, not by median month.
        #
        # A filter that sits out half the year makes more than half its months
        # exactly zero, so the median is zero no matter what the deployed
        # months earned. That is an artifact of the statistic, not a property
        # of the strategy, and it reported 60%-deployed books as returning
        # nothing at all.
        rets, total_bps, dep_days, tot_days = [], 0.0, 0, 0
        for k in sorted(per_month):
            bps, dd, td = per_month[k]
            if td < 20:
                continue
            rets.append(bps * 12.0 * LEV / 100.0)
            total_bps += bps
            dep_days += dd
            tot_days += td
        if not rets or not tot_days:
            continue

        frac = 100.0 * dep_days / tot_days
        years = tot_days / 365.0
        overall = total_bps / years * LEV / 100.0
        when_in = (total_bps / dep_days * 365.0 * LEV / 100.0) if dep_days else 0.0
        print("%-11s %8.0f%% %12.1f%% %11.1f%% %10.1f%% %8d" %
              ("always" if th is None else "%.2f bps/hr" % th,
               frac, when_in, overall, min(rets),
               sum(1 for x in rets if x >= TARGET)))

    print("\nAll figures at %gx leverage, static long-basis, %.0f bps round trip"
          % (LEV, ROUNDTRIP_BPS))
    print("charged on every entry. 'overall' counts flat months as zero,")
    print("which is what your capital actually earns while sitting out.")
    print("\nIf no threshold beats 'always' on BOTH overall return and worst")
    print("month, funding is not predictable at this horizon and the honest")
    print("answer is that the good months can only be recognised afterwards.")


if __name__ == "__main__":
    main()
