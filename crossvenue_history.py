#!/usr/bin/env python3
"""Two years of cross-venue perp-perp: long the venue paying you, short the other.

WHY THIS STRUCTURE MIGHT BEAT EVERYTHING MEASURED SO FAR

Spot-perp carry is bounded by two things we measured the hard way: spot fees
(10 bps a side, versus 5 for perps) and spot book depth, which capped total
capacity at $171. Cross-venue perp-perp has NEITHER. No spot leg means no spot
fee, no borrow, and no lender who can refuse. And perp books are far deeper
than spot -- the journal shows perp top-of-book routinely 10-20x the spot side
on the same coin.

What it gives up is size of edge. Spot-perp captures the FULL funding rate.
Cross-venue captures only the DIFFERENCE between two venues' rates, which is
smaller by construction. Whether the cheaper, deeper, unborrowed structure
beats the richer but capped one is an empirical question, and this answers it
over two years of settled data.

THREE BOOKS, same as the majors analysis so the numbers are comparable

  STATIC     hold one venue pair, one direction, always
  PERFECT    always on the right side of the spread, free instantaneous flips
  REALISTIC  flips only after the spread's sign has persisted, and pays a
             round trip on BOTH perp legs each time

Cost is 25 bps round trip: four taker fills (entry and exit on two venues) at
~5-5.5 bps each, plus spread. That is CHEAPER than the 33 bps measured for
spot-perp, and the difference is the spot leg we are not trading.

FUNDING INTERVALS ARE DERIVED, NOT ASSUMED. Binance, Bybit and OKX mostly
settle 8-hourly but not always, and a rate quoted per interval means nothing
without the interval. The script infers it from consecutive timestamps.

NOT MODELLED: basis divergence between venues (the two perps can decouple),
venue failure -- which is now doubled because you are exposed to two -- and
liquidation. A cross-venue book also cannot net margin across exchanges, so
leverage here is more dangerous than on a single venue with portfolio margin.

Run from ~/vega-bot:  python3 crossvenue_history.py
"""

import collections
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request

CACHE = "data/history/crossvenue_2y.json"
YEARS = 2
SYMBOLS = ["BTC", "ETH", "SOL", "XRP", "DOGE"]
ROUNDTRIP_BPS = 25.0
PERSIST = 3
TARGET = 18.0
LEVERAGE = [("unlevered", 0.5), ("PM 1.11x", 0.9), ("2x", 2.0),
            ("3x", 3.0), ("5x", 5.0), ("8x", 8.0)]


def http(url):
    req = urllib.request.Request(url, headers={"User-Agent": "vega/1.0"})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.loads(r.read().decode())


def binance(coin, start_ms, end_ms):
    out, cur = [], start_ms
    while cur < end_ms:
        try:
            rows = http("https://fapi.binance.com/fapi/v1/fundingRate"
                        "?symbol=%sUSDT&startTime=%d&limit=1000" % (coin, cur))
        except Exception:
            break
        if not rows:
            break
        out += [(int(r["fundingTime"]), float(r["fundingRate"])) for r in rows]
        last = int(rows[-1]["fundingTime"])
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.25)
        if len(rows) < 1000:
            break
    return out


def bybit(coin, start_ms, end_ms):
    """Bybit pages backwards and caps at 200 rows."""
    out, end = [], end_ms
    for _ in range(40):
        try:
            d = http("https://api.bybit.com/v5/market/funding/history"
                     "?category=linear&symbol=%sUSDT&startTime=%d&endTime=%d&limit=200"
                     % (coin, start_ms, end))
        except Exception:
            break
        rows = ((d.get("result") or {}).get("list")) or []
        if not rows:
            break
        for r in rows:
            out.append((int(r["fundingRateTimestamp"]), float(r["fundingRate"])))
        oldest = min(int(r["fundingRateTimestamp"]) for r in rows)
        if oldest <= start_ms:
            break
        end = oldest - 1
        time.sleep(0.25)
    return out


def okx(coin, start_ms, end_ms):
    """OKX pages backwards via `after`, caps at 100 rows."""
    out, after = [], end_ms
    for _ in range(80):
        try:
            d = http("https://www.okx.com/api/v5/public/funding-rate-history"
                     "?instId=%s-USDT-SWAP&after=%d&limit=100" % (coin, after))
        except Exception:
            break
        rows = d.get("data") or []
        if not rows:
            break
        for r in rows:
            out.append((int(r["fundingTime"]), float(r["fundingRate"])))
        oldest = min(int(r["fundingTime"]) for r in rows)
        if oldest <= start_ms:
            break
        after = oldest
        time.sleep(0.25)
    return out


VENUES = [("binance", binance), ("bybit", bybit), ("okx", okx)]


def to_bps_hr(rows):
    """(ts, rate_per_interval) -> {ts: bps_per_hour}, interval derived."""
    rows = sorted(set(rows))
    if len(rows) < 3:
        return {}
    deltas = [rows[i + 1][0] - rows[i][0] for i in range(len(rows) - 1)]
    iv_ms = statistics.median(deltas)
    iv_h = max(iv_ms / 3600000.0, 0.5)
    return {t: r * 10000.0 / iv_h for t, r in rows}


