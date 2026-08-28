#!/usr/bin/env python3
"""How long does favourable funding last, and what should we hold for?

WHY THE PARAMETER MATTERS

The reverse book holds 2 days. That was picked early and never tested, and the
round trip is a FIXED cost paid on every turnover -- so the hold length decides
how much of the gross funding survives.

Two practitioners disagree about whether long holds are possible:

	"high funding (>0.1% per 8h) usually lasts 1-7 days before normalizing"
	"I have 3 positions open, the oldest one +70 days old"

Our own book has seen both: SAND and ONT ran the full hold and closed at +77
and +106 bps; HOLO and JTO flipped against us inside a day.

THE FIRST VERSION OF THIS WAS WRONG, AND THE OUTPUT SAID SO

It fixed entry at -0.5 bps/hr and swept only the hold, producing 83%
persistence alongside a 10% win rate. Those cannot both be true. At -0.5
bps/hr a two-day hold earns 24 bps gross against a 47.7 bps round trip, so
nearly every position lost on arithmetic before persistence mattered. It
measured "enter on anything mildly negative", which is not the strategy --
live entries were ONT -2.4, SAND -3.97, HOME -2.18 bps/hr.

Threshold and hold interact: a longer hold has more hours to clear the same
fixed cost, so a looser threshold becomes viable. Neither can be swept alone,
hence a grid.

WHAT IS MEASURED

	1. PERSISTENCE -- given funding is favourable now, what share of the next
	   N hours is it still favourable. The fundamental quantity.
	2. A GRID of annualised return on capital across entry threshold and hold
	   length, borrow charged by the clock, one round trip per position,
	   positions non-overlapping so one long favourable stretch is not counted
	   as many independent wins.

Annualised, so a 2-day hold recycling capital fifteen times is compared fairly
against a 14-day hold that does not.

LIMIT: 20 days of journal. Holds past ~14 days are untestable, and a 70-day
position is entirely outside what we can see.

Run from ~/vega-bot:  nice -n 10 python3 hold_length.py
"""

import collections
import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"
NOTIONAL = 50.0
MIN_TOP_FRAC = 0.25
MIN_VOL = 250_000.0

# Exits measured 12% above entry on real closes, so the round trip is not
# symmetric and 45 bps was optimistic.
ROUND_TRIP_BPS = 45.0 * 1.06

HOLDS_D = [1, 2, 3, 5, 7, 10, 14]
THRESHOLDS = [-0.5, -1.0, -2.0, -4.0, -8.0]
PERSIST_CHECKS = [1, 3, 6, 12, 24, 48, 96, 168, 336]
MAX_GAP_MS = 45 * 60 * 1000


def opener(p):
    return (gzip.open(p, "rt", encoding="utf-8", errors="replace")
            if p.endswith(".gz") else open(p, "rt", errors="replace"))


def base(s):
    for q in ("USDT", "USDC", "USD"):
        if s.endswith(q) and len(s) > len(q):
            return s[: -len(q)]
    return s


def borrow_map():
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
    return {c: a * 100.0 / 8760.0 for c, a in best.items()}


def load(lend):
    need = NOTIONAL * MIN_TOP_FRAC
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
                iv, rate = r.get("interval_hours") or 0, r.get("funding_rate_pct")
                ts = r.get("ts_ms") or 0
                if not iv or rate is None or not ts:
                    continue
                sym = r.get("symbol") or ""
                if base(sym) not in lend:
                    continue
                ok = (r.get("spot_available") and r.get("liq_measured")
                      and (r.get("spot_top_usd") or 0) >= need
                      and (r.get("perp_top_usd") or 0) >= need
                      and (r.get("spot_vol_24h_usd") or 0) >= MIN_VOL
                      and (r.get("perp_vol_24h_usd") or 0) >= MIN_VOL)
                series[(r.get("venue"), sym)].append(
                    (ts, rate * 100.0 / iv, bool(ok)))
    for k in series:
        series[k].sort()
    return series


