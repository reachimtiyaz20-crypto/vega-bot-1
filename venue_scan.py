#!/usr/bin/env python3
"""Where are the cheapest dollars, and which venues can we actually use?

WHY THIS EXISTS

The whole leverage conclusion turned on ONE borrow rate from ONE venue read
ONCE: Bybit at 4.45%. Varying that single input to OKX's 3.00% moved the
answer at 10x from 6% to 19%. That is a three-fold swing from a number I never
checked twice, and it is the most expensive mistake of the project so far.

	funding 5.27% - borrow 4.45% = 0.82% per turn
	funding 5.27% - borrow 3.00% = 2.27% per turn
	funding 5.27% - borrow 1.00% = 4.27% per turn

Every basis point off the borrow rate is a basis point of pure edge, multiplied
by leverage. Finding the cheapest dollars is worth more than any strategy
refinement we have attempted.

WHAT IT SCANS

	DeFi     every lending market DefiLlama indexes, borrow side, with the
	         liquidity available to actually draw and the LTV allowed
	CEX      public interest-rate endpoints where venues publish them

INCENTIVISED BORROWING is checked explicitly. Some protocols pay you in tokens
to borrow, which can make the net cost near zero or negative. That is real
money but it is an emission with an end date, so gross and net are shown apart
-- the same discipline as separating base yield from reward yield.

WHAT IT CANNOT TELL YOU

Whether you can get the size on, whether the LTV survives a drawdown, and
whether the protocol is solvent. A cheap rate on $2M of available liquidity is
a real opportunity; the same rate on $40k is a screenshot.

Public endpoints only. No keys.

Run:  python3 venue_scan.py
"""

import json
import statistics
import sys
import urllib.error
import urllib.request

POOLS = "https://yields.llama.fi/pools"
LENDBORROW = "https://yields.llama.fi/lendBorrow"

STABLES = {"USDT", "USDC", "DAI", "USDS", "FDUSD", "TUSD", "USDT0",
           "PYUSD", "RLUSD", "GHO", "CRVUSD", "FRAX", "LUSD", "USD1"}

# Public interest-rate endpoints. Venues that do not publish one are listed
# with what we measured by hand, clearly labelled.
CEX_PUBLIC = [
    ("OKX", "https://www.okx.com/api/v5/public/interest-rate-loan-quota"),
]

CEX_MANUAL = [
    ("Bybit", "USDT", 4.45, "read in the UI 2026-08-26"),
]

# Venues worth considering for the trade itself: spot + perp on one venue is
# required for a hedged basis position, and unified margin is required for
# leverage on it.
VENUES = [
    # name, spot, perp, margin lending, unified/portfolio margin, notes
    ("Binance", True, True, True, True, "deepest books; our funding source"),
    ("Bybit", True, True, True, True, "UTA + PM + spot hedging confirmed on account"),
    ("OKX", True, True, True, True, "borrow 3.00% -- cheapest CEX found"),
    ("Gate", True, True, True, False, "wide altcoin listing"),
    ("Bitget", True, True, True, False, "depth measured: 2nd best on several alts"),
    ("KuCoin", True, True, True, False, ""),
    ("MEXC", True, True, False, False, "perp-only in our code; no spot leg built"),
    ("Hyperliquid", True, True, False, False, "on-chain; HLP vault 7.25% APR"),
]