def bucket(ts):
    """Align to the hour so venues settling seconds apart still match."""
    return int(ts // 3600000) * 3600000


def load():
    if os.path.exists(CACHE):
        age_h = (time.time() - os.path.getmtime(CACHE)) / 3600
        if age_h < 24:
            print("using cached cross-venue history (%.1fh old)" % age_h)
            return json.load(open(CACHE))
    now = int(time.time() * 1000)
    start = now - YEARS * 365 * 86400 * 1000
    data = {}
    print("fetching %d years, %d coins x %d venues" % (YEARS, len(SYMBOLS), len(VENUES)))
    for c in SYMBOLS:
        data[c] = {}
        for vname, fn in VENUES:
            rows = fn(c, start, now)
            data[c][vname] = rows
            print("  %-6s %-8s %5d settlements" % (c, vname, len(rows)))
    os.makedirs(os.path.dirname(CACHE), exist_ok=True)
    json.dump(data, open(CACHE, "w"))
    return data


def main():
    data = load()

    # month -> book -> accrued bps on notional
    per_month = collections.defaultdict(
        lambda: collections.defaultdict(float))
    per_month_n = collections.Counter()
    flips = 0
    pairs_used = []

    for coin, venues in data.items():
        series = {v: to_bps_hr(rows) for v, rows in venues.items() if rows}
        names = [v for v in series if len(series[v]) > 100]
        if len(names) < 2:
            print("  %s: fewer than two usable venues, skipped" % coin)
            continue

        # Pick the venue pair with the widest median |spread|. Choosing the
        # best pair in hindsight flatters the result, so it is stated: this is
        # an upper bound on pair selection.
        best_pair, best_med, best_common = None, -1.0, None
        for i in range(len(names)):
            for j in range(i + 1, len(names)):
                a, b = names[i], names[j]
                ba = {bucket(t): v for t, v in series[a].items()}
                bb = {bucket(t): v for t, v in series[b].items()}
                common = sorted(set(ba) & set(bb))
                if len(common) < 200:
                    continue
                sp = [abs(ba[t] - bb[t]) for t in common]
                m = statistics.median(sp)
                if m > best_med:
                    best_pair, best_med = (a, b), m
                    best_common = (common, ba, bb)
        if not best_pair:
            print("  %s: no overlapping pair with enough history" % coin)
            continue

        a, b = best_pair
        common, ba, bb = best_common
        pairs_used.append("%s %s/%s med|spread| %.4f bps/hr (%d pts)"
                          % (coin, a, b, best_med, len(common)))

        # Hours between settlements, for accrual.
        deltas = [common[i + 1] - common[i] for i in range(len(common) - 1)]
        iv_h = statistics.median(deltas) / 3600000.0 if deltas else 8.0

        side, run = 1, 0
        for t in common:
            spread = ba[t] - bb[t]          # bps/hr, positive => short a, long b
            per_int = spread * iv_h         # bps accrued this settlement
            ym = time.strftime("%Y-%m", time.gmtime(t / 1000))
            per_month_n[ym] += 1

            per_month[ym]["static"] += per_int
            per_month[ym]["perfect"] += abs(per_int)
            per_month[ym]["real"] += per_int * side

            want = 1 if spread >= 0 else -1
            if want != side:
                run += 1
                if run >= PERSIST:
                    side = want
                    run = 0
                    per_month[ym]["real"] -= ROUNDTRIP_BPS
                    flips += 1
            else:
                run = 0

    if not per_month:
        sys.exit("no usable cross-venue history")

    print("\nPAIRS SELECTED (widest median spread, chosen in hindsight):")
    for p in pairs_used:
        print("  " + p)

    keys = sorted(k for k in per_month_n if per_month_n[k] >= 30)
    n_coins = max(len(pairs_used), 1)
    print("\n%d months, %s to %s, %d coin-pairs\n" % (len(keys), keys[0], keys[-1], n_coins))

    def ann(bps_total, lev):
        return (bps_total / n_coins) * 12.0 * lev / 100.0

    print("MEDIAN ANNUALISED RETURN ON CAPITAL")
    print("%-14s %10s %10s %10s" % ("leverage", "static", "realistic", "perfect"))
    store = {}
    for lname, lev in LEVERAGE:
        v = {b: sorted(ann(per_month[k][b], lev) for k in keys)
             for b in ("static", "real", "perfect")}
        store[lname] = v
        print("%-14s %9.1f%% %9.1f%% %9.1f%%" %
              (lname, statistics.median(v["static"]),
               statistics.median(v["real"]), statistics.median(v["perfect"])))

    print("\nMONTHS OF %d CLEARING %.0f%%/yr" % (len(keys), TARGET))
    print("%-14s %10s %10s %10s" % ("leverage", "static", "realistic", "perfect"))
    for lname, _ in LEVERAGE:
        v = store[lname]
        print("%-14s %10d %10d %10d" %
              (lname, sum(1 for x in v["static"] if x >= TARGET),
               sum(1 for x in v["real"] if x >= TARGET),
               sum(1 for x in v["perfect"] if x >= TARGET)))

    print("\nWORST MONTH at 5x")
    v = store["5x"]
    for b, label in (("static", "static"), ("real", "realistic"),
                     ("perfect", "perfect")):
        print("  %-12s %8.1f%%/yr" % (label, min(v[b])))

    print("\n%d side changes over %d months (%.1f per month per pair)"
          % (flips, len(keys), flips / max(len(keys), 1) / n_coins))
    print("Round trip %.0f bps -- cheaper than spot-perp's 33, because there is"
          % ROUNDTRIP_BPS)
    print("no spot leg. No borrow either: nobody can refuse to lend a perp.")
    print("\nAgainst spot-perp majors (static 5x): 14.5%/yr, worst month -21.8%.")
    print("Cross-venue wins only if it beats that AND holds more size.")


if __name__ == "__main__":
    main()
