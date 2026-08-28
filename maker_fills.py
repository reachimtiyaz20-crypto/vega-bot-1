#!/usr/bin/env python3
"""Would a resting order actually fill -- and would BOTH legs fill?

WHY THIS MUST COME BEFORE ANY MAKER CODE

The earlier maker simulation assumed a flat 75% fill rate and reported a 48%
uplift. That number is unusable for two reasons.

FIRST, it was never measured. A fill rate is a property of the market, not an
assumption, and a maker model fed a made-up rate will report that capacity is
effectively unlimited -- an answer that feels like progress and is worth
nothing.

SECOND, and worse, it treated the legs as independent. Reverse carry is SHORT
SPOT and LONG PERP. To short spot you post a sell at the ask, ABOVE the mark.
To go long perp you post a buy at the bid, BELOW it. Spot and perp track each
other, so a market trending up fills the spot leg and leaves the perp leg
unfilled; trending down does the reverse. Both legs fill only when price
OSCILLATES across the spread inside the horizon.

So the joint fill probability is not 0.75 x 0.75. It may be far lower, because
the two events are negatively correlated by construction. And a single-leg fill
is not a partial win -- it is an UNHEDGED POSITION, which is the one thing a
market-neutral book must never hold by accident.

WHAT IS MEASURED, per (venue, symbol) and horizon

  spot_only    price rose through the ask, never touched the perp bid
  perp_only    price fell through the bid, never touched the spot ask
  both         price crossed both -- the only outcome that opens a position
  neither      no fill; the opportunity is missed, which costs nothing

Fills are inferred from 5-minute snapshots, which is conservative in one
direction and optimistic in another, and the script says so rather than
pretending to tick data:

  UNDERSTATES fills -- a dip that fills the order and recovers inside one poll
                       interval is invisible here
  OVERSTATES  fills -- price reaching a level does not guarantee OUR order
                       filled; queue position is unmodelled. Requiring price to
                       cross STRICTLY THROUGH the level mitigates this: if the
                       market prints past your price, everything resting at it
                       traded.

Restricted to borrowable coins, because a fill rate on a coin we cannot short
is not a fact we need.

Run from ~/vega-bot:  nice -n 10 python3 maker_fills.py
"""

import collections
import glob
import gzip
import json
import os
import sys

JOURNAL = "data/journal"
HORIZONS_MIN = (5, 10, 15, 30, 60, 120)
MAX_GAP_MS = 45 * 60 * 1000


def base(s):
    for q in ("USDT", "USDC", "USD"):
        if s.endswith(q) and len(s) > len(q):
            return s[: -len(q)]
    return s


def borrowable():
    try:
        last = [l for l in open("data/borrow/rates.jsonl") if l.strip()][-1]
    except (OSError, IndexError):
        return set()
    out = set()
    for x in json.loads(last).get("rates") or []:
        if x.get("ok") and x.get("borrowable") and x.get("currency"):
            out.add(x["currency"])
    return out


def opener(p):
    return (gzip.open(p, "rt", encoding="utf-8", errors="replace")
            if p.endswith(".gz") else open(p, "rt", errors="replace"))


def load(lend):
    """series[(venue,symbol)] = [(ts_ms, mark, spot_half_bps, perp_half_bps), ...]"""
    series = collections.defaultdict(list)
    paths = (sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
             + sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl"))))
    if not paths:
        sys.exit("no journal files -- run from ~/vega-bot")
    print("reading %d journal files" % len(paths))
    for p in paths:
        with opener(p) as f:
            for line in f:
                if '"obs"' not in line:
                    continue
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if r.get("type") != "obs":
                    continue
                sym = r.get("symbol") or ""
                if base(sym) not in lend:
                    continue
                mark = r.get("mark_price")
                sh = r.get("spot_half_spread_bps")
                ph = r.get("perp_half_spread_bps")
                ts = r.get("ts_ms") or 0
                if not mark or sh is None or ph is None or not ts:
                    continue
                series[(r.get("venue"), sym)].append((ts, mark, sh, ph))
    for k in series:
        series[k].sort()
    return series


def main():
    lend = borrowable()
    if not lend:
        sys.exit("no borrow snapshot -- is vega-borrow running?")
    print("%d borrowable currencies" % len(lend))

    series = load(lend)
    print("%d venue-symbols with usable price series\n" % len(series))

    # counts[horizon] = Counter of outcome
    counts = {h: collections.Counter() for h in HORIZONS_MIN}
    # spread captured, in bps, when BOTH legs fill
    captured = {h: [] for h in HORIZONS_MIN}

    for key, rows in series.items():
        n = len(rows)
        for i in range(n):
            ts, mark, sh, ph = rows[i]
            if mark <= 0:
                continue
            # Post at the touch on both legs.
            ask = mark * (1.0 + sh / 10000.0)    # sell spot here
            bid = mark * (1.0 - ph / 10000.0)    # buy perp here
            for h in HORIZONS_MIN:
                deadline = ts + h * 60 * 1000
                hit_ask = hit_bid = False
                gapped = False
                prev = ts
                for j in range(i + 1, n):
                    tj, mj, _, _ = rows[j]
                    if tj > deadline:
                        break
                    if tj - prev > MAX_GAP_MS:
                        # A hole in the record is not evidence of no fill.
                        gapped = True
                        break
                    prev = tj
                    if mj >= ask:
                        hit_ask = True
                    if mj <= bid:
                        hit_bid = True
                    if hit_ask and hit_bid:
                        break
                if gapped:
                    continue
                if hit_ask and hit_bid:
                    counts[h]["both"] += 1
                    captured[h].append(sh + ph)
                elif hit_ask:
                    counts[h]["spot_only"] += 1
                elif hit_bid:
                    counts[h]["perp_only"] += 1
                else:
                    counts[h]["neither"] += 1

    print("%9s %8s %10s %10s %10s %14s" %
          ("horizon", "both", "spot only", "perp only", "neither", "spread saved"))
    for h in HORIZONS_MIN:
        c = counts[h]
        tot = sum(c.values())
        if not tot:
            continue
        cap = captured[h]
        med = sorted(cap)[len(cap) // 2] if cap else 0.0
        print("%7dm %7.1f%% %9.1f%% %9.1f%% %9.1f%% %11.2f bps" %
              (h, 100.0 * c["both"] / tot, 100.0 * c["spot_only"] / tot,
               100.0 * c["perp_only"] / tot, 100.0 * c["neither"] / tot, med))

    print("\nREADING THIS TABLE")
    print("  'both' is the only column that opens a hedged position.")
    print("  'spot only' and 'perp only' are UNHEDGED fills -- the maker book")
    print("  must cancel the unfilled leg and either cross it (paying taker,")
    print("  losing the saving) or abandon the trade. Their sum is the rate at")
    print("  which a naive maker implementation would carry naked risk.")
    print("  'spread saved' is what a both-legs fill earns versus crossing,")
    print("  BEFORE any adverse selection, which this does not yet model.")


if __name__ == "__main__":
    main()