def http(url, timeout=45):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-venue-scan/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def main():
    print("VENUES THAT COULD CARRY THE TRADE")
    print("%-14s %6s %6s %8s %8s  %s"
          % ("venue", "spot", "perp", "lending", "unified", "note"))
    for n, sp, pp, ml, um, note in VENUES:
        print("%-14s %6s %6s %8s %8s  %s"
              % (n, "yes" if sp else "-", "yes" if pp else "-",
                 "yes" if ml else "-", "yes" if um else "-", note))
    print("\n  A hedged basis position needs spot AND perp on the SAME venue,")
    print("  because margin does not net across exchanges. Leverage on top of")
    print("  it needs unified margin. That reduces the real candidate list to")
    print("  Binance, Bybit and OKX.")

    print("\n\nfetching DeFi lending markets...")
    try:
        meta = {p["pool"]: p for p in (http(POOLS).get("data") or [])}
        lb = http(LENDBORROW)
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
        sys.exit("fetch failed: %s" % e)

    rows = []
    for r in (lb if isinstance(lb, list) else lb.get("data") or []):
        pid = r.get("pool")
        m = meta.get(pid)
        if not m:
            continue
        sym = (m.get("symbol") or "").upper()
        if sym not in STABLES:
            continue
        gross = r.get("apyBaseBorrow")
        if gross is None:
            continue
        rew = r.get("apyRewardBorrow") or 0.0
        supply = r.get("totalSupplyUsd") or 0.0
        borrowed = r.get("totalBorrowUsd") or 0.0
        avail = max(supply - borrowed, 0.0)
        rows.append({
            "proj": m.get("project") or "?", "chain": m.get("chain") or "?",
            "sym": sym, "gross": float(gross), "rew": float(rew),
            "net": float(gross) - float(rew), "avail": avail,
            "ltv": r.get("ltv") or 0.0,
        })

    if not rows:
        sys.exit("no borrow markets matched")
    print("%d stablecoin borrow markets\n" % len(rows))

    print("CHEAPEST DOLLARS, with at least $5M available to draw")
    print("%-20s %-12s %-7s %9s %9s %9s %6s %11s"
          % ("project", "chain", "asset", "gross", "reward", "NET", "LTV", "available"))
    big = [r for r in rows if r["avail"] >= 5e6]
    for r in sorted(big, key=lambda r: r["net"])[:20]:
        print("%-20s %-12s %-7s %8.2f%% %8.2f%% %8.2f%% %5.0f%% %10.0fM"
              % (r["proj"][:20], r["chain"][:12], r["sym"][:7],
                 r["gross"], r["rew"], r["net"], r["ltv"] * 100,
                 r["avail"] / 1e6))

    print("\nCHEAPEST BY GROSS RATE ONLY (ignoring token rewards)")
    print("Rewards end. Gross is what you owe when they do.")
    for r in sorted(big, key=lambda r: r["gross"])[:12]:
        print("  %-20s %-10s %-7s gross %6.2f%%  $%.0fM available"
              % (r["proj"][:20], r["chain"][:10], r["sym"][:7],
                 r["gross"], r["avail"] / 1e6))

    if big:
        g = sorted(r["gross"] for r in big)
        n = sorted(r["net"] for r in big)
        print("\nDISTRIBUTION across %d markets with $5M+ available" % len(big))
        print("  gross borrow: min %.2f%%  median %.2f%%  p75 %.2f%%"
              % (g[0], statistics.median(g), g[int(0.75 * (len(g) - 1))]))
        print("  net of rewards: min %.2f%%  median %.2f%%"
              % (n[0], statistics.median(n)))

    print("\n\nCEX BORROW, published endpoints")
    for name, url in CEX_PUBLIC:
        try:
            d = http(url, timeout=20)
            for row in (d.get("data") or [])[:1]:
                for b in (row.get("basic") or []):
                    if b.get("ccy") in ("USDT", "USDC"):
                        print("  %-8s %-6s %.2f%%/yr (base tier)"
                              % (name, b.get("ccy"),
                                 float(b.get("rate", 0)) * 100 * 365))
        except Exception as e:
            print("  %-8s lookup failed: %s" % (name, e))
    for name, ccy, rate, note in CEX_MANUAL:
        print("  %-8s %-6s %.2f%%/yr  (%s)" % (name, ccy, rate, note))

    print("\n\nWHAT THIS CHANGES")
    print("  Leverage economics are decided by the SPREAD, and the borrow side")
    print("  of that spread is the part we can shop for. Funding is what it is;")
    print("  the cost of dollars is negotiable across venues and chains.")
    print("\n  But note the constraint above: a hedged basis position needs spot")
    print("  and perp on ONE venue. Borrowing cheaply on a chain and trading on")
    print("  a CEX means moving collateral, which costs time, gas and adds")
    print("  bridge risk -- and the position cannot be margined against the")
    print("  borrowed dollars sitting somewhere else.")
    print("\n  So the practical question is not 'what is the cheapest rate in")
    print("  the world' but 'what is the cheapest rate ON A VENUE THAT CAN ALSO")
    print("  HOLD THE HEDGE'. On that test the list is short: Binance, Bybit,")
    print("  OKX -- and OKX at 3.00%% is currently the cheapest of the three.")


if __name__ == "__main__":
    main()
