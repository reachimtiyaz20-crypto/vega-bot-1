#!/usr/bin/env python3
"""We measured one venue TYPE and called it the market.

THE GAP THIS CLOSES

Five years of Binance funding, measured carefully, produced a clear conclusion:
carry pays ~4%, borrowing costs 4.45%, leverage is worthless. Every step of
that arithmetic was right. The scope was not.

A practitioner running this on GMX for a year reports 34% annualised, after
fees, on $25,000, with on-chain receipts. The reason is structural, not luck,
and it is two things we never modelled:

1. CAPITAL EFFICIENCY. On a CEX you buy spot AND post margin for the short:
   about $1.25 of capital per $1 of notional, and exceeding that means
   borrowing dollars at 4.45%. On GMX you post WBTC as COLLATERAL and short
   BTC against it -- the collateral IS the long leg. One position, $1.00 of
   capital per $1 of notional, nothing borrowed. My entire "leverage requires
   borrowing, therefore leverage is worthless" conclusion is an artifact of
   the CEX structure and does not apply here.

2. FUNDING LEVEL. DEX perp flow is heavily long-biased retail with far less
   arbitrage capital present than Binance. Shorts get paid more, and more
   persistently, because there are fewer people competing to take that side.

So this measures DEX funding with the same rigour applied to Binance, and puts
them side by side on a capital-adjusted basis.

WHAT THE PRACTITIONERS IN THAT THREAD ALSO SAID, which the numbers must respect

	"ensure open interest is much higher than your position size, otherwise
	 funding rates will turn against you"   -- at size you BECOME the
	 imbalance you were harvesting. That is the capacity limit.

	"funding rates shift right before the payout and swing right back"
	 -- the same decay that turned our HOLO and JTO positions negative.

	"was easier to do years ago"  -- the compression we measured independently.

None of those are modelled here. This measures the RATE, which is necessary and
not sufficient.

Public endpoints. No keys.

Run:  python3 dex_funding.py
"""

import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request

DAYS = 180
COINS = ["BTC", "ETH", "SOL"]
CEX_CACHE = "data/history/funding_5y.json"

# Capital required per $1 of notional, delta-neutral.
#   CEX: buy $1 spot, post margin for the $1 short.
#   DEX: post $1 of the coin as collateral, short $1 against it.
CAP_CEX = 1.25
CAP_DEX = 1.00


def get(url, timeout=25):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-dex/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def post(url, payload, timeout=25):
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json",
                 "User-Agent": "vega-dex/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def hyperliquid(coin, start_ms, end_ms):
    """Hourly funding. Returns bps/hr, positive = SHORT receives."""
    out, cur = [], start_ms
    for _ in range(30):
        if cur >= end_ms:
            break
        try:
            rows = post("https://api.hyperliquid.xyz/info",
                        {"type": "fundingHistory", "coin": coin,
                         "startTime": cur, "endTime": end_ms})
        except Exception:
            break
        if not rows:
            break
        for r in rows:
            try:
                out.append((int(r["time"]), float(r["fundingRate"]) * 10000.0))
            except (KeyError, ValueError, TypeError):
                continue
        last = max(int(r["time"]) for r in rows)
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.2)
        if len(rows) < 500:
            break
    return out


def dydx(coin, start_ms, end_ms):
    """dYdX v4 hourly funding via the public indexer."""
    out = []
    url = ("https://indexer.dydx.trade/v4/historicalFunding/%s-USD?limit=1000"
           % coin)
    try:
        d = get(url)
    except Exception:
        return out
    for r in (d.get("historicalFunding") or []):
        try:
            t = time.strptime(r["effectiveAt"][:19], "%Y-%m-%dT%H:%M:%S")
            ms = int(time.mktime(t) * 1000)
            if ms < start_ms:
                continue
            out.append((ms, float(r["rate"]) * 10000.0))
        except (KeyError, ValueError, TypeError):
            continue
    return out


