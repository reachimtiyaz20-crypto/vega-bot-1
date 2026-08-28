#!/usr/bin/env python3
"""How much of the opportunity are we blind to, running only two venues?

THE GAP THIS SIZES

Our capacity figure -- median $114, measured every 15 minutes across 172
samples -- was computed on BINANCE AND BYBIT ONLY. I then quoted it as the
capacity of the strategy. That was the same single-source error as quoting
Bybit's 4.45% borrow as the cost of dollars, and it took a challenge to catch
both times.

Coinglass shows reverse-carry opportunities on Bitget, OKX, Gate, KuCoin,
BingX, WhiteBIT, Bitunix, LBank and Aster -- some with millions in open
interest. We see none of them.

Before building four exchange integrations, size the prize: how many
(venue, coin) pairs currently show harvestable negative funding on venues we
do not have? If the answer is "a handful on illiquid venues", the integration
is not worth it. If it is "the opportunity set triples", it is the highest
value work available.

WHAT IT MEASURES

For a candidate coin list, current funding across six venues, and how many
opportunities sit on venues we cannot currently see.

WHAT IT DOES NOT MEASURE, and both matter

	borrow      reverse carry needs to short spot, which needs a lender. A
	            venue showing negative funding with no borrow market is
	            another COTI: rich, and untradeable. Only Binance and Bybit
	            borrow data exists in this project today.
	depth       an opportunity on $4k of open interest is a screenshot, not
	            a trade. Coinglass showed exactly that -- CYBER at 4390% APR
	            on $4,010 of OI.

So read the output as a CEILING on what integration could unlock, and treat
any venue that clears it as needing a borrow and depth check before a single
line of integration code gets written.

Public endpoints only. No keys.

Run:  python3 multivenue_probe.py
"""

import collections
import json
import statistics
import sys
import time
import urllib.error
import urllib.request

# Coins currently showing reverse-carry interest, from our own journal and
# from the Coinglass funding-arbitrage list.
COINS = ["BICO", "ONT", "SAND", "HOME", "RVN", "MINA", "AGI", "ACE", "ZIL",
         "PUFFER", "RUNE", "TAC", "JTO", "AERO", "ENA", "HOLO", "KAITO",
         "COTI", "STORJ", "BMT", "MOVE", "RED", "XAI", "WAL", "KMNO",
         "ID", "COOKIE", "TREE", "HFT", "ONG", "PROM", "VANRY", "B3"]

HAVE = {"binance", "bybit"}      # what VEGA can actually trade today

# Negative funding beyond this is worth looking at. 0.05%/8h is roughly
# 0.6 bps/hr, comfortably above the neutral baseline of 0.125.
THRESHOLD = -0.05


def http(url, timeout=12):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-mvp/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


# Each returns funding rate as a PERCENT per settlement interval, or None.

def f_binance(c):
    d = http("https://fapi.binance.com/fapi/v1/premiumIndex?symbol=%sUSDT" % c)
    r = d.get("lastFundingRate")
    return float(r) * 100 if r not in (None, "") else None


def f_bybit(c):
    d = http("https://api.bybit.com/v5/market/tickers"
             "?category=linear&symbol=%sUSDT" % c)
    lst = (d.get("result") or {}).get("list") or []
    if not lst:
        return None
    r = lst[0].get("fundingRate")
    return float(r) * 100 if r not in (None, "") else None


def f_okx(c):
    d = http("https://www.okx.com/api/v5/public/funding-rate"
             "?instId=%s-USDT-SWAP" % c)
    lst = d.get("data") or []
    if not lst:
        return None
    r = lst[0].get("fundingRate")
    return float(r) * 100 if r not in (None, "") else None


def f_bitget(c):
    d = http("https://api.bitget.com/api/v2/mix/market/current-fund-rate"
             "?symbol=%sUSDT&productType=usdt-futures" % c)
    lst = d.get("data") or []
    if not lst:
        return None
    r = lst[0].get("fundingRate")
    return float(r) * 100 if r not in (None, "") else None


