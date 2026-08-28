#!/usr/bin/env python3
"""Stop judging the whole market by one broker.

THE GAP THIS CLOSES

Every leverage conclusion in this project rests on ONE number: Bybit's USDT
borrow rate of 4.45%, read once, on one afternoon. From that single input I
concluded that funding (5.27%) barely exceeds the cost of dollars, so each turn
of leverage nets 0.16% and leverage is worthless.

Change that one input and the conclusion inverts:

	funding 5.27%  -  borrow 4.45%  =  0.82% per turn  ->  pointless
	funding 5.27%  -  borrow 2.00%  =  3.27% per turn  ->  5x adds ~13%/yr

Nothing requires borrowing from the venue you trade on. If dollars are cheaper
somewhere else, the carry trade changes character entirely -- and I never
checked.

The same narrowness applies to the yields. 6.83% (Bybit Earn), 7.25% (HLP) and
8.25% (Felix) were three places I happened to look, and I presented their
agreement as a market-wide convergence at 7-8%. Three samples is not a
distribution.

WHAT THIS MEASURES

	stablecoin supply yields   across every protocol DefiLlama indexes
	stablecoin borrow costs    where published
	the spread between them    which is what decides whether leverage works

TVL filtering is not optional. A pool paying 40% on $200k of TVL is not an
opportunity, it is a warning -- either the yield is a token emission about to
end, or the pool is small because people who understand it are staying away.
Rates are shown by TVL band so the trade-off is visible rather than hidden.

WHAT IT STILL WILL NOT TELL YOU

Whether the protocol is safe. Yield is measurable; solvency is not. Every
number here is a payment that was made, not a promise that will be kept, and
the highest rates cluster in exactly the places most likely to stop paying.

Public endpoints only. No keys.

Run:  python3 rate_survey.py
"""

import collections
import json
import statistics
import sys
import urllib.error
import urllib.request

LLAMA = "https://yields.llama.fi/pools"
OKX = "https://www.okx.com/api/v5/public/interest-rate-loan-quota"

STABLES = {"USDT", "USDC", "DAI", "USDE", "SUSDE", "USDS", "FDUSD", "TUSD",
           "USDT0", "FEUSD", "LIMUSD", "USD1", "PYUSD", "RLUSD", "GHO",
           "CRVUSD", "FRAX", "USDD", "LUSD", "SUSDS", "USDY"}

BANDS = [(1e9, "over $1B"), (2e8, "$200M-1B"), (5e7, "$50-200M"),
         (1e7, "$10-50M"), (1e6, "$1-10M"), (0, "under $1M")]

# Our own measurements, for comparison.
OURS = [("Bybit USDT borrow", 4.45, "measured 2026-08-26"),
        ("Bybit Earn USDT", 6.83, "promotional tier"),
        ("HLP vault (venue APR)", 7.25, "venue reported"),
        ("Felix USDT0", 8.25, "published"),
        ("our funding carry", 4.00, "5y measured, net")]


def http(url):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-rates/1.0"})
    with urllib.request.urlopen(req, timeout=40) as r:
        return json.loads(r.read().decode())


def band_of(tvl):
    for lo, name in BANDS:
        if tvl >= lo:
            return name
    return "under $1M"


