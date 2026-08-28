#!/usr/bin/env python3
"""What GMX pays a delta-neutral short, netting the borrowing fee.

WHY GMX

A practitioner reported 34%/yr, after fees, on $25,000, shorting BTC on GMX
with WBTC as collateral -- one position, delta zero, nothing borrowed. He
quoted 78.64% APR on the short side.

GMX differs from everything else measured here in two ways:

	STRUCTURE   collateral IS the long leg. $1 capital carries $1 notional.
	            On a CEX: buy spot AND post margin, ~$1.25 per $1, and beyond
	            that you borrow at 4.45%. The "leverage needs borrowing so
	            leverage is worthless" conclusion was an artifact of the CEX
	            structure and does not apply here.
	MECHANISM   a pool, not an order book. The dominant side pays the minority
	            side and no external arbitrage flattens it.

Measured elsewhere: Binance 0.9%/yr on capital, Hyperliquid 3.4%, dYdX -4.0%.

SCHEMA AND SIGN, both inferred and both checked

The API returns fundingRateLong/Short, borrowingRateLong/Short and
openInterestLong/Short, all scaled by 1e30. Dividing by 1e30 gives sane annual
fractions -- the sample market shows 5.7% borrowing and 16.2% funding. Every
other scaling produces absurdities.

Sign: on the sample, shorts hold the LARGER open interest AND the POSITIVE
funding rate. GMX charges the dominant side, so POSITIVE means that side PAYS.
That is an inference from one market, so the script CHECKS it across all of
them and reports how often it holds. If it holds rarely, the convention is
wrong and every number below is inverted -- better to know than to assume.

WHAT A SHORT NETS

	receives  -fundingRateShort   (negative rate = receiving)
	pays       borrowingRateShort (charged regardless of side)

Quoting the funding leg alone is how a 78% APR gets advertised.

Public endpoint, no keys.  Run:  python3 gmx_funding.py
"""

import json
import statistics
import sys
import urllib.error
import urllib.request

BASE = "https://arbitrum-api.gmxinfra.io"
E30 = 10.0 ** 30


def get(path, timeout=30):
    req = urllib.request.Request(BASE + path,
                                 headers={"User-Agent": "vega-gmx/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def f(d, k):
    v = d.get(k)
    if v in (None, ""):
        return None
    try:
        return float(v) / E30
    except (TypeError, ValueError):
        return None


def main():
    print("fetching GMX v2 market state\n")
    try:
        d = get("/markets/info")
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
        sys.exit("fetch failed: %s" % e)
    markets = d.get("markets") if isinstance(d, dict) else d
    if not markets:
        sys.exit("no markets returned")
    print("%d markets\n" % len(markets))

    rows, agree, total = [], 0, 0
    for m in markets:
        fl, fs = f(m, "fundingRateLong"), f(m, "fundingRateShort")
        bl, bs = f(m, "borrowingRateLong"), f(m, "borrowingRateShort")
        ol, os_ = f(m, "openInterestLong"), f(m, "openInterestShort")
        if None in (fl, fs, bs) or ol is None or os_ is None:
            continue
        if ol <= 0 and os_ <= 0:
            continue

        # Convention check: does the side with more open interest carry the
        # positive (paying) rate?
        if abs(ol - os_) / max(ol + os_, 1e-9) > 0.02:
            total += 1
            dominant_is_short = os_ > ol
            positive_is_short = fs > fl
            if dominant_is_short == positive_is_short:
                agree += 1

        # Short: receives when its rate is negative, always pays borrowing.
        net_short = (-fs) - (bs or 0.0)
        net_long = (-fl) - (bl or 0.0)
        rows.append({
            "name": m.get("name") or "?",
            "fs": fs, "bs": bs or 0.0, "net_s": net_short,
            "fl": fl, "bl": bl or 0.0, "net_l": net_long,
            "ol": ol, "os": os_,
        })

    if not rows:
        sys.exit("no usable markets")

    if total:
        print("SIGN CHECK: dominant side carries the positive rate in "
              "%d of %d skewed markets (%.0f%%)"
              % (agree, total, 100.0 * agree / total))
        if agree < 0.7 * total:
            print("  BELOW 70%% -- the convention is NOT as assumed and every")
            print("  number below may be inverted. Treat with suspicion.\n")
        else:
            print("  convention holds: positive = that side PAYS\n")

    best = sorted(rows, key=lambda r: -r["net_s"])
    print("BEST MARKETS FOR A DELTA-NEUTRAL SHORT  (annual, on capital)")
    print("%-26s %10s %10s %10s %11s %11s"
          % ("market", "funding", "borrow", "NET", "OI long $M", "OI short $M"))
    for r in best[:15]:
        print("%-26s %9.1f%% %9.1f%% %9.1f%% %10.1f %10.1f"
              % (r["name"][:26], -r["fs"] * 100, r["bs"] * 100,
                 r["net_s"] * 100, r["ol"] / 1e6, r["os"] / 1e6))

    print("\nBEST FOR A DELTA-NEUTRAL LONG (collateral in stables, long the perp)")
    for r in sorted(rows, key=lambda x: -x["net_l"])[:8]:
        print("%-26s %9.1f%% %9.1f%% %9.1f%% %10.1f %10.1f"
              % (r["name"][:26], -r["fl"] * 100, r["bl"] * 100,
                 r["net_l"] * 100, r["ol"] / 1e6, r["os"] / 1e6))

    # Only markets with real size are worth anything.
    big = [r for r in rows if min(r["ol"], r["os"]) >= 1e6]
    print("\nWITH AT LEAST $1M ON THE SMALLER SIDE (%d markets)" % len(big))
    for r in sorted(big, key=lambda x: -x["net_s"])[:10]:
        smaller = min(r["ol"], r["os"]) / 1e6
        print("%-26s net %8.1f%%/yr   smaller side $%.1fM"
              % (r["name"][:26], r["net_s"] * 100, smaller))

    nets = [r["net_s"] * 100 for r in big]
    if nets:
        print("\nacross those: median %.1f%%  best %.1f%%  worst %.1f%%"
              % (statistics.median(nets), max(nets), min(nets)))

    print("\n" + "=" * 68)
    print("COMPARISON, all delta-neutral, on capital, unlevered")
    print("  Binance (CEX)         0.9%/yr    measured, 180 days")
    print("  Hyperliquid           3.4%/yr    measured, 180 days")
    print("  dYdX v4              -4.0%/yr    measured, negative")
    if big:
        b = max(big, key=lambda r: r["net_s"])
        print("  GMX best (>$1M)    %7.1f%%/yr    this snapshot: %s"
              % (b["net_s"] * 100, b["name"][:30]))
    print("  claim, mid-2025        34%/yr    GMX, realised, $25k, receipts")

    print("\nTHE CAPACITY CONSTRAINT IS THE SMALLER SIDE")
    print("  The imbalance IS the payment. Take a position comparable to the")
    print("  smaller side and you become the imbalance -- the rate moves")
    print("  against you. That is what the practitioner meant by 'ensure open")
    print("  interest is much higher than your position size'.")
    print("\n  ONE SNAPSHOT. If this justifies it, GMX history lives on the")
    print("  Subsquid GraphQL endpoint and deserves the same treatment we gave")
    print("  Binance's five years.")


if __name__ == "__main__":
    main()
