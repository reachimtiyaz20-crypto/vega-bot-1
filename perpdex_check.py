#!/usr/bin/env python3
"""The column that aggregator left out: open interest.

WHAT IS BEING CHECKED

PerpDexList advertises cross-DEX funding arbitrage "up to 1000% APY": AXS at
1240%, SAHARA 891%, WTI 592%, YZY 571%. The underlying rates are things like
-2028% on Aster against +2116% on Lighter.

Funding rates of plus or minus two thousand percent do not occur in markets
with participants. They occur in markets with almost nobody. This project has
now hit that pattern seven times:

	CYBER    4390% APR on $4,010 of open interest
	AAVE    16766% APR on zero
	COTI     -109 bps/hr, no venue would lend it
	STORJ     -24 bps/hr, $5.10 resting on the perp touch
	new listings   0 of 35 had a spot pair to hedge against

Every time, the rate was extreme BECAUSE the trade could not be built.

Two other tells on that page. The value +10.95% appears repeatedly across
different venues and coins -- that is exactly the neutral baseline of 0.01%
per 8h annualised, i.e. the default a venue reports when there is no real
market. An "arbitrage" between a genuine extreme and a placeholder is not an
arbitrage. And their own worked example is internally inconsistent: $20k of
capital earning $8,000 (40%/yr) in one sentence, $100k earning $12,000
(12%/yr) in the next.

WHY IT STILL MATTERS

These are perp DEXs. No KYC, no geographic gating. Bybit error 10024 -- the
regulatory block on API access -- does not apply to any of them. For an account
locked out of CEX APIs by jurisdiction, this may be the only route that stays
open. So the question is not whether the page is honest; it is whether ANY of
these markets have real depth.

Hyperliquid publishes open interest, so its column can be checked directly.

Public endpoint, no keys.  Run:  python3 perpdex_check.py
"""

import json
import sys
import urllib.error
import urllib.request

API = "https://api.hyperliquid.xyz/info"

# Coins advertised on the PerpDexList funding-arbitrage page.
WANTED = ["AXS", "SAHARA", "YZY", "TOWNS", "GRIFFAIN", "TOSHI", "IR",
          "DOT", "KITE", "WTI"]

# What the page claimed for the Hyperliquid leg, where it showed one.
CLAIMED = {"AXS": -1980.15, "YZY": -891.59, "GRIFFAIN": -10.95, "DOT": -161.66}

BASELINE = 10.95     # 0.01% per 8h annualised -- the "no real market" default


def post(payload):
    req = urllib.request.Request(
        API, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json",
                 "User-Agent": "vega-perpdex/1.0"})
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.loads(r.read().decode())


def main():
    print("checking the PerpDexList coins against Hyperliquid's own data\n")
    try:
        meta, ctxs = post({"type": "metaAndAssetCtxs"})
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
        sys.exit("fetch failed: %s" % e)

    universe = (meta or {}).get("universe") or []
    if not universe or not ctxs:
        sys.exit("unexpected response shape")

    rows = {}
    for u, c in zip(universe, ctxs):
        name = u.get("name")
        if not name:
            continue
        try:
            # Hourly funding rate -> annualised percent.
            fund = float(c.get("funding") or 0.0) * 8760.0 * 100.0
            oi = float(c.get("openInterest") or 0.0)
            px = float(c.get("markPx") or 0.0)
        except (TypeError, ValueError):
            continue
        rows[name] = {"fund": fund, "oi_usd": oi * px, "px": px,
                      "vol": float(c.get("dayNtlVlm") or 0.0)}

    print("%d markets on Hyperliquid\n" % len(rows))

    print("THE ADVERTISED COINS")
    print("%-10s %13s %13s %15s %15s"
          % ("coin", "claimed %/yr", "actual %/yr", "open interest", "24h volume"))
    found = 0
    for c in WANTED:
        r = rows.get(c)
        claim = CLAIMED.get(c)
        if not r:
            print("%-10s %13s %13s %15s %15s"
                  % (c, ("%.0f%%" % claim) if claim else "-",
                     "NOT LISTED", "-", "-"))
            continue
        found += 1
        print("%-10s %13s %12.0f%% %14s %14s"
              % (c, ("%.0f%%" % claim) if claim else "-", r["fund"],
                 "$%.0f" % r["oi_usd"], "$%.0f" % r["vol"]))

    print("\n%d of %d advertised coins are listed on Hyperliquid at all"
          % (found, len(WANTED)))

    # How much of the whole venue is in markets too small to matter?
    ois = sorted((r["oi_usd"], n) for n, r in rows.items())
    tiny = [x for x in ois if x[0] < 100_000]
    print("\nACROSS ALL %d HYPERLIQUID MARKETS" % len(rows))
    print("  with open interest under $100k: %d (%.0f%%)"
          % (len(tiny), 100.0 * len(tiny) / len(rows)))
    print("  with open interest over $10M:   %d"
          % sum(1 for x in ois if x[0] > 10_000_000))

    # The core test: is extreme funding concentrated in empty markets?
    print("\nFUNDING vs DEPTH -- the relationship the aggregator omitted")
    bands = [(0, 100_000, "under $100k"), (100_000, 1_000_000, "$100k-1M"),
             (1_000_000, 10_000_000, "$1M-10M"), (10_000_000, 1e18, "over $10M")]
    print("%-14s %8s %14s %14s" % ("open interest", "markets", "median |f|%/yr",
                                   "max |f|%/yr"))
    for lo, hi, label in bands:
        grp = [abs(r["fund"]) for r in rows.values()
               if lo <= r["oi_usd"] < hi]
        if not grp:
            continue
        grp.sort()
        print("%-14s %8d %13.1f%% %13.1f%%"
              % (label, len(grp), grp[len(grp) // 2], grp[-1]))

    # And the ones that are actually worth something.
    print("\nRICHEST FUNDING WITH AT LEAST $1M OPEN INTEREST")
    good = [(abs(r["fund"]), n, r) for n, r in rows.items()
            if r["oi_usd"] >= 1_000_000]
    good.sort(reverse=True)
    print("%-10s %13s %15s %15s" % ("coin", "%/yr", "open interest", "24h volume"))
    for f, n, r in good[:12]:
        print("%-10s %12.1f%% %14s %14s"
              % (n, r["fund"], "$%.0f" % r["oi_usd"], "$%.0f" % r["vol"]))

    print("\nHOW TO READ IT")
    print("  If the extreme rates sit in the 'under $100k' band and the $1M+")
    print("  band is boring, the page is selling the same illusion this")
    print("  project has now measured seven times.")
    print("  The $1M+ table is the real opportunity set on this venue. It is")
    print("  smaller and duller than any aggregator headline, and it is the")
    print("  only part that can hold money.")
    print("\n  NOTE: this checks the HYPERLIQUID column only. Aster, Lighter,")
    print("  EdgeX and Variational are not verified here, and a cross-venue")
    print("  trade needs capital on BOTH sides with no margin netting and")
    print("  counterparty risk at two small venues.")


if __name__ == "__main__":
    main()