def main():
    print("OUR OWN NUMBERS, for reference")
    for name, rate, note in OURS:
        print("  %-26s %6.2f%%   %s" % (name, rate, note))

    print("\nfetching the wider market from DefiLlama...")
    try:
        d = http(LLAMA)
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
        sys.exit("fetch failed: %s" % e)
    pools = d.get("data") or []
    if not pools:
        sys.exit("no pools returned")
    print("%d pools indexed\n" % len(pools))

    stab = []
    for p in pools:
        sym = (p.get("symbol") or "").upper()
        if not p.get("stablecoin"):
            continue
        # Single-asset stable exposure only. LP pairs carry different risk and
        # comparing them to a deposit rate is comparing two unlike things.
        if "-" in sym or "/" in sym:
            continue
        if sym not in STABLES:
            continue
        apy = p.get("apy")
        tvl = p.get("tvlUsd") or 0
        if apy is None or tvl <= 0:
            continue
        stab.append({
            "sym": sym, "apy": float(apy), "tvl": float(tvl),
            "base": p.get("apyBase") or 0.0, "rew": p.get("apyReward") or 0.0,
            "proj": p.get("project") or "?", "chain": p.get("chain") or "?",
        })

    if not stab:
        sys.exit("no single-asset stablecoin pools matched")
    print("%d single-asset stablecoin pools\n" % len(stab))

    print("YIELD BY TVL BAND -- size is the crudest risk proxy we have")
    print("%-14s %7s %9s %9s %9s %9s" %
          ("TVL band", "pools", "median", "p75", "p90", "max"))
    for lo, name in BANDS:
        grp = [p for p in stab if band_of(p["tvl"]) == name]
        if not grp:
            continue
        a = sorted(p["apy"] for p in grp)
        print("%-14s %7d %8.2f%% %8.2f%% %8.2f%% %8.2f%%"
              % (name, len(grp), statistics.median(a),
                 a[int(0.75 * (len(a) - 1))], a[int(0.90 * (len(a) - 1))], a[-1]))

    print("\nBEST YIELDS ON SERIOUS SIZE (TVL over $50M)")
    big = sorted([p for p in stab if p["tvl"] >= 5e7],
                 key=lambda p: -p["apy"])[:20]
    print("%-20s %-12s %-8s %9s %9s %9s %11s"
          % ("project", "chain", "asset", "APY", "base", "reward", "TVL"))
    for p in big:
        print("%-20s %-12s %-8s %8.2f%% %8.2f%% %8.2f%% %10.0fM"
              % (p["proj"][:20], p["chain"][:12], p["sym"][:8],
                 p["apy"], p["base"], p["rew"], p["tvl"] / 1e6))

    print("\nBASE YIELD ONLY (excluding token rewards, TVL over $50M)")
    print("Reward APY is paid in a token whose price is a bet. Base is what")
    print("borrowers actually pay, and it is the number that persists.")
    bigbase = sorted([p for p in stab if p["tvl"] >= 5e7],
                     key=lambda p: -(p["base"] or 0))[:15]
    for p in bigbase:
        print("  %-20s %-10s %-8s base %6.2f%%  (total %6.2f%%)  $%.0fM"
              % (p["proj"][:20], p["chain"][:10], p["sym"][:8],
                 p["base"], p["apy"], p["tvl"] / 1e6))

    allapy = sorted(p["apy"] for p in stab if p["tvl"] >= 1e7)
    if allapy:
        print("\nACROSS EVERYTHING ABOVE $10M TVL (%d pools)" % len(allapy))
        print("  median %.2f%%   p75 %.2f%%   p90 %.2f%%   p99 %.2f%%"
              % (statistics.median(allapy),
                 allapy[int(0.75 * (len(allapy) - 1))],
                 allapy[int(0.90 * (len(allapy) - 1))],
                 allapy[int(0.99 * (len(allapy) - 1))]))

    # CEX borrow, where it is public.
    print("\nCEX BORROW RATES (public endpoints)")
    try:
        o = http(OKX)
        for row in (o.get("data") or [])[:1]:
            for b in (row.get("basic") or []):
                if b.get("ccy") in ("USDT", "USDC"):
                    print("  OKX %-6s borrow %.2f%%/yr (base tier)"
                          % (b.get("ccy"),
                             float(b.get("rate", 0)) * 100 * 365))
    except Exception as e:
        print("  OKX lookup failed: %s" % e)
    print("  Bybit USDT  borrow 4.45%/yr  (measured in the UI)")

    print("\nWHAT TO TAKE FROM THIS")
    print("  If the median large-TVL stablecoin yield is near 7-8%, the")
    print("  convergence I claimed is real and was not an artifact of looking")
    print("  at four venues.")
    print("  If borrowing is materially cheaper than 4.45% somewhere credible,")
    print("  the leverage conclusion needs redoing -- the spread, not the")
    print("  funding rate, is what decides whether leverage pays.")
    print("  And separate BASE from REWARD everywhere. Base yield is paid by")
    print("  borrowers and persists; reward yield is an emission with an end")
    print("  date, and treating the two as one number is how farms look better")
    print("  than they are.")


if __name__ == "__main__":
    main()
