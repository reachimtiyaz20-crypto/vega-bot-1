#!/usr/bin/env python3
"""Does capacity multiply across venues, or is a crowded short thin everywhere?

THE ONE QUESTION THAT COULD MOVE THE CAPACITY NUMBER BY AN ORDER OF MAGNITUDE.

Measured 2026-08-25 on binance and bybit: the entire profitable opportunity set
holds about $171. At ~23% annualised on deployed capital that is a real edge
and a small business, and adding capital does not scale it.

Order books are independent. HOLO's $100 of depth on binance says nothing about
what is resting on OKX or Gate. If five venues each hold comparable depth, real
capacity is five times what we measured and the picture changes materially.

Or it does not. The coins running negative funding are crowded shorts, and a
crowded short may be thin EVERYWHERE for precisely the reason its funding is
negative. That would make the ceiling structural rather than an artifact of
having integrated only two venues.

This probe answers it WITHOUT INTEGRATING ANYTHING: public order-book endpoints,
no keys, no code in the trading path.

SPOT ONLY, AND DELIBERATELY

Spot is the binding leg -- the journal fires 'thinspot' far more often than
'thinperp', and SAND measured $92 spot against $99 perp. Spot quantities are
also unambiguously in base units, whereas Gate, MEXC and KuCoin quote perps in
CONTRACTS with per-venue contract sizes. One bad contract-size conversion would
produce a confidently wrong capacity figure, which is worse than a narrow one.
So this is an upper bound on the spot leg, and the perp side needs separate
verification before anyone believes a total.

A FAILED FETCH IS UNKNOWN, NEVER ZERO. A venue that times out is not a venue
with no depth, and recording it as zero would understate capacity in a way that
looks like evidence.

BORROW IS NOT MEASURED HERE. Reverse carry needs somebody on that venue willing
to lend the coin. Depth without borrow is not tradeable capacity, and every
number below should be read as a ceiling on what integration could unlock.

Run from ~/vega-bot:  python3 venue_depth_probe.py
"""

import glob
import json
import sys
import time
import urllib.error
import urllib.request

TIMEOUT = 8
PACE = 0.15          # polite gap between requests
WINDOW_H = 1.0
UA = "vega-depth-probe/1.0"


def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def usd(px, sz):
    try:
        return float(px) * float(sz)
    except (TypeError, ValueError):
        return None


# Each adapter returns the USD resting on the BID -- we SELL spot to short it,
# so the bid is the side we would hit. None means unknown, not zero.

def okx(c):
    d = get("https://www.okx.com/api/v5/market/books?instId=%s-USDT&sz=1" % c)
    b = (d.get("data") or [{}])[0].get("bids") or []
    return usd(b[0][0], b[0][1]) if b else None


def gate(c):
    d = get("https://api.gateio.ws/api/v4/spot/order_book"
            "?currency_pair=%s_USDT&limit=1" % c)
    b = d.get("bids") or []
    return usd(b[0][0], b[0][1]) if b else None


def kucoin(c):
    d = get("https://api.kucoin.com/api/v1/market/orderbook/level1?symbol=%s-USDT" % c)
    x = d.get("data") or {}
    return usd(x.get("bestBid"), x.get("bestBidSize")) if x.get("bestBid") else None


def bitget(c):
    d = get("https://api.bitget.com/api/v2/spot/market/orderbook"
            "?symbol=%sUSDT&limit=1" % c)
    b = ((d.get("data") or {}).get("bids")) or []
    return usd(b[0][0], b[0][1]) if b else None


def mexc(c):
    d = get("https://api.mexc.com/api/v3/depth?symbol=%sUSDT&limit=1" % c)
    b = d.get("bids") or []
    return usd(b[0][0], b[0][1]) if b else None


VENUES = [("okx", okx), ("gate", gate), ("kucoin", kucoin),
          ("bitget", bitget), ("mexc", mexc)]


def base(s):
    for q in ("USDT", "USDC", "USD"):
        if s.endswith(q) and len(s) > len(q):
            return s[: -len(q)]
    return s


def current_negatives():
    """Coins with negative funding right now, and our measured spot depth."""
    cut = (time.time() - WINDOW_H * 3600) * 1000
    newest = {}
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
            if k not in newest or (r.get("ts_ms") or 0) > (newest[k].get("ts_ms") or 0):
                newest[k] = r

    out = {}
    for (v, s), r in newest.items():
        iv, rate = r.get("interval_hours") or 0, r.get("funding_rate_pct")
        if not iv or rate is None:
            continue
        b = rate * 100.0 / iv
        if b >= 0:
            continue
        c = base(s or "")
        top = r.get("spot_top_usd") or 0.0
        cur = out.get(c)
        # Keep the deepest of our two venues as the baseline.
        if cur is None or top > cur["known"]:
            out[c] = {"bps_hr": b, "known": top, "venue": v}
    return out


def main():
    negs = current_negatives()
    if not negs:
        sys.exit("no negative-funding coins in the last hour -- try again later")

    coins = sorted(negs, key=lambda c: negs[c]["bps_hr"])[:20]
    print("probing %d coins across %d venues, spot bid depth only\n"
          % (len(coins), len(VENUES)))

    hdr = "%-9s %8s %11s" % ("coin", "fund/hr", "bin/bybit")
    for name, _ in VENUES:
        hdr += " %9s" % name
    print(hdr + " %11s" % "cross-venue")

    tot_known = 0.0
    tot_all = 0.0
    unknowns = 0
    for c in coins:
        row = "%-9s %8.3f %11.0f" % (c, negs[c]["bps_hr"], negs[c]["known"])
        found = negs[c]["known"]
        for name, fn in VENUES:
            try:
                d = fn(c)
            except (urllib.error.URLError, urllib.error.HTTPError,
                    TimeoutError, ValueError, KeyError, IndexError):
                d = None
            time.sleep(PACE)
            if d is None:
                row += " %9s" % "-"
                unknowns += 1
            else:
                row += " %9.0f" % d
                found += d
        tot_known += negs[c]["known"]
        tot_all += found
        print(row + " %11.0f" % found)

    print("\nbinance+bybit spot depth on these coins:   $%.0f" % tot_known)
    print("with five more venues added:               $%.0f" % tot_all)
    if tot_known > 0:
        print("multiplier:                                %.1fx" % (tot_all / tot_known))
    print("\n%d venue-coin lookups returned nothing. Unknown, NOT zero: a coin"
          % unknowns)
    print("not listed and a venue that timed out look identical from here.")
    print("\nCEILING, NOT CAPACITY. Spot bid only; the perp leg is unverified,")
    print("and reverse carry additionally needs each venue to LEND the coin.")
    print("Treat this as the most integration could possibly buy.")


if __name__ == "__main__":
    main()