def f_gate(c):
    d = http("https://api.gateio.ws/api/v4/futures/usdt/contracts/%s_USDT" % c)
    r = d.get("funding_rate") if isinstance(d, dict) else None
    return float(r) * 100 if r not in (None, "") else None


def f_kucoin(c):
    d = http("https://api-futures.kucoin.com/api/v1/funding-rate/%sUSDTM/current" % c)
    r = (d.get("data") or {}).get("value")
    return float(r) * 100 if r is not None else None


VENUES = [("binance", f_binance), ("bybit", f_bybit), ("okx", f_okx),
          ("bitget", f_bitget), ("gate", f_gate), ("kucoin", f_kucoin)]


def main():
    print("probing %d coins across %d venues, current funding only\n"
          % (len(COINS), len(VENUES)))
    print("VEGA can trade today: %s" % ", ".join(sorted(HAVE)))
    print("threshold for interest: funding at or below %.3f%% per interval\n"
          % THRESHOLD)

    table = {}
    listed = collections.Counter()
    for c in COINS:
        row = {}
        for vname, fn in VENUES:
            try:
                r = fn(c)
            except (urllib.error.URLError, urllib.error.HTTPError,
                    TimeoutError, ValueError, KeyError, IndexError, TypeError):
                r = None
            time.sleep(0.08)
            row[vname] = r
            if r is not None:
                listed[vname] += 1
        table[c] = row

    names = [v for v, _ in VENUES]
    print("%-10s" % "coin" + "".join("%10s" % n for n in names))
    for c in COINS:
        row = table[c]
        line = "%-10s" % c
        for n in names:
            v = row[n]
            line += "%10s" % ("-" if v is None else "%.4f" % v)
        print(line)

    print("\nLISTINGS PER VENUE (of %d coins)" % len(COINS))
    for n in names:
        mark = "  <- we have this" if n in HAVE else ""
        print("  %-10s %2d%s" % (n, listed[n], mark))

    # Opportunities: negative funding past the threshold.
    ours = 0
    theirs = 0
    theirs_detail = []
    for c in COINS:
        for n in names:
            v = table[c][n]
            if v is None or v > THRESHOLD:
                continue
            if n in HAVE:
                ours += 1
            else:
                theirs += 1
                theirs_detail.append((v, c, n))

    print("\nOPPORTUNITIES AT OR BELOW %.3f%%" % THRESHOLD)
    print("  on venues we HAVE:        %d" % ours)
    print("  on venues we DO NOT have: %d" % theirs)
    if ours:
        print("  integration would multiply the visible set by %.1fx"
              % ((ours + theirs) / ours))

    if theirs_detail:
        print("\nWHAT WE ARE BLIND TO, richest first")
        print("%-10s %-10s %10s %12s" % ("coin", "venue", "funding", "ann. %/yr"))
        for v, c, n in sorted(theirs_detail)[:25]:
            # Assume 8h settlement for the annualisation; venues differ and
            # this is a sizing exercise, not a trade decision.
            print("%-10s %-10s %9.4f%% %11.0f%%" % (c, n, v, v * 3 * 365))

    print("\nBEFORE ANY INTEGRATION WORK")
    print("  1. Does that venue have a BORROW market for the coin? Reverse")
    print("     carry shorts spot, and a venue with no lender is another COTI.")
    print("  2. Is there depth? Coinglass showed CYBER at 4390%% APR on $4,010")
    print("     of open interest. That is a screenshot, not a position.")
    print("  3. Can the account trade there at all? The Bybit 10024 restriction")
    print("     is a reminder that jurisdiction gates some venues entirely.")
    print("\n  A high count above is a reason to CHECK those three, not a reason")
    print("  to start building.")


if __name__ == "__main__":
    main()
