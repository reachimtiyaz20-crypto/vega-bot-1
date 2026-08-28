#!/usr/bin/env python3
"""Is there a funding premium on newly listed perps -- and can it be harvested?

PATH 3 OF FIVE

Of the paths worth measuring, this is the one still untouched. The others
resolved:

	points farming        measured: base 8%, points a bet, needs $5k+ to clear friction
	alt reverse carry     capacity $74 on two venues, x4.1 across seven = ~$300
	maker rebates         spread is 0.72 bps and the game is decided by latency
	vault fees            a business-model question, not a measurement

New listings are different because the premium is real and well known: when a
perp lists, funding frequently runs at extreme levels for days while the market
finds its level. Everyone wants the new thing, longs crowd in, and shorts get
paid handsomely to take the other side.

The catch is always the same one this project keeps rediscovering: the biggest
funding sits where the trade cannot be constructed. So this measures both
halves.

WHAT IT MEASURES

	1. Funding by days since listing, across recent listings. Is there a
	   premium, how large, and how fast does it decay?
	2. Whether the hedge existed. A short perp against no spot pair is a
	   naked short, not a carry trade. Binance spot listings are checked
	   directly.
	3. Whether the premium survives the round trip. Days 1-7 of a listing
	   have wide spreads and thin books; a 40 bps cost against a 3-day
	   premium is a different proposition from the same premium held a month.

Binance publishes onboardDate for every futures symbol, so the day-since-listing
axis is exact rather than inferred.

WHAT IT CANNOT TELL YOU

Whether you could have got size on. Early listing books are thin and the
premium attracts exactly the people best equipped to take it. Treat any
positive result as an upper bound and the depth column as the real constraint.

Run from ~/vega-bot:  python3 new_listings.py
"""

import collections
import json
import statistics
import sys
import time
import urllib.error
import urllib.request

FAPI = "https://fapi.binance.com"
SAPI = "https://api.binance.com"
MONTHS_BACK = 9
MAX_SYMBOLS = 40
BUCKETS = [(0, 1), (1, 3), (3, 7), (7, 14), (14, 30), (30, 90), (90, 365)]


def http(url):
    req = urllib.request.Request(url, headers={"User-Agent": "vega/1.0"})
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.loads(r.read().decode())


def recent_listings():
    d = http(FAPI + "/fapi/v1/exchangeInfo")
    cut = (time.time() - MONTHS_BACK * 30 * 86400) * 1000
    out = []
    for s in d.get("symbols", []):
        if s.get("status") != "TRADING" or s.get("quoteAsset") != "USDT":
            continue
        ob = s.get("onboardDate")
        if not ob or ob < cut:
            continue
        out.append((int(ob), s["symbol"], s.get("baseAsset")))
    out.sort(reverse=True)
    return out[:MAX_SYMBOLS]


def spot_symbols():
    """Which bases have a Binance spot pair -- the hedge leg must exist."""
    try:
        d = http(SAPI + "/api/v3/exchangeInfo")
    except Exception:
        return set()
    return {s["baseAsset"] for s in d.get("symbols", [])
            if s.get("status") == "TRADING" and s.get("quoteAsset") == "USDT"}


def funding(symbol, start_ms):
    out, cur = [], start_ms
    for _ in range(20):
        try:
            rows = http("%s/fapi/v1/fundingRate?symbol=%s&startTime=%d&limit=1000"
                        % (FAPI, symbol, cur))
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError):
            break
        if not rows:
            break
        out += [(int(r["fundingTime"]), float(r["fundingRate"])) for r in rows]
        last = int(rows[-1]["fundingTime"])
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.2)
        if len(rows) < 1000:
            break
    return out


