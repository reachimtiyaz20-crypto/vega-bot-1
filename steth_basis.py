#!/usr/bin/env python3
"""Does swapping the majors book's spot leg from ETH to stETH actually pay?

THE IDEA

Our majors book is long spot ETH, short ETH perp. The spot leg sits there doing
nothing but hedging. If it were stETH instead, the same hedged position would
also collect Ethereum staking yield -- roughly 3% on the NOTIONAL, which at
leverage is 3% x L on capital.

Naively that is enormous. At 10x it would add 30 points to a 19% book. Any time
arithmetic produces a number that large from a change that small, the arithmetic
is missing a constraint, and this script exists to find the constraint rather
than to confirm the number.

WHERE THE FREE LUNCH GOES -- three constraints, measured here

 1. DEPEG. stETH is not ETH. It trades at a floating discount and has broken
    hard before. At leverage L, an x% depeg costs x*L% of capital. This measures
    the actual distribution rather than assuming the peg holds.

 2. COLLATERAL DOES NOT NET. No CEX takes stETH as margin. To lever, the stETH
    sits on Aave while the perp short sits on a CEX. They do not offset. If ETH
    falls, the perp short PROFITS on the exchange while the Aave loan moves
    toward liquidation, and the two balances cannot see each other. Our current
    single-venue book has no such failure mode. This is a NEW risk, not a
    cheaper version of an old one.

 3. THE EXIT IS NOT INSTANT. Unstaking has a queue. In the moment you most want
    out, the secondary market is exactly where the discount is widest.

WHAT IT MEASURES

    stETH staking APR              from Lido
    stETH/ETH discount history     magnitude, frequency, worst case
    return at 1x, 3x, 5x, 10x      staking and funding levered, borrow netted
    liquidating depeg per leverage the discount that ends the position

The honest question is not "does staking yield exist" -- it does. It is whether
the leverage needed to make it material survives constraint 2, and whether the
depeg tail is small enough that constraint 1 does not eat the gain.

Run from ~/vega-bot:  python3 steth_basis.py
"""

import json
import os
import statistics
import time
import urllib.error
import urllib.request

CACHE = "data/steth"
BORROW_USD = 3.00           # OKX USDT, measured
ROUND_TRIP_BPS = 45.0       # spot + perp entry and exit, measured on the book
LEVERAGES = [1, 2, 3, 5, 10]


def get(url, timeout=30):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-steth/1.0",
                                               "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def cached(name, fn, ttl=86400):
    os.makedirs(CACHE, exist_ok=True)
    p = os.path.join(CACHE, name + ".json")
    if os.path.exists(p) and time.time() - os.path.getmtime(p) < ttl:
        try:
            return json.load(open(p))
        except Exception:
            pass
    d = fn()
    if d:
        json.dump(d, open(p, "w"))
    return d


def lido_apr():
    for url in ("https://eth-api.lido.fi/v1/protocol/steth/apr/sma",
                "https://eth-api.lido.fi/v1/protocol/steth/apr/last"):
        try:
            d = get(url)
            for path in (("data", "smaApr"), ("data", "apr"),
                         ("data", "aprs"), ("apr",)):
                cur = d
                for k in path:
                    cur = cur.get(k) if isinstance(cur, dict) else None
                    if cur is None:
                        break
                if isinstance(cur, (int, float)):
                    return float(cur)
                if isinstance(cur, list) and cur:
                    last = cur[-1]
                    if isinstance(last, dict) and "apr" in last:
                        return float(last["apr"])
        except Exception as e:
            print("  lido %s -> %s" % (url.rsplit("/", 1)[-1], e))
    return None


def steth_in_eth():
    """Daily stETH price denominated in ETH. 1.0 = perfect peg."""
    def go():
        url = ("https://api.coingecko.com/api/v3/coins/staked-ether/"
               "market_chart?vs_currency=eth&days=365&interval=daily")
        d = get(url)
        return d.get("prices") or []
    return cached("steth_eth", go)


def eth_funding():
    """Reuse the Binance ETH funding already cached by boros_predict.py."""
    p = "data/boros/binance_ETHUSDT.json"
    if not os.path.exists(p):
        return None
    try:
        rows = json.load(open(p))
    except Exception:
        return None
    if len(rows) < 100:
        return None
    gaps = [rows[i + 1][0] - rows[i][0] for i in range(min(300, len(rows) - 1))]
    gaps = [g for g in gaps if g > 0]
    per_year = 365.0 * 24 * 3600_000.0 / statistics.median(gaps)
    cutoff = time.time() * 1000 - 365 * 86400_000
    recent = [r[1] * per_year * 100.0 for r in rows if r[0] >= cutoff]
    return recent or None


