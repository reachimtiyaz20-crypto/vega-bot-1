#!/usr/bin/env python3
"""How much smaller must we trade before the edge is reachable -- and then how
much money can the strategy actually hold?

THREE WALLS, IN ORDER, EACH MORE FUNDAMENTAL THAN THE LAST

  borrow availability   fixed: 25 -> 132 lendable coins
  borrow cost           fixed: the cheap tier borrows at 0.005-0.04 bps/hr
  BOOK DEPTH AT $50     this one is not a proxy and cannot be argued with

Measured 2026-08-25 11:30: of 53 symbols with negative funding, 18 are
borrowable, and 3 clear their round trip over a 48h hold. All 3 are refused for
'shallow' or 'slippage' -- direct measurements of whether the trade is
executable, not gates anybody guessed at.

THE ONLY HONEST LEVER LEFT IS SIZE.

Depth gates are relative to notional. At $20 a book that is too thin for $50 is
genuinely deep enough; the gate passes because the trade got easier, not
because the line moved. Returns are in bps, so percentage return is unchanged.

WHY COST DOES NOT IMPROVE WHEN SIZE FALLS

cost_bps = fees + 2 x (spot half-spread + perp half-spread). Crossing the
spread costs the same fraction at any size. What changes is whether that
number is TRUE: once notional exceeds top-of-book you walk the book, and the
journal says so explicitly -- "measured slippage is a FLOOR, not the cost".
Below the touch, cost_bps is the whole cost. So shrinking size does not make
the trade cheaper; it makes the measurement honest.

CAPACITY IS THE NUMBER THAT DECIDES THIS PROJECT

A strategy netting 40 bps on a $20 position is not a strategy that can hold
20 crore. For every symbol that passes, the largest honest position is
min(spot top-of-book, perp top-of-book). Summed, that is the instantaneous
capacity of the whole opportunity set -- and it is the figure to put in front
of an investor before anything else.

Run from ~/vega-bot:  python3 notional_sweep.py
"""

import glob
import json
import time

HOLD_H = 48.0
MIN_VOL = 250_000
WINDOW_H = 1.5
FEES_ROUNDTRIP = 30.0   # only used to sanity-check the decomposition


def base(s):
    for q in ("USDT", "USDC", "USD"):
        if s.endswith(q) and len(s) > len(q):
            return s[: -len(q)]
    return s


def borrow_map():
    last = [l for l in open("data/borrow/rates.jsonl") if l.strip()][-1]
    best = {}
    for x in json.loads(last)["rates"]:
        if x.get("ok") and x.get("borrowable"):
            c, a = x["currency"], x["annual_pct"]
            if c not in best or a < best[c]:
                best[c] = a
    return {c: a * 100 / 8760 for c, a in best.items()}


def newest_obs():
    cut = (time.time() - WINDOW_H * 3600) * 1000
    out = {}
    for p in sorted(glob.glob("data/reverse/journal/*.jsonl")):
        for line in open(p, errors="replace"):
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
    lend = borrow_map()
    obs = newest_obs()

    # Candidates: negative funding, borrowable, liquid enough by volume.
    cands = []
    for (v, s), r in obs.items():
        iv, rate = r.get("interval_hours") or 0, r.get("funding_rate_pct")
        if not iv or rate is None:
            continue
        b = rate * 100.0 / iv
        if b >= 0:
            continue
        c = base(s or "")
        if c not in lend:
            continue
        if (r.get("spot_vol_24h_usd") or 0) < MIN_VOL:
            continue
        if (r.get("perp_vol_24h_usd") or 0) < MIN_VOL:
            continue
        carry = -b - lend[c]
        cost = r.get("cost_bps") or 45.0
        top = min(r.get("spot_top_usd") or 0, r.get("perp_top_usd") or 0)
        cands.append({
            "v": v, "s": s, "carry": carry, "cost": cost, "top": top,
            "net48": carry * HOLD_H - cost, "fund": b, "borrow": lend[c],
        })

    print("borrowable negative-funding candidates clearing volume: %d\n" % len(cands))

    print("%8s %6s %9s %11s %14s %13s" %
          ("notional", "pass", "capital ea", "positions/$1600", "capacity now", "median net@48h"))
    for N in (50, 20, 10, 5, 2):
        # HONEST DEPTH: the whole position must sit inside top-of-book on both
        # legs. Anything larger walks the book and the cost figure becomes a
        # floor rather than a cost.
        ok = [c for c in cands if c["top"] >= N and c["net48"] > 0]
        cap = sum(min(c["top"], 1e9) for c in ok)
        nets = sorted(c["net48"] for c in ok)
        med = nets[len(nets) // 2] if nets else 0.0
        print("%8s %6d %9s %11d %14s %13.1f" %
              ("$%d" % N, len(ok), "$%d" % (2 * N), 1600 // (2 * N),
               "$%.0f" % cap, med))

    print("\ncandidates that clear economics, by how large a position they could hold:")
    winners = sorted((c for c in cands if c["net48"] > 0),
                     key=lambda c: -c["top"])
    if not winners:
        print("  none clear their round trip at any size")
    for c in winners[:20]:
        print("  %-20s fund %7.3f  borrow %6.3f  carry %6.3f  cost %5.1f  "
              "net@48h %+7.1f  max honest size $%.0f"
              % (c["v"] + "/" + c["s"], c["fund"], c["borrow"], c["carry"],
                 c["cost"], c["net48"], c["top"]))

    tot = sum(c["top"] for c in cands if c["net48"] > 0)
    print("\nINSTANTANEOUS CAPACITY of everything currently profitable: $%.0f" % tot)
    print("At 1.5%%/month the brother's fund needs ~$24k/month on 20 crore.")
    print("This snapshot is one moment; run it repeatedly before believing it.")


if __name__ == "__main__":
    main()
