#!/usr/bin/env python3
"""What a position would ACTUALLY have collected, entering hot and exiting cold.

funding_at_50.py found 27.9% of hot observations tradeable at $50 rather than
0.04% at $400. That part stands: it is a mechanical consequence of the touch
requirement falling from $100 to $12.50.

Its magnitude figures do not stand, for two reasons.

FIRST, IT EXTRAPOLATED AN INSTANT OVER 30 DAYS. net30 was rate x 24 x 30 minus
cost, using the episode's PEAK rate. COTI's episode lasted 427 hours, not 720,
and it did not hold its peak throughout. +78,840 bps was not earnable by anyone.
Here funding is accrued at the rate actually observed, for the time it actually
persisted, and one round trip is subtracted.

SECOND, IT IGNORED THE SIGN. Taking abs() assumes negative funding is harvestable.
It is not: that needs short spot, which needs borrow, and borrow covers four
currencies. Positive and negative are reported separately, and only positive is
cash-and-carry.

Accrual uses the rate observed BEFORE each interval, not after -- the same
discipline the settlement code follows, and for the same reason: crediting the
next interval's rate to the one that just ended flatters the result.

Run from ~/vega-bot:  nice -n 10 python3 funding_realizable.py
"""

import collections
import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"
BASELINE_BPS_HR = 0.125
ENTER_BPS_HR = 4 * BASELINE_BPS_HR
MAX_GAP_MS = 45 * 60 * 1000

NOTIONAL = 50.0
MIN_TOP_FRACTION = 0.25
MIN_VOL_USD = 1_000_000.0
COST_RATIO_50 = 0.78

# Capital is 2x notional for unlevered cash-and-carry, so return on capital is
# half the return on notional.
CAPITAL_MULTIPLE = 2.0


def pct(v, q):
    if not v:
        return float("nan")
    i = int(round(q * (len(v) - 1)))
    return v[max(0, min(i, len(v) - 1))]


def files():
    return (sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
            + sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl"))))


def opener(p):
    return (gzip.open(p, "rt", encoding="utf-8", errors="replace")
            if p.endswith(".gz")
            else open(p, "rt", encoding="utf-8", errors="replace"))


def tradeable(r, need_touch):
    if not r.get("spot_available") or not r.get("liq_measured"):
        return False
    if (r.get("perp_vol_24h_usd") or 0) < MIN_VOL_USD:
        return False
    if (r.get("spot_vol_24h_usd") or 0) < MIN_VOL_USD:
        return False
    if (r.get("spot_top_usd") or 0) < need_touch:
        return False
    if (r.get("perp_top_usd") or 0) < need_touch:
        return False
    return True


def main():
    if not os.path.isdir(JOURNAL):
        sys.exit("run from ~/vega-bot")
    need_touch = NOTIONAL * MIN_TOP_FRACTION

    state = {}
    done = []          # (hours, collected_bps, cost_bps, sign, venue, symbol, peak)
    total = 0

    def close(key, st):
        if st["hours"] <= 0:
            return
        done.append((st["hours"], st["collected"], st["cost"], st["sign"],
                     key[0], key[1], st["peak"]))

    for path in files():
        with opener(path) as f:
            for line in f:
                if '"obs"' not in line:
                    continue
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if r.get("type") != "obs":
                    continue
                iv = r.get("interval_hours") or 0
                rate = r.get("funding_rate_pct")
                if not iv or rate is None:
                    continue
                total += 1

                bps_hr = rate * 100.0 / iv
                mag = abs(bps_hr)
                sign = 1 if bps_hr >= 0 else -1
                key = (r.get("venue"), r.get("symbol"))
                ts = r.get("ts_ms") or 0
                st = state.get(key)

                hot = mag >= ENTER_BPS_HR and tradeable(r, need_touch)

                if st is not None:
                    gap_h = (ts - st["last"]) / 3600000.0
                    if ts - st["last"] > MAX_GAP_MS or sign != st["sign"]:
                        # A hole in the record, or the rate flipped side. Either
                        # ends the position; bridging would invent income.
                        close(key, st)
                        del state[key]
                        st = None
                    else:
                        # Accrue at the PREVIOUS observation's rate.
                        st["collected"] += st["rate"] * gap_h
                        st["hours"] += gap_h
                        st["last"] = ts
                        st["rate"] = mag
                        st["peak"] = max(st["peak"], mag)

                if hot and st is None:
                    cost = (r.get("cost_bps") or 0.0) * COST_RATIO_50
                    state[key] = {"start": ts, "last": ts, "rate": mag,
                                  "peak": mag, "collected": 0.0, "hours": 0.0,
                                  "cost": cost, "sign": sign}
                elif not hot and st is not None:
                    close(key, st)
                    del state[key]

    for key, st in state.items():
        close(key, st)

    print("REALIZABLE CASH-AND-CARRY AT $%.0f/LEG" % NOTIONAL)
    print("enter above %.2f bps/hr, exit when it cools, accrue at the observed rate\n"
          % ENTER_BPS_HR)
    print("%d observations, %d completed positions\n" % (total, len(done)))

    for sign, label in ((1, "POSITIVE funding -- long spot / short perp (no borrow needed)"),
                        (-1, "NEGATIVE funding -- needs SHORT SPOT, so borrow, which we have on 4 coins")):
        grp = [d for d in done if d[3] == sign]
        print("=" * 74)
        print(label)
        print("=" * 74)
        if not grp:
            print("  none\n")
            continue

        nets = sorted(d[1] - d[2] for d in grp)
        wins = [n for n in nets if n > 0]
        hrs = sorted(d[0] for d in grp)
        print("  positions: %d, median hold %.1f h, p90 %.1f h, max %.1f h"
              % (len(grp), statistics.median(hrs), pct(hrs, 0.9), hrs[-1]))
        print("  profitable: %d (%.1f%%)" % (len(wins), 100.0 * len(wins) / len(grp)))
        print("  net bps per position: median %+.1f, p90 %+.1f, max %+.1f"
              % (statistics.median(nets), pct(nets, 0.9), nets[-1]))

        tot_net = sum(nets)
        tot_h = sum(hrs)
        print("  TOTAL across all positions: %+.0f bps over %.0f position-hours" % (tot_net, tot_h))
        if tot_h > 0:
            per_hr = tot_net / tot_h
            print("  = %+.4f bps per position-hour" % per_hr)
            print("  = %+.1f%%/yr on notional, %+.1f%%/yr on capital (if always deployed)"
                  % (per_hr * 24 * 365 / 100.0,
                     per_hr * 24 * 365 / 100.0 / CAPITAL_MULTIPLE))

        best = sorted(grp, key=lambda d: -(d[1] - d[2]))[:10]
        print("\n  BEST POSITIONS")
        for h, coll, cost, _, venue, sym, peak in best:
            print("    %-8s %-13s %6.1f h  collected %8.1f  cost %5.1f  NET %+8.1f bps"
                  % (venue, sym, h, coll, cost, coll - cost))
        print()

    print("CAVEATS")
    print("  cost scaled by %.2f from $400 measurements on 7 pairs. For THIN" % COST_RATIO_50)
    print("  books the true ratio is lower (CATSTOCK measured 0.43), so this")
    print("  overstates cost on exactly the coins that now qualify -- the error")
    print("  runs against the result, not for it.")
    print("  No slippage on the funding itself, no failed fills, no partial")
    print("  fills, and entry assumed at the first hot observation.")


if __name__ == "__main__":
    main()
