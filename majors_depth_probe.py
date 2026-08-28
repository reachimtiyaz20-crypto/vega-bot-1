#!/usr/bin/env python3
"""How much notional can the majors basis trade actually hold, across venues?

THE CONSTRAINT THIS MEASURES

Capacity is denominated in NOTIONAL, not capital. Leverage does not create
depth -- it means less of your capital occupies the same space in the book. So:

    20 crore  ~= $2.4M capital
    at 5x     ->  $12M of notional required
    measured  ->  $6.5M available on binance + bybit
    therefore ->  capped near 2.7x, and about 9%/yr instead of 17%

If depth across seven venues is 3-5x what two venues hold, $12M fits and 17%
becomes reachable at that size. If majors are concentrated on binance and bybit
in a way alts are not, it does not, and 9% is the ceiling for a book that large.

C1 measured a 4.1x multiplier -- on THIN ALTS. Majors are a different market
with different venue concentration, so that number cannot simply be reused.

TWO DELIBERATE CHANGES FROM C1

1. DEPTH WITHIN A PRICE BAND, not just top-of-book. A 30-day hold is built
   patiently over hours; the touch is what you can take in one second and
   badly understates what you could accumulate. Bands at 5, 10 and 25 bps.

2. SPOT ASK, because cash-and-carry BUYS spot and SELLS perp. C1 measured the
   bid because reverse carry sells spot. Getting this backwards would measure
   the wrong side of the book.

PERP IS CHECKED, NOT ASSUMED

The journal suggests perp books run far deeper than spot, which is why spot is
treated as the binding leg. That is verified here on binance and bybit, whose
depth endpoints return quantities in BASE UNITS. OKX, Gate and MEXC quote perps
in CONTRACTS with per-venue contract sizes, and a bad conversion would produce
a confidently wrong number -- so their perp side is left unmeasured rather than
guessed. Spot is unambiguous everywhere.

A FAILED FETCH IS UNKNOWN, NEVER ZERO.

Run from ~/vega-bot:  python3 majors_depth_probe.py
"""

import json
import sys
import time
import urllib.error
import urllib.request

COINS = ["BTC", "ETH", "SOL", "BNB", "XRP", "DOGE"]
BANDS_BPS = [5, 10, 25]
TIMEOUT = 10
PACE = 0.15


def http(url):
    req = urllib.request.Request(url, headers={"User-Agent": "vega/1.0"})
    with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
        return json.loads(r.read().decode())


def depth_usd(levels, side, bands):
    """Sum resting USD within each band of the best price.

    levels: [[price, size], ...] sorted best-first.
    side:   'ask' (prices rise) or 'bid' (prices fall).
    """
    if not levels:
        return None
    try:
        best = float(levels[0][0])
    except (ValueError, IndexError):
        return None
    out = {}
    for b in bands:
        lim = best * (1 + b / 10000.0) if side == "ask" else best * (1 - b / 10000.0)
        tot = 0.0
        for row in levels:
            try:
                p, s = float(row[0]), float(row[1])
            except (ValueError, IndexError):
                continue
            if (side == "ask" and p > lim) or (side == "bid" and p < lim):
                break
            tot += p * s
        out[b] = tot
    return out


# ---- spot adapters: return ASK levels, best first ----

def sp_binance(c):
    d = http("https://api.binance.com/api/v3/depth?symbol=%sUSDT&limit=500" % c)
    return d.get("asks")


def sp_bybit(c):
    d = http("https://api.bybit.com/v5/market/orderbook"
             "?category=spot&symbol=%sUSDT&limit=200" % c)
    return ((d.get("result") or {}).get("a")) or None


def sp_okx(c):
    d = http("https://www.okx.com/api/v5/market/books?instId=%s-USDT&sz=400" % c)
    return (d.get("data") or [{}])[0].get("asks")


def sp_gate(c):
    d = http("https://api.gateio.ws/api/v4/spot/order_book"
             "?currency_pair=%s_USDT&limit=100" % c)
    return d.get("asks")


def sp_mexc(c):
    d = http("https://api.mexc.com/api/v3/depth?symbol=%sUSDT&limit=500" % c)
    return d.get("asks")


