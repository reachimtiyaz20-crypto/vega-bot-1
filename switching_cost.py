#!/usr/bin/env python3
"""How much of the levered return depends on perfectly timed, free side-switching?

THE OPTIMISM BEING TESTED

fee_leverage_grid.py reported 23.1%/yr at 5x, 22 of 25 months clearing 18%. It
computed that from mean |funding| -- the absolute value -- which quietly
assumes the book is always positioned on the paying side and can flip between
cash-and-carry and reverse carry instantly and for free.

The %neg column says how much work that assumption does: February 2026 was
57.5% negative, March 52.0%, June 42.4%. A book sitting statically long-basis
PAYS funding through those periods rather than earning it. And every real flip
costs a full round trip on both legs, multiplied by leverage.

So this compares three books over the same two years:

  STATIC LONG    long spot / short perp, always. Earns positive funding, pays
                 negative. The honest floor, and what most people actually run.
  PERFECT SWITCH what the grid assumed: always on the right side, no switch
                 cost, perfect foresight. The ceiling. Not achievable.
  REALISTIC      switches only after the sign has PERSISTED, and pays a round
                 trip on both legs each time it does. Between the two, and the
                 only one worth planning against.

The persistence rule matters. Flipping on a single adverse settlement is the
twitchy behaviour that produced 13 stop-losses at 1.5-hour holds in the
archive; it converts noise into cost. REALISTIC waits for confirmation, which
means it eats some adverse funding before acting -- as any real book would.

STILL NOT MODELLED: liquidation, basis dislocation, margin silos, venue
failure. At 5x those decide the outcome and nothing here speaks to them.

Run from ~/vega-bot:  python3 switching_cost.py
"""

import collections
import json
import os
import statistics
import sys
import time

CACHE = "data/history/funding_2y.json"
ROUNDTRIP_BPS = 33.0
TARGET = 18.0
LEVERAGE = [("unlevered", 0.5), ("PM 1.11x", 0.9), ("2x", 2.0),
            ("3x", 3.0), ("5x", 5.0), ("8x", 8.0)]
BORROW_ANNUAL = {"BTC": 0.4, "ETH": 1.0, "SOL": 3.0, "BNB": 3.7,
                 "XRP": 3.0, "DOGE": 3.6}

# Settlements the sign must hold before the book flips. 3 x 8h = one day of
# agreement before paying to reposition.
PERSIST = 3


def base(s):
    return s[:-4] if s.endswith("USDT") else s


def main():
    if not os.path.exists(CACHE):
        sys.exit("run fee_leverage_grid.py first to build %s" % CACHE)
    data = json.load(open(CACHE))

    # Per symbol, walk the settlements in order and accrue three books.
    # bps accrued are per-NOTIONAL; leverage is applied at the end.
    per_month = collections.defaultdict(
        lambda: {"static": 0.0, "perfect": 0.0, "real": 0.0, "n": 0,
                 "flips": 0})

    for sym, rows in data.items():
        rows = sorted(rows)
        b_hr = BORROW_ANNUAL.get(base(sym), 5.0) * 100.0 / 8760.0
        borrow_per_settle = b_hr * 8.0          # 8h between settlements

        side = 1          # REALISTIC book starts long-basis
        run = 0           # consecutive settlements favouring the other side
        for t, rate in rows:
            bps = rate * 10000.0                # per settlement, per notional
            ym = time.strftime("%Y-%m", time.gmtime(t / 1000))
            m = per_month[ym]
            m["n"] += 1

            # STATIC: always long-basis. Sign of funding is the sign of income.
            m["static"] += bps

            # PERFECT: always right, no cost. Pays borrow when short spot.
            m["perfect"] += abs(bps) - (borrow_per_settle if bps < 0 else 0.0)

            # REALISTIC: hold the current side, flip only after PERSIST
            # settlements of disagreement, paying a round trip to do it.
            earn = bps * side - (borrow_per_settle if side < 0 else 0.0)
            m["real"] += earn

            want = 1 if bps >= 0 else -1
            if want != side:
                run += 1
                if run >= PERSIST:
                    side = want
                    run = 0
                    m["real"] -= ROUNDTRIP_BPS
                    m["flips"] += 1
            else:
                run = 0

    keys = sorted(k for k in per_month if per_month[k]["n"] >= 30)
    if not keys:
        sys.exit("not enough monthly data")

    nsym = len(data)
    print("%d months, %s to %s, %d symbols\n" % (len(keys), keys[0], keys[-1], nsym))

    def annualise(total_bps, months_n, lev):
        """bps accrued across all symbols in a month -> %/yr on capital."""
        per_sym = total_bps / nsym            # average symbol
        return per_sym * 12.0 * lev / 100.0   # -> percent per year

    print("MEDIAN ANNUALISED RETURN ON CAPITAL")
    print("%-14s %10s %10s %10s" % ("leverage", "static", "realistic", "perfect"))
    med = {}
    for lname, lev in LEVERAGE:
        vals = {b: sorted(annualise(per_month[k][b], 1, lev) for k in keys)
                for b in ("static", "real", "perfect")}
        med[lname] = vals
        print("%-14s %9.1f%% %9.1f%% %9.1f%%" %
              (lname,
               statistics.median(vals["static"]),
               statistics.median(vals["real"]),
               statistics.median(vals["perfect"])))

    print("\nMONTHS OF %d CLEARING %.0f%%/yr" % (len(keys), TARGET))
    print("%-14s %10s %10s %10s" % ("leverage", "static", "realistic", "perfect"))
    for lname, lev in LEVERAGE:
        v = med[lname]
        print("%-14s %10d %10d %10d" %
              (lname,
               sum(1 for x in v["static"] if x >= TARGET),
               sum(1 for x in v["real"] if x >= TARGET),
               sum(1 for x in v["perfect"] if x >= TARGET)))

    print("\nWORST MONTH at 5x")
    v = med["5x"]
    for b, label in (("static", "static long"), ("real", "realistic"),
                     ("perfect", "perfect switch")):
        worst = min(v[b])
        k = [kk for kk in keys
             if abs(annualise(per_month[kk][b], 1, 5.0) - worst) < 1e-9]
        print("  %-16s %8.1f%%/yr  (%s)" % (label, worst, k[0] if k else "?"))

    flips = sum(per_month[k]["flips"] for k in keys)
    print("\n%d side changes across %d months (%.1f per month per symbol)"
          % (flips, len(keys), flips / len(keys) / nsym))
    print("Each costs %.0f bps on notional, multiplied by leverage." % ROUNDTRIP_BPS)

    print("\nTHE NUMBER THAT MATTERS is the REALISTIC column. Static is what a")
    print("book that never adapts earns; perfect is the fiction the earlier")
    print("grid priced. If realistic sits far below perfect, the levered case")
    print("rests on execution nobody achieves.")


if __name__ == "__main__":
    main()
