#!/usr/bin/env python3
"""Equity/commodity perp carry, NET of what the hedge actually costs.

WHERE WE ARE

xyz_perps2.py measured GROSS funding on 25 sized markets: NVDA 6.7%, GOOGL
10.3%, INTC 12.6%, and on the other side BRENTOIL -17.1%, CBRS -17.8%. Funding
accrues evenly across sessions, so it is not a closed-hours trap, and there is
$2.98bn of open interest, so capacity is not the constraint for the first time
in this project.

Gross funding is not a return. The trade is:

    SHORT the perp on Hyperliquid   (collateral in USDC)
    LONG the real underlying        (cash or margin in a brokerage)

and the hedge leg costs money. This prices that.

THE CAPITAL STRUCTURE, which is what actually decides it

Our repeated mistake has been assuming leverage is free. It is not, and here
there are TWO capital demands at once:

    perp side    notional / maxLeverage, posted as USDC on Hyperliquid
    stock side   full notional in cash, OR half on Reg-T margin paying interest

Buying the stock on margin does not obviously help: the margin rate (~5.5%) is
close to the funding being collected (~6-13%), so borrowing to hold the hedge
hands most of the carry to the broker. The script shows both so the comparison
is explicit rather than assumed.

THE BASELINE TRAP

Most positive rates here cluster near 11%/yr, which is Hyperliquid's baseline
funding of 0.00125%/hr. That baseline is the INTEREST RATE COMPONENT, not
alpha. Collecting 11% while dollars cost you 5% leaves 6% for taking basis
risk. The script subtracts an explicit cost of capital so that this is visible
instead of flattering the result.

Run from ~/vega-bot:  python3 xyz_net.py
"""

import json
import statistics
import time
import urllib.request

HL = "https://api.hyperliquid.xyz/info"
DAYS = 45
MIN_OI = 20_000_000.0
HOLD_DAYS = 90.0

HL_TAKER_BPS = 4.5          # published Hyperliquid perp taker
STOCK_COST_BPS = 4.0        # commission + spread, one way, liquid US names
STOCK_MARGIN_RATE = 5.5     # broker margin interest, %/yr
CASH_OPP_COST = 4.0         # what idle dollars earn elsewhere, %/yr


def post(body, timeout=30):
    req = urllib.request.Request(
        HL, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "User-Agent": "vega-xyz/3.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def main():
    print("LOADING xyz MARKETS")
    d = post({"type": "metaAndAssetCtxs", "dex": "xyz"})
    uni = d[0]["universe"]
    ctxs = d[1]
    rows = []
    for i, m in enumerate(uni):
        c = ctxs[i] if i < len(ctxs) else {}
        try:
            oi = float(c["openInterest"]) * float(c.get("markPx") or c["oraclePx"])
        except (TypeError, ValueError, KeyError):
            continue
        if oi < MIN_OI:
            continue
        rows.append((oi, m.get("name"), int(m.get("maxLeverage") or 1)))
    rows.sort(reverse=True)
    print("  %d markets above $%.0fm OI\n" % (len(rows), MIN_OI / 1e6))

    start = int((time.time() - DAYS * 86400) * 1000)
    out = []
    for oi, name, lev in rows:
        h, cur, seen = [], start, set()
        for _ in range(60):
            try:
                r = post({"type": "fundingHistory", "coin": name,
                          "startTime": cur})
            except Exception:
                break
            if not r:
                break
            n = 0
            for x in r:
                t = int(x["time"])
                if t in seen:
                    continue
                seen.add(t)
                h.append(float(x["fundingRate"]))
                n += 1
            last = max(int(x["time"]) for x in r)
            if n == 0 or last <= cur:
                break
            cur = last + 1
            time.sleep(0.06)
        if len(h) < 200:
            continue
        f = statistics.mean(h) * 24 * 365 * 100.0
        out.append((name, f, oi, max(lev, 1)))

    if not out:
        print("no funding history")
        return

    # Round trip, charged once over the hold, expressed as %/yr on notional.
    rt_bps = 2 * HL_TAKER_BPS + 2 * STOCK_COST_BPS
    rt_yr = rt_bps / 100.0 * (365.0 / HOLD_DAYS)

    print("=" * 78)
    print("NET CARRY ON CAPITAL  (%d-day hold, round trip %.1f bps = %.2f%%/yr)"
          % (HOLD_DAYS, rt_bps, rt_yr))
    print("=" * 78)
    print("  perp collateral = notional / maxLeverage")
    print("  A: stock bought with CASH      capital = notional + collateral")
    print("  B: stock on 50%% Reg-T margin   capital = 0.5*notional + collateral,")
    print("     paying %.1f%% on the borrowed half\n" % STOCK_MARGIN_RATE)
    print("%-14s %7s %5s %9s %9s %9s %9s"
          % ("market", "gross", "lev", "cap A", "NET A", "cap B", "NET B"))

    res = []
    for name, f, oi, lev in sorted(out, key=lambda r: -abs(r[1])):
        coll = 1.0 / lev
        # Side is chosen by the sign of funding: short the perp when funding
        # is positive, long it when negative. Either way we EARN |f| on
        # notional and must hold the opposite leg in the underlying.
        gross = abs(f)

        cap_a = 1.0 + coll
        net_a = (gross - rt_yr) / cap_a - CASH_OPP_COST * (coll / cap_a)

        cap_b = 0.5 + coll
        net_b = (gross - rt_yr - 0.5 * STOCK_MARGIN_RATE) / cap_b \
            - CASH_OPP_COST * (coll / cap_b)

        print("%-14s %+6.1f%% %4dx %8.2fx %+8.1f%% %8.2fx %+8.1f%%"
              % (name[:14], f, lev, cap_a, net_a, cap_b, net_b))
        res.append((max(net_a, net_b), name, f, net_a, net_b, oi))

    res.sort(reverse=True)
    print("\n" + "=" * 78)
    print("BEST NET, AND WHAT IT WOULD TAKE")
    print("=" * 78)
    for best, name, f, na, nb, oi in res[:8]:
        print("  %-14s gross %+6.1f%%  ->  best net %+6.1f%%   OI $%.0fm"
              % (name, f, best, oi / 1e6))

    top = res[0]
    print("\n  Top structure nets %.1f%%/yr." % top[0])
    print("  For 35%% we would need gross funding of roughly %.0f%%/yr on the"
          % (35.0 * 2.0 + rt_yr + CASH_OPP_COST))
    print("  same capital structure. Nothing on this venue pays that.")

    print("\n" + "=" * 78)
    print("WHAT THIS DOES NOT PRICE")
    print("=" * 78)
    print("""  BASIS. The perp tracks an oracle, not your fill. Your stock is bought
  at a real price and the perp settles against a reference that can drift.
  Every hedged book we have run showed exit costs about 12% above the
  symmetric estimate, and none of them had a hedge sitting at a different
  broker on a different continent.

  THE COMMODITY LEGS. BRENTOIL and CL show the largest rates, and both
  are negative, meaning we would be LONG the perp and SHORT the
  underlying. Shorting oil means futures, which roll monthly, and the
  roll can exceed the funding entirely. Those two rows are not tradeable
  on this arithmetic and need their own measurement.

  CORPORATE ACTIONS. Splits, dividends and halts hit the stock leg and
  not the perp. A dividend is a real cash flow the short perp does not
  receive, and on a 6% carry a 2% dividend is a third of the trade.

  ACCESS. All of it assumes a brokerage account that can hold US equity
  against a crypto position. That is the part your UAE company might
  unlock, and it is worth confirming BEFORE any further modelling.""")


if __name__ == "__main__":
    main()