def sp_bitget(c):
    d = http("https://api.bitget.com/api/v2/spot/market/orderbook"
             "?symbol=%sUSDT&limit=150" % c)
    return ((d.get("data") or {}).get("asks")) or None


def sp_kucoin(c):
    d = http("https://api.kucoin.com/api/v1/market/orderbook/level2_100"
             "?symbol=%s-USDT" % c)
    return ((d.get("data") or {}).get("asks")) or None


SPOT = [("binance", sp_binance), ("bybit", sp_bybit), ("okx", sp_okx),
        ("gate", sp_gate), ("mexc", sp_mexc), ("bitget", sp_bitget),
        ("kucoin", sp_kucoin)]


# ---- perp adapters, base units only ----

def pp_binance(c):
    d = http("https://fapi.binance.com/fapi/v1/depth?symbol=%sUSDT&limit=500" % c)
    return d.get("bids")


def pp_bybit(c):
    d = http("https://api.bybit.com/v5/market/orderbook"
             "?category=linear&symbol=%sUSDT&limit=200" % c)
    return ((d.get("result") or {}).get("b")) or None


PERP = [("binance", pp_binance), ("bybit", pp_bybit)]


def safe(fn, c):
    try:
        return fn(c)
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError,
            ValueError, KeyError, IndexError, TypeError):
        return None


def main():
    print("spot ASK depth within price bands, %d coins x %d venues\n"
          % (len(COINS), len(SPOT)))

    totals = {b: 0.0 for b in BANDS_BPS}
    base_totals = {b: 0.0 for b in BANDS_BPS}   # binance + bybit only
    unknown = 0

    for c in COINS:
        print("%s" % c)
        for vname, fn in SPOT:
            lv = safe(fn, c)
            time.sleep(PACE)
            d = depth_usd(lv, "ask", BANDS_BPS) if lv else None
            if not d:
                print("  %-9s %s" % (vname, "unknown"))
                unknown += 1
                continue
            print("  %-9s " % vname
                  + "  ".join("%dbps $%-11.0f" % (b, d[b]) for b in BANDS_BPS))
            for b in BANDS_BPS:
                totals[b] += d[b]
                if vname in ("binance", "bybit"):
                    base_totals[b] += d[b]
        print()

    print("=" * 62)
    print("%-28s %14s %14s" % ("", "binance+bybit", "all 7 venues"))
    for b in BANDS_BPS:
        mult = totals[b] / base_totals[b] if base_totals[b] else 0
        print("%-28s %13.0f %13.0f   %.1fx"
              % ("spot depth within %d bps" % b, base_totals[b], totals[b], mult))

    print("\nperp BID depth, binance + bybit only (base-unit quantities):")
    perp_tot = {b: 0.0 for b in BANDS_BPS}
    for c in COINS:
        for vname, fn in PERP:
            lv = safe(fn, c)
            time.sleep(PACE)
            d = depth_usd(lv, "bid", BANDS_BPS) if lv else None
            if d:
                for b in BANDS_BPS:
                    perp_tot[b] += d[b]
    for b in BANDS_BPS:
        ratio = perp_tot[b] / base_totals[b] if base_totals[b] else 0
        print("  within %2d bps: $%-12.0f  (%.1fx the spot side)"
              % (b, perp_tot[b], ratio))

    print("\n%d venue-coin lookups returned nothing. Unknown, not zero." % unknown)
    print("\nWHAT THIS DECIDES")
    print("  $2.4M of capital at 5x needs $12M of notional.")
    for b in BANDS_BPS:
        ok = "FITS" if totals[b] >= 12e6 else "does not fit"
        print("    within %2d bps of mid, seven venues hold $%.1fM -- %s"
              % (b, totals[b] / 1e6, ok))
    print("  If it fits, 17%/yr is reachable at 20 crore. If not, leverage is")
    print("  capped by depth and the honest figure stays near 9%.")
    print("\n  One snapshot, one moment. Depth varies with the hour and the")
    print("  regime, and this is a calm Tuesday. Sample it repeatedly before")
    print("  betting on it.")


if __name__ == "__main__":
    main()