def main():
    if not os.path.isdir("data"):
        raise SystemExit("run from ~/vega-bot")

    print("=" * 72)
    print("1. WHAT THE STAKING LEG PAYS")
    print("=" * 72)
    apr = lido_apr()
    if apr is None:
        apr = 3.0
        print("  Lido API unavailable -- assuming %.2f%% and flagging it" % apr)
    else:
        print("  Lido stETH APR: %.2f%%" % apr)

    print("\n" + "=" * 72)
    print("2. THE PEG -- stETH priced in ETH, 365 days")
    print("=" * 72)
    px = steth_in_eth()
    disc = []
    if not px:
        print("  price history unavailable; skipping the depeg distribution.")
        print("  DO NOT lever this without it -- the depeg tail is the whole risk.")
    else:
        disc = [(1.0 - p[1]) * 100.0 for p in px if p[1] and p[1] > 0]
        s = sorted(disc)

        def q(f):
            return s[min(int(f * (len(s) - 1)), len(s) - 1)]

        print("  %d daily observations" % len(s))
        print("  discount to ETH:  median %+.3f%%   mean %+.3f%%"
              % (statistics.median(s), statistics.mean(s)))
        print("  p50 %+.3f%%  p90 %+.3f%%  p99 %+.3f%%  WORST %+.3f%%"
              % (q(0.5), q(0.9), q(0.99), s[-1]))
        for thr in (0.25, 0.5, 1.0, 2.0):
            n = sum(1 for d in s if d >= thr)
            print("    discount >= %.2f%% on %d of %d days (%.1f%%)"
                  % (thr, n, len(s), 100.0 * n / len(s)))
        print("\n  NOTE: this window is only 365 days. stETH traded near 0.94")
        print("  in mid-2022 -- a 6%% discount. Nothing in a one-year sample")
        print("  bounds that, and at leverage it is the number that matters.")

    print("\n" + "=" * 72)
    print("3. THE FUNDING LEG (Binance ETH, trailing year)")
    print("=" * 72)
    f = eth_funding()
    if f is None:
        f_mean = 5.0
        print("  no cached funding -- run boros_predict.py first.")
        print("  assuming %.2f%% and flagging it" % f_mean)
    else:
        f_mean = statistics.mean(f)
        neg = sum(1 for x in f if x < 0)
        print("  %d settlements   mean %+.2f%%/yr   median %+.2f%%/yr"
              % (len(f), f_mean, statistics.median(f)))
        print("  negative on %d of %d (%.0f%%) -- when negative the short PAYS"
              % (neg, len(f), 100.0 * neg / len(f)))

    print("\n" + "=" * 72)
    print("4. WHAT IT ADDS, AND WHAT IT COSTS")
    print("=" * 72)
    print("  funding %+.2f%%/yr on notional | staking %.2f%% | borrow %.2f%%"
          % (f_mean, apr, BORROW_USD))
    print("  round trip %.0f bps charged once at entry\n" % ROUND_TRIP_BPS)
    print("%6s %12s %12s %12s %12s %14s"
          % ("lev", "ETH leg", "stETH leg", "added", "notional", "depeg to -50%"))
    for L in LEVERAGES:
        rt = ROUND_TRIP_BPS / 100.0
        eth_ret = L * f_mean - BORROW_USD * (L - 1) - rt
        st_ret = L * (f_mean + apr) - BORROW_USD * (L - 1) - rt
        # A depeg of x% costs x*L% of capital. What x halves the capital?
        kill = 50.0 / L
        print("%5dx %11.1f%% %11.1f%% %11.1f%% %11.1fx %13.2f%%"
              % (L, eth_ret, st_ret, st_ret - eth_ret, L, kill))

    if disc:
        s = sorted(disc)
        print("\n  Against the measured peg, worst discount in the window was")
        print("  %.3f%%. At 10x that is %.1f%% of capital; at 5x, %.1f%%."
              % (s[-1], s[-1] * 10, s[-1] * 5))

    print("\n" + "=" * 72)
    print("5. THE CONSTRAINT THE ARITHMETIC ABOVE IGNORES")
    print("=" * 72)
    print("""  Every 'added' figure above assumes you can lever stETH the way we
  lever ETH today. You cannot, and this is the finding that decides it:

    no CEX accepts stETH as margin.

  Today the majors book is ONE venue. Spot and perp sit together, the
  exchange nets them, and a price move cannot liquidate us because the
  legs offset inside the same account.

  With stETH the collateral moves to Aave and the perp stays on the CEX.
  They do not net. If ETH falls hard:

      the perp short GAINS      -- on the exchange
      the Aave position         -- moves toward liquidation
      and neither balance can see the other

  You would have to move margin between a DeFi protocol and a CEX,
  during the exact conditions where gas spikes and withdrawals queue.
  That is a new failure mode we have never carried, and it is worth
  more than the 3% it buys.

  WHAT IS ACTUALLY SAFE HERE: the UNLEVERED version. At 1x the staking
  yield is a clean addition with no borrow, no liquidation path, and a
  depeg exposure equal to the depeg itself rather than a multiple. Small,
  real, and it does not add a way to lose everything.""")


if __name__ == "__main__":
    main()
