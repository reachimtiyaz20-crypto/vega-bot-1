#!/usr/bin/env python3
"""Listed is not liquid. How much spot can actually be bought?

THE CHECK THAT HAS ENDED THIS FIVE TIMES

	COTI     -109 bps/hr funding, no venue would lend it
	STORJ     -24 bps/hr, $5.10 resting on the perp touch
	new listings   0 of 35 had a spot pair at all
	AAVE on GMX   16,766% APR on zero open interest
	AXS advertised -1980%, actually +11%

Each time the rate was spectacular and the position could not be built. The
Hyperliquid candidates cleared the "does spot exist" test -- the first lead
that has -- but existing on an exchange's symbol list says nothing about
whether $10,000 can be bought without moving the price.

WHAT IS BEING PRICED

	short the perp on Hyperliquid, collecting funding
	long the spot on a CEX, bought with cash, no borrowing
	capital sits on BOTH venues and cannot net

	CASHCAT   44%/yr on capital   spot: gate, kucoin, mexc
	APEX      33%/yr              spot: bybit, mexc
	XMR        8%/yr              spot: kucoin, mexc
	VVV        8%/yr              spot: bybit, gate, kucoin, mexc

The spot leg is bought at the ASK, so that is the side measured. Depth is
summed within price bands rather than at the touch alone, because a position
held for a week is accumulated over hours -- the same reasoning that moved the
majors capacity figure from $171 to unconstrained.

WHAT THIS STILL DOES NOT SETTLE

	fees        assumed 40 bps a round trip across all four legs. Unverified.
	access      Gate, KuCoin and MEXC are unknown for this jurisdiction after
	            the Bybit 10024 block. CASHCAT is unreachable if they are.
	perp side   Hyperliquid open interest was $36.4M for CASHCAT but only
	            $3.27M for APEX on $196k of daily volume -- thin enough that
	            a position may move the funding rate against itself.

Public endpoints, no keys.  Run:  python3 hedge_depth.py
"""

import json
import sys
import time
import urllib.error
import urllib.request

# coin -> venues where spot was found
TARGETS = {
    "CASHCAT": ["gate", "kucoin", "mexc"],
    "APEX": ["bybit", "mexc"],
    "XMR": ["kucoin", "mexc"],
    "VVV": ["bybit", "gate", "kucoin", "mexc"],
}
NET_YR = {"CASHCAT": 44.0, "APEX": 33.0, "XMR": 8.0, "VVV": 7.5}
BANDS = [10, 25, 50]        # bps from best ask


def get(url, timeout=15):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-depth/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


# Each returns ASK levels [[price, size], ...], best first.

def bybit(c):
    d = get("https://api.bybit.com/v5/market/orderbook"
            "?category=spot&symbol=%sUSDT&limit=200" % c)
    return ((d.get("result") or {}).get("a")) or None


def gate(c):
    d = get("https://api.gateio.ws/api/v4/spot/order_book"
            "?currency_pair=%s_USDT&limit=100" % c)
    return d.get("asks")


def kucoin(c):
    d = get("https://api.kucoin.com/api/v1/market/orderbook/level2_100"
            "?symbol=%s-USDT" % c)
    return ((d.get("data") or {}).get("asks")) or None


def mexc(c):
    d = get("https://api.mexc.com/api/v3/depth?symbol=%sUSDT&limit=500" % c)
    return d.get("asks")


ADAPTERS = {"bybit": bybit, "gate": gate, "kucoin": kucoin, "mexc": mexc}


def depth_usd(levels, bands):
    if not levels:
        return None
    try:
        best = float(levels[0][0])
    except (ValueError, IndexError, TypeError):
        return None
    out = {}
    for b in bands:
        lim = best * (1 + b / 10000.0)
        tot = 0.0
        for row in levels:
            try:
                p, s = float(row[0]), float(row[1])
            except (ValueError, IndexError, TypeError):
                continue
            if p > lim:
                break
            tot += p * s
        out[b] = tot
    return out


def main():
    print("spot ASK depth for the Hyperliquid funding candidates")
    print("(the spot leg is BOUGHT, so the ask is the side that matters)\n")

    totals = {}
    for coin, venues in TARGETS.items():
        print("%s  --  %.0f%%/yr on capital" % (coin, NET_YR.get(coin, 0)))
        best_by_band = {b: 0.0 for b in BANDS}
        any_ok = False
        for v in venues:
            fn = ADAPTERS.get(v)
            if not fn:
                continue
            try:
                lv = fn(coin)
            except (urllib.error.URLError, urllib.error.HTTPError,
                    TimeoutError, ValueError, KeyError, TypeError):
                lv = None
            time.sleep(0.15)
            d = depth_usd(lv, BANDS) if lv else None
            if not d:
                print("    %-9s unavailable" % v)
                continue
            any_ok = True
            print("    %-9s " % v
                  + "  ".join("%dbps $%-10.0f" % (b, d[b]) for b in BANDS))
            for b in BANDS:
                # Deepest single venue, not the sum: splitting a hedge across
                # three exchanges triples the accounts, the transfers and the
                # counterparty exposure for one position.
                best_by_band[b] = max(best_by_band[b], d[b])
        if any_ok:
            totals[coin] = best_by_band
            print("    %-9s " % "BEST"
                  + "  ".join("%dbps $%-10.0f" % (b, best_by_band[b])
                              for b in BANDS))
        print()

    print("=" * 66)
    print("WHAT SIZE EACH TRADE SUPPORTS, on the deepest single venue")
    print("%-10s %9s %14s %14s %14s"
          % ("coin", "net/yr", "10 bps", "25 bps", "50 bps"))
    for coin in TARGETS:
        t = totals.get(coin)
        if not t:
            print("%-10s %9s %14s" % (coin, "-", "no depth data"))
            continue
        print("%-10s %8.0f%% %13s %13s %13s"
              % (coin, NET_YR.get(coin, 0),
                 "$%.0f" % t[10], "$%.0f" % t[25], "$%.0f" % t[50]))

    print("\nAND WHAT IT EARNS AT THAT SIZE")
    print("capital is 2x the position (spot bought outright + perp margin)")
    print("%-10s %14s %14s %14s"
          % ("coin", "position @25bps", "capital", "annual $"))
    for coin in TARGETS:
        t = totals.get(coin)
        if not t:
            continue
        pos = t[25]
        cap = pos * 2
        earn = cap * NET_YR.get(coin, 0) / 100.0
        print("%-10s %13s %13s %13s"
              % (coin, "$%.0f" % pos, "$%.0f" % cap, "$%.0f" % earn))

    print("\nHOW TO JUDGE IT")
    print("  A 25 bps band is a fair accumulation target for a week-long hold:")
    print("  wider than the touch, tight enough that impact does not eat the")
    print("  carry. If that column is thousands, the trade is a business at")
    print("  small size. If it is hundreds, this joins the $114 alt book --")
    print("  real, measured, and unable to hold money.")
    print("\n  The perp side matters too: CASHCAT had $36.4M of open interest,")
    print("  APEX only $3.27M on $196k daily volume. Depth on one leg does not")
    print("  help if the other is the constraint.")


if __name__ == "__main__":
    main()
