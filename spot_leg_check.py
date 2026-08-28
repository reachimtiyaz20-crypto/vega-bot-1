#!/usr/bin/env python3
"""Can the hedge actually be built? Where does spot exist for these coins?

WHY THIS DECIDES EVERYTHING

Measured over 45 days of Hyperliquid hourly funding, holding a short from any
rich entry and charging ~40 bps a round trip:

	CASHCAT   7-day hold   169 bps net   ~44%/yr on capital
	APEX      7-day hold   127 bps net   ~33%/yr
	XMR       7-day hold    31 bps net    ~8%/yr
	VVV       7-day hold    29 bps net    ~7.5%/yr

CASHCAT was positive in EVERY hour of 45 days -- a structural long bias with no
arbitrage capital present. 44% on capital clears the target for the first time
in this project.

But a short perp is only delta-neutral if you hold the spot. Without a spot
leg it is a naked short on a memecoin, which is not a strategy, it is a
directional bet with extra steps.

This project has been stopped by exactly this five times already:

	COTI     -109 bps/hr funding, no venue would lend it
	STORJ     -24 bps/hr, $5.10 on the perp touch, absent from borrow
	new listings   0 of 35 had a spot pair
	AAVE on GMX   16,766% APR on zero open interest
	AXS advertised at -1980%, actually +11%

Every time the number was spectacular and the trade could not be constructed.
So before any more arithmetic: does the spot exist, and where.

WHAT IS CHECKED

	Hyperliquid spot   best case -- same venue, one account, no transfers
	Binance, Bybit, OKX, Gate, KuCoin, MEXC   a CEX leg means capital on two
	                   venues, no margin netting, and transfer time between
	                   them when either side needs topping up

A coin with spot ONLY on an on-chain DEX is a different proposition again:
gas, slippage, and a wallet to manage.

Public endpoints, no keys.  Run:  python3 spot_leg_check.py
"""

import json
import sys
import time
import urllib.error
import urllib.request

COINS = ["CASHCAT", "APEX", "XMR", "VVV", "CHIP", "IMX", "FARTCOIN",
         "JTO", "MON", "LIT", "MINA", "ACE"]

# Net on capital at a 7-day hold, from the persistence run, after 40 bps.
NET_ON_CAPITAL = {"CASHCAT": 44.0, "APEX": 33.0, "XMR": 8.0, "VVV": 7.5}


def get(url, timeout=25):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-spot/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def post(url, payload, timeout=25):
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json",
                 "User-Agent": "vega-spot/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def hyperliquid_spot():
    try:
        d = post("https://api.hyperliquid.xyz/info", {"type": "spotMeta"})
    except Exception as e:
        print("  hyperliquid spot lookup failed: %s" % e)
        return set()
    out = set()
    for t in (d.get("tokens") or []):
        n = t.get("name")
        if n:
            out.add(n.upper())
    return out


def binance():
    d = get("https://api.binance.com/api/v3/exchangeInfo")
    return {s["baseAsset"].upper() for s in d.get("symbols", [])
            if s.get("status") == "TRADING" and s.get("quoteAsset") in ("USDT", "USDC")}


def bybit():
    d = get("https://api.bybit.com/v5/market/instruments-info?category=spot&limit=1000")
    out = set()
    for s in ((d.get("result") or {}).get("list") or []):
        if s.get("quoteCoin") in ("USDT", "USDC") and s.get("baseCoin"):
            out.add(s["baseCoin"].upper())
    return out


def okx():
    d = get("https://www.okx.com/api/v5/public/instruments?instType=SPOT")
    return {i["baseCcy"].upper() for i in (d.get("data") or [])
            if i.get("quoteCcy") in ("USDT", "USDC") and i.get("baseCcy")}


def gate():
    d = get("https://api.gateio.ws/api/v4/spot/currency_pairs")
    return {p["base"].upper() for p in d
            if p.get("quote") in ("USDT", "USDC") and p.get("base")}


def kucoin():
    d = get("https://api.kucoin.com/api/v1/symbols")
    return {s["baseCurrency"].upper() for s in (d.get("data") or [])
            if s.get("quoteCurrency") in ("USDT", "USDC") and s.get("baseCurrency")}


def mexc():
    d = get("https://api.mexc.com/api/v3/exchangeInfo")
    return {s["baseAsset"].upper() for s in d.get("symbols", [])
            if s.get("quoteAsset") in ("USDT", "USDC") and s.get("baseAsset")}


VENUES = [("hyperliquid", hyperliquid_spot), ("binance", binance),
          ("bybit", bybit), ("okx", okx), ("gate", gate),
          ("kucoin", kucoin), ("mexc", mexc)]


def main():
    print("checking where spot exists for the Hyperliquid funding candidates\n")
    sets = {}
    for name, fn in VENUES:
        try:
            s = fn()
            sets[name] = s
            print("  %-12s %5d spot pairs" % (name, len(s)))
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError,
                ValueError, KeyError, TypeError) as e:
            print("  %-12s lookup failed: %s" % (name, e))
            sets[name] = set()
        time.sleep(0.2)

    names = [n for n, _ in VENUES]
    print("\n%-10s %10s" % ("coin", "net/yr"), end="")
    for n in names:
        print("%13s" % n[:12], end="")
    print()

    for c in COINS:
        net = NET_ON_CAPITAL.get(c)
        print("%-10s %10s" % (c, ("%.0f%%" % net) if net else "-"), end="")
        for n in names:
            print("%13s" % ("yes" if c in sets.get(n, set()) else "-"), end="")
        print()

    print("\nVERDICT")
    for c in COINS:
        net = NET_ON_CAPITAL.get(c)
        if net is None:
            continue
        where = [n for n in names if c in sets.get(n, set())]
        if not where:
            verdict = "NO SPOT ANYWHERE we checked -- cannot be hedged"
        elif "hyperliquid" in where:
            verdict = "spot on Hyperliquid itself -- one venue, no transfers"
        else:
            verdict = "CEX leg required: %s" % ", ".join(where)
        print("  %-10s %5.0f%%/yr on capital   %s" % (c, net, verdict))

    print("\nWHAT EACH VERDICT COSTS")
    print("  same venue      one account, margin may net, no transfer delay")
    print("  CEX leg         capital sits on TWO venues and cannot net. Each")
    print("                  side needs its own buffer, and topping one up")
    print("                  means a transfer while the position is live.")
    print("  no spot found   the rate is unreachable. This is the sixth time")
    print("                  a spectacular number has failed exactly here, and")
    print("                  it is the reason to check before modelling.")
    print("\n  A coin absent from every list above may still trade on an")
    print("  on-chain DEX -- which is gas, slippage and a wallet to run, and")
    print("  a different business from what VEGA currently does.")


if __name__ == "__main__":
    main()
