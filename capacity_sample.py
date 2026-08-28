#!/usr/bin/env python3
"""Sample how much money the opportunity set can actually hold, repeatedly.

ONE READING IS AN ANECDOTE.

At 2026-08-25 11:30 the whole profitable opportunity set held $171. That single
number reframes the project -- 23%/yr on $171 is not a business -- but it was
taken at one instant on one calm morning, and capacity varies with volatility
in exactly the way returns do. Negative funding episodes cluster; so, probably,
does the depth available to harvest them.

Deciding anything on one sample is the error this project was built to avoid.
So: sample every 15 minutes, append to a journal, and in a week we will have a
distribution instead of an anecdote.

WHAT IS BEING MEASURED

For every symbol with negative funding that is borrowable and clears volume,
the largest HONEST position is min(spot top-of-book, perp top-of-book). Beyond
that you walk the book and the cost figure stops being a cost. Summed across
every symbol that also clears its round trip over the planned hold, that is the
instantaneous capacity of the strategy.

It is a FLOOR, not the capacity. Depth replenishes, and a position worked
patiently over a 48-hour hold can exceed the touch many times over. We have not
measured replenishment, so this number understates by an unknown multiple. It
is still the right number to track, because it is the one we can state without
guessing.

WRITES ONLY to data/capacity/log.jsonl. Touches nothing the books read.

Run:  python3 capacity_sample.py          (quiet, for the timer)
      python3 capacity_sample.py -v       (human summary)
"""

import glob
import json
import os
import sys
import time

HOLD_H = 48.0
MIN_VOL = 250_000
WINDOW_H = 0.5          # only observations from the last poll or two
TIERS = (100, 50, 20, 10, 5, 2)
OUT = "data/capacity/log.jsonl"


def base(s):
    for q in ("USDT", "USDC", "USD"):
        if s.endswith(q) and len(s) > len(q):
            return s[: -len(q)]
    return s


def borrow_map():
    """Cheapest lender per currency, from the newest snapshot only.

    A rate from last week is not evidence about this hour, and borrow rises
    precisely when shorting demand rises -- which is precisely when a reverse
    position wants to open. A stale rate is optimistic in the wrong direction.
    """
    try:
        last = [l for l in open("data/borrow/rates.jsonl") if l.strip()][-1]
    except (OSError, IndexError):
        return {}
    best = {}
    for x in json.loads(last).get("rates") or []:
        if x.get("ok") and x.get("borrowable"):
            c, a = x.get("currency"), x.get("annual_pct")
            if c and a is not None and (c not in best or a < best[c]):
                best[c] = a
    return {c: a * 100 / 8760 for c, a in best.items()}


def newest_obs():
    cut = (time.time() - WINDOW_H * 3600) * 1000
    out = {}
    for p in sorted(glob.glob("data/reverse/journal/*.jsonl")):
        try:
            f = open(p, errors="replace")
        except OSError:
            continue
        with f:
            for line in f:
                if '"obs"' not in line:
                    continue
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if r.get("type") != "obs" or (r.get("ts_ms") or 0) < cut:
                    continue
                k = (r.get("venue"), r.get("symbol"))
                if k not in out or (r.get("ts_ms") or 0) > (out[k].get("ts_ms") or 0):
                    out[k] = r
    return out


def main():
    verbose = "-v" in sys.argv
    if not os.path.isdir("data"):
        sys.exit("run from ~/vega-bot")

    lend = borrow_map()
    obs = newest_obs()
    if not obs:
        # No fresh observations is a fact worth recording, not a crash. A gap
        # in the series must be visible as a gap.
        rec = {"ts_ms": int(time.time() * 1000), "stale": True}
        os.makedirs(os.path.dirname(OUT), exist_ok=True)
        with open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")
        if verbose:
            print("no observations in the last %.1fh -- recorded as stale" % WINDOW_H)
        return 0

    negatives = 0
    cands = []
    for (v, s), r in obs.items():
        iv, rate = r.get("interval_hours") or 0, r.get("funding_rate_pct")
        if not iv or rate is None:
            continue
        b = rate * 100.0 / iv
        if b >= 0:
            continue
        negatives += 1
        c = base(s or "")
        if c not in lend:
            continue
        if (r.get("spot_vol_24h_usd") or 0) < MIN_VOL:
            continue
        if (r.get("perp_vol_24h_usd") or 0) < MIN_VOL:
            continue
        carry = -b - lend[c]
        cost = r.get("cost_bps") or 45.0
        top = min(r.get("spot_top_usd") or 0.0, r.get("perp_top_usd") or 0.0)
        net48 = carry * HOLD_H - cost
        if net48 <= 0:
            continue
        cands.append({
            "venue": v, "symbol": s, "fund_bps_hr": round(b, 4),
            "borrow_bps_hr": round(lend[c], 4), "carry_bps_hr": round(carry, 4),
            "cost_bps": round(cost, 2), "net48_bps": round(net48, 2),
            "max_size_usd": round(top, 2),
        })

    tiers = {}
    for n in TIERS:
        fit = [c for c in cands if c["max_size_usd"] >= n]
        tiers[str(n)] = {
            "pass": len(fit),
            "capacity_usd": round(sum(c["max_size_usd"] for c in fit), 2),
            "deployable_usd": round(sum(n for _ in fit), 2),
        }

    rec = {
        "ts_ms": int(time.time() * 1000),
        "negatives": negatives,
        "profitable": len(cands),
        "capacity_usd": round(sum(c["max_size_usd"] for c in cands), 2),
        "tiers": tiers,
        "top": sorted(cands, key=lambda c: -c["max_size_usd"])[:10],
    }

    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "a") as f:
        f.write(json.dumps(rec) + "\n")

    if verbose:
        print("%d negative-funding symbols, %d clear economics, capacity $%.0f"
              % (negatives, len(cands), rec["capacity_usd"]))
        for n in TIERS:
            t = tiers[str(n)]
            print("  at $%-4d notional: %2d fit, $%.0f deployable"
                  % (n, t["pass"], t["deployable_usd"]))
        for c in rec["top"][:8]:
            print("  %-20s carry %6.3f/hr  net@48h %+7.1f  max $%.0f"
                  % (c["venue"] + "/" + c["symbol"], c["carry_bps_hr"],
                     c["net48_bps"], c["max_size_usd"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