def main():
    print("finding perps listed in the last %d months" % MONTHS_BACK)
    lst = recent_listings()
    if not lst:
        sys.exit("no recent listings found")
    print("%d symbols\n" % len(lst))

    spot = spot_symbols()
    print("%d bases have a Binance spot pair\n" % len(spot))

    # bucket -> list of bps/hr
    by_age = collections.defaultdict(list)
    hedgeable = collections.defaultdict(list)
    per_symbol = []

    for ob, sym, base in lst:
        rows = funding(sym, ob)
        if len(rows) < 6:
            continue
        has_spot = base in spot
        vals = []
        for t, rate in rows:
            age_d = (t - ob) / 86400000.0
            bps_hr = rate * 10000.0 / 8.0
            for lo, hi in BUCKETS:
                if lo <= age_d < hi:
                    by_age[(lo, hi)].append(bps_hr)
                    if has_spot:
                        hedgeable[(lo, hi)].append(bps_hr)
                    break
            if age_d < 7:
                vals.append(bps_hr)
        if vals:
            per_symbol.append((statistics.mean(vals), sym, has_spot,
                               time.strftime("%Y-%m-%d", time.gmtime(ob / 1000))))
        time.sleep(0.1)

    if not by_age:
        sys.exit("no funding history retrieved")

    print("FUNDING BY AGE SINCE LISTING")
    print("%-14s %8s %12s %12s %12s" %
          ("days old", "n", "mean bps/hr", "median", "annualised"))
    for lo, hi in BUCKETS:
        v = by_age.get((lo, hi))
        if not v:
            continue
        m = statistics.mean(v)
        print("%-14s %8d %12.4f %12.4f %11.1f%%" %
              ("%g-%g" % (lo, hi), len(v), m, statistics.median(v),
               m * 8760.0 / 100.0))

    print("\nSAME, BUT ONLY WHERE A SPOT PAIR EXISTS (i.e. actually hedgeable)")
    print("%-14s %8s %12s %12s %12s" %
          ("days old", "n", "mean bps/hr", "median", "annualised"))
    for lo, hi in BUCKETS:
        v = hedgeable.get((lo, hi))
        if not v:
            continue
        m = statistics.mean(v)
        print("%-14s %8d %12.4f %12.4f %11.1f%%" %
              ("%g-%g" % (lo, hi), len(v), m, statistics.median(v),
               m * 8760.0 / 100.0))

    n_spot = sum(1 for _, _, hs, _ in per_symbol if hs)
    print("\n%d of %d recent listings have a spot pair to hedge against (%.0f%%)"
          % (n_spot, len(per_symbol), 100.0 * n_spot / max(len(per_symbol), 1)))

    print("\nPER LISTING, first 7 days, richest first")
    print("%-16s %12s %8s  %s" % ("symbol", "mean bps/hr", "spot?", "listed"))
    for m, sym, hs, when in sorted(per_symbol, reverse=True)[:20]:
        print("%-16s %12.4f %8s  %s" % (sym, m, "yes" if hs else "NO", when))

    print("\nWHAT DECIDES THIS")
    first = by_age.get((0, 1)) or by_age.get((1, 3)) or []
    later = by_age.get((30, 90)) or []
    if first and later:
        f, l = statistics.mean(first), statistics.mean(later)
        print("  premium in the first days vs a month later: %.4f vs %.4f bps/hr"
              % (f, l))
        if l:
            print("  that is %.1fx" % (f / l if l else 0))
        # A 40 bps round trip is realistic on a fresh listing: wide spreads,
        # thin books, and both legs to cross.
        for hold_d in (3, 7, 14):
            gross = f * 24 * hold_d
            print("  held %2dd at the day-0 rate: %6.1f bps gross, %+6.1f after "
                  "a 40 bps round trip" % (hold_d, gross, gross - 40))
    print("\n  The hedgeable table is the one that counts. A premium on a perp")
    print("  with no spot pair is a naked short, not a carry trade -- and this")
    print("  project has already found twice that the richest funding sits")
    print("  exactly where the trade cannot be built.")


if __name__ == "__main__":
    main()