def simulate(series, lend, enter_bps, hd):
    """Non-overlapping positions at one threshold and one hold length."""
    hold_ms = hd * 86400000
    out = []
    for key, rows in series.items():
        b_hr = lend.get(base(key[1]), 0.0)
        n = len(rows)
        i = 0
        while i < n:
            ts, b, ok = rows[i]
            if b > enter_bps or not ok:
                i += 1
                continue
            deadline = ts + hold_ms
            collected = 0.0
            prev_ts, prev_b = ts, b
            j = i + 1
            gapped = False
            while j < n and rows[j][0] <= deadline:
                tj, bj, _ = rows[j]
                if tj - prev_ts > MAX_GAP_MS:
                    gapped = True
                    break
                hrs = (tj - prev_ts) / 3600000.0
                # Accrue at the rate observed BEFORE the interval; charge
                # borrow for the same clock time.
                collected += (-prev_b - b_hr) * hrs
                prev_ts, prev_b = tj, bj
                j += 1
            held_h = (prev_ts - ts) / 3600000.0
            if not gapped and held_h >= hd * 24 * 0.6:
                out.append(collected - ROUND_TRIP_BPS)
            i = j if j > i else i + 1
    return out


def persistence(series):
    still, seen = collections.Counter(), collections.Counter()
    for key, rows in series.items():
        n = len(rows)
        for i in range(n):
            ts, b, ok = rows[i]
            if b > -0.5 or not ok:
                continue
            for h in PERSIST_CHECKS:
                deadline = ts + h * 3600000
                fav = tot = 0
                gapped = False
                prev = ts
                for j in range(i + 1, n):
                    tj, bj, _ = rows[j]
                    if tj > deadline:
                        break
                    if tj - prev > MAX_GAP_MS:
                        gapped = True
                        break
                    prev = tj
                    tot += 1
                    if bj < 0:
                        fav += 1
                if gapped or tot < 2:
                    continue
                seen[h] += 1
                still[h] += fav / float(tot)
    return still, seen


def main():
    lend = borrow_map()
    if not lend:
        sys.exit("no borrow snapshot -- is vega-borrow running?")
    print("%d borrowable currencies" % len(lend))
    series = load(lend)
    print("%d venue-symbols on borrowable coins\n" % len(series))

    still, seen = persistence(series)
    print("PERSISTENCE -- entering below -0.5 bps/hr, share of the next N hours")
    print("still favourable (funding negative)")
    print("%10s %10s %16s" % ("hours", "samples", "still favourable"))
    for h in PERSIST_CHECKS:
        if seen[h] >= 20:
            print("%10d %10d %15.0f%%" % (h, seen[h], 100.0 * still[h] / seen[h]))

    print("\n\nANNUALISED RETURN ON CAPITAL -- entry threshold x hold length")
    print("borrow by the clock, one round trip of %.1f bps, non-overlapping"
          % ROUND_TRIP_BPS)
    header = "%-14s" % "entry bps/hr"
    for hd in HOLDS_D:
        header += "%10s" % ("%dd" % hd)
    print(header)

    grid = {}
    for th in THRESHOLDS:
        row = "%-14s" % ("%.1f" % th)
        for hd in HOLDS_D:
            nets = simulate(series, lend, th, hd)
            if len(nets) < 10:
                row += "%10s" % "-"
                continue
            mean = statistics.mean(nets)
            ann = mean / 10000.0 * (365.0 / hd) / 2.0 * 100.0
            win = 100.0 * sum(1 for x in nets if x > 0) / len(nets)
            grid[(th, hd)] = (ann, len(nets), win, mean)
            row += "%9.0f%%" % ann
        print(row)

    if not grid:
        sys.exit("\nno cell had enough positions")

    print("\nTOP CELLS")
    print("%-24s %10s %8s %12s %12s"
          % ("entry / hold", "positions", "win %", "mean bps", "ann on cap"))
    for (th, hd), (ann, n, win, mean) in sorted(
            grid.items(), key=lambda kv: -kv[1][0])[:8]:
        print("%-24s %10d %7.0f%% %12.1f %11.0f%%"
              % ("%.1f bps/hr, %dd" % (th, hd), n, win, mean, ann))

    cur = grid.get((-2.0, 2))
    if cur:
        print("\nnearest cell to the live setting (-2 bps/hr, 2d): "
              "%.0f%%/yr, %d positions, %.0f%% won" % (cur[0], cur[1], cur[2]))

    print("\nHOW TO READ IT")
    print("  Compare the annualised column, not mean bps: a 2-day hold")
    print("  recycles capital seven times more often than a 14-day hold, so a")
    print("  smaller per-position number can still win.")
    print("  A rich threshold with few positions is not obviously better than")
    print("  a loose one with many -- the position count is the capacity, and")
    print("  we already know capacity is this strategy's binding limit.")
    print("\n  LIMIT: 20 days of journal. Nothing past ~14 days is testable.")


if __name__ == "__main__":
    main()