def binance_cached(coin, start_ms):
    """From the 5y cache we already built. 8h settlements -> bps/hr."""
    if not os.path.exists(CEX_CACHE):
        return []
    try:
        d = json.load(open(CEX_CACHE))
    except Exception:
        return []
    rows = d.get(coin + "USDT") or []
    return [(t, r * 10000.0 / 8.0) for t, r in rows if t >= start_ms]


def stats(rows, label, cap):
    if len(rows) < 30:
        print("  %-14s insufficient data (%d points)" % (label, len(rows)))
        return None
    v = [x for _, x in rows]
    med = statistics.median(v)
    mean = statistics.mean(v)
    pos = 100.0 * sum(1 for x in v if x > 0) / len(v)
    # A SHORT receives positive funding. Annualise on NOTIONAL, then divide by
    # the capital required to hold that notional delta-neutral.
    ann_notional = mean * 8760.0 / 100.0
    ann_capital = ann_notional / cap
    print("  %-14s %5d pts  median %7.4f  mean %7.4f bps/hr  "
          "positive %4.0f%%  %7.1f%%/yr on notional  %7.1f%%/yr on CAPITAL"
          % (label, len(v), med, mean, pos, ann_notional, ann_capital))
    return ann_capital


def main():
    now = int(time.time() * 1000)
    start = now - DAYS * 86400 * 1000
    print("comparing CEX and DEX perp funding, last %d days\n" % DAYS)
    print("capital per $1 notional: CEX %.2f (buy spot + margin), "
          "DEX %.2f (collateral IS the long leg)\n" % (CAP_CEX, CAP_DEX))

    results = {}
    for coin in COINS:
        print("%s" % coin)
        b = binance_cached(coin, start)
        r1 = stats(b, "binance (CEX)", CAP_CEX)

        h = hyperliquid(coin, start, now)
        r2 = stats(h, "hyperliquid", CAP_DEX)

        d = dydx(coin, start, now)
        r3 = stats(d, "dydx v4", CAP_DEX)

        results[coin] = {"binance": r1, "hyperliquid": r2, "dydx": r3}
        print()

    print("=" * 70)
    print("RETURN ON CAPITAL, delta-neutral short, %%/yr" % ())
    print("%-8s %14s %14s %14s" % ("coin", "binance", "hyperliquid", "dydx"))
    for coin in COINS:
        r = results[coin]
        row = "%-8s" % coin
        for k in ("binance", "hyperliquid", "dydx"):
            row += "%13s " % ("--" if r[k] is None else "%.1f%%" % r[k])
        print(row)

    vals = {k: [results[c][k] for c in COINS if results[c][k] is not None]
            for k in ("binance", "hyperliquid", "dydx")}
    print()
    for k, v in vals.items():
        if v:
            print("  %-12s mean across coins: %6.1f%%/yr on capital"
                  % (k, statistics.mean(v)))

    cex = vals.get("binance") or []
    dex = (vals.get("hyperliquid") or []) + (vals.get("dydx") or [])
    if cex and dex:
        mc, md = statistics.mean(cex), statistics.mean(dex)
        print("\n  DEX vs CEX: %.1f%% vs %.1f%%  ->  %.1fx"
              % (md, mc, (md / mc) if mc else 0))

    print("\nWHAT THIS DOES AND DOES NOT SETTLE")
    print("  Settles: whether DEX funding is structurally richer than CEX, and")
    print("  by how much once capital efficiency is accounted for.")
    print("\n  Does NOT settle:")
    print("   - capacity. A practitioner in the thread: 'ensure open interest is")
    print("     much higher than your position size, otherwise funding rates")
    print("     will turn against you.' At size you become the imbalance.")
    print("   - fees. Drift quotes zero taker on BTC perps; GMX and Hyperliquid")
    print("     differ. Round trips must be measured per venue, not assumed.")
    print("   - protocol risk. A CEX can freeze your account; a contract can")
    print("     lose the whole position. Neither is a drawdown.")
    print("   - decay. 'Rates shift right before the payout and swing back' --")
    print("     the same effect that turned our HOLO and JTO positions negative.")


if __name__ == "__main__":
    main()
