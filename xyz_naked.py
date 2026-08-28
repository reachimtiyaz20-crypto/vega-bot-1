#!/usr/bin/env python3
"""If you just HELD the funding-receiving side unhedged, did you make money?

WHY THIS EXISTS

I have been dismissing the big rates -- CXMT -169.7%, UNITREE -65.7%,
BRENTOIL -17.1% -- on the grounds that the underlying cannot be shorted, so the
carry cannot be hedged. That is true, and it is also an ARGUMENT rather than a
measurement, which is exactly the laziness that let a wrong 19% stand for weeks.

There IS a way to collect those rates: take the funding-receiving side and hold
it naked. The funding is real money, paid hourly, into the account. The only
question is whether the price moved against you faster than the funding paid.

That is arithmetic, not opinion, so here it is.

WHAT IS MEASURED, per market, over 45 days

    side          long the perp if funding is negative (longs are paid),
                  short the perp if funding is positive (shorts are paid)
    funding       what actually accrued, hour by hour, at the rate observed
                  BEFORE each hour -- never the rate that arrived after
    price         what the position lost or gained on the mark
    TOTAL         the two together, which is the whole answer
    max drawdown  the worst peak-to-trough on the equity path
    max safe lev  roughly 1/drawdown -- above this you were liquidated
                  BEFORE the funding ever arrived

THE TRAP THIS IS BUILT TO CATCH

A position can show a magnificent total return and still be untradeable, because
the drawdown along the way would have closed it. 169%/yr sounds enormous; it is
0.019% per hour. A 20% adverse move is 1,000 hours of funding, and at 10x
leverage a 10% move is the whole account. So the drawdown column matters more
than the total column.

NO CHERRY-PICKING: every market above the OI floor is included and reported,
winners and losers alike.

Run from ~/vega-bot:  python3 xyz_naked.py
"""

import json
import statistics
import time
import urllib.request

HL = "https://api.hyperliquid.xyz/info"
DAYS = 45
MIN_OI = 20_000_000.0


def post(body, timeout=40):
    req = urllib.request.Request(
        HL, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "User-Agent": "vega-xyz/4.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def funding_series(coin, start):
    out, cur, seen = {}, start, set()
    for _ in range(60):
        try:
            r = post({"type": "fundingHistory", "coin": coin, "startTime": cur})
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
            out[t // 3600000] = float(x["fundingRate"])
            n += 1
        last = max(int(x["time"]) for x in r)
        if n == 0 or last <= cur:
            break
        cur = last + 1
        time.sleep(0.05)
    return out


def price_series(coin, start, end):
    try:
        c = post({"type": "candleSnapshot",
                  "req": {"coin": coin, "interval": "1h",
                          "startTime": start, "endTime": end}})
    except Exception:
        return {}
    out = {}
    for k in (c or []):
        try:
            out[int(k["t"]) // 3600000] = float(k["c"])
        except (KeyError, TypeError, ValueError):
            continue
    return out


def main():
    print("LOADING")
    d = post({"type": "metaAndAssetCtxs", "dex": "xyz"})
    uni, ctxs = d[0]["universe"], d[1]
    mk = []
    for i, m in enumerate(uni):
        c = ctxs[i] if i < len(ctxs) else {}
        try:
            oi = float(c["openInterest"]) * float(c.get("markPx") or c["oraclePx"])
        except (TypeError, ValueError, KeyError):
            continue
        if oi >= MIN_OI:
            mk.append((oi, m.get("name"), int(m.get("maxLeverage") or 1)))
    mk.sort(reverse=True)
    print("  %d markets above $%.0fm OI\n" % (len(mk), MIN_OI / 1e6))

    now = int(time.time() * 1000)
    start = now - DAYS * 86400 * 1000
    res = []

    for oi, name, lev in mk:
        f = funding_series(name, start)
        p = price_series(name, start, now)
        hrs = sorted(set(f) & set(p))
        if len(hrs) < 300:
            print("  %-14s insufficient overlap (%d hrs)" % (name[:14], len(hrs)))
            continue

        mean_f = statistics.mean(f[h] for h in hrs)
        side = 1 if mean_f < 0 else -1        # take the side that RECEIVES

        eq, peak, mdd = 0.0, 0.0, 0.0
        fund_cum = 0.0
        p0 = p[hrs[0]]
        for i, h in enumerate(hrs):
            # funding accrues at the rate observed at h, applied over that hour
            fund_cum += -side * f[h]
            px = (p[h] - p0) / p0 * side
            eq = fund_cum + px
            peak = max(peak, eq)
            mdd = max(mdd, peak - eq)

        span = len(hrs) / 24.0
        ann = 365.0 / span
        fund_ann = fund_cum * 100.0 * ann
        px_ann = ((p[hrs[-1]] - p0) / p0 * side) * 100.0 * ann
        tot_ann = eq * 100.0 * ann
        safe_lev = (1.0 / mdd) if mdd > 0 else 99.0
        res.append((tot_ann, name, side, fund_ann, px_ann, mdd * 100, safe_lev,
                    oi, lev))

    if not res:
        print("no data")
        return

    res.sort(reverse=True)
    print("=" * 80)
    print("NAKED HOLD OF THE FUNDING-RECEIVING SIDE, %d DAYS, ANNUALISED" % DAYS)
    print("=" * 80)
    print("%-14s %5s %9s %9s %9s %8s %8s"
          % ("market", "side", "funding", "price", "TOTAL", "maxDD", "safe lev"))
    for tot, name, side, fa, pa, mdd, sl, oi, lev in res:
        print("%-14s %5s %+8.0f%% %+8.0f%% %+8.0f%% %7.1f%% %7.1fx"
              % (name[:14], "LONG" if side > 0 else "SHORT", fa, pa, tot,
                 mdd, min(sl, 99)))

    tots = [r[0] for r in res]
    wins = sum(1 for t in tots if t > 0)
    print("\n" + "=" * 80)
    print("SUMMARY")
    print("=" * 80)
    print("  %d markets   profitable %d (%.0f%%)   median total %+.0f%%/yr"
          % (len(res), wins, 100.0 * wins / len(res), statistics.median(tots)))
    print("  mean total %+.0f%%/yr   worst %+.0f%%   best %+.0f%%"
          % (statistics.mean(tots), min(tots), max(tots)))

    dds = [r[5] for r in res]
    print("  median max drawdown %.1f%%   worst %.1f%%"
          % (statistics.median(dds), max(dds)))

    surv = [r for r in res if r[0] > 0 and r[6] >= 3.0]
    print("\n  markets that BOTH made money AND survived 3x leverage: %d"
          % len(surv))
    for tot, name, side, fa, pa, mdd, sl, oi, lev in surv[:12]:
        print("    %-14s %5s  total %+7.0f%%  maxDD %5.1f%%  OI $%.0fm"
              % (name, "LONG" if side > 0 else "SHORT", tot, mdd, oi / 1e6))

    print("\n" + "=" * 80)
    print("HOW TO READ THIS HONESTLY")
    print("=" * 80)
    print("""  45 days is SEVEN weeks. The reverse book read +23.9% at four closes
  and +4.6% at seven. A single window of this length settles nothing on
  its own -- but if most markets are positive with survivable drawdowns,
  that is real evidence against my position and worth acting on.

  Look at funding vs price separately. If funding is large and positive
  while price is large and negative, the market is paying you exactly
  what it is taking, which is the efficient-market answer. If funding
  outruns price, there is something here.

  And weigh maxDD above total. A market showing +200%/yr with a 40%
  drawdown is a 2.5x-max position, and one bad week ends it.""")


if __name__ == "__main__":
    main()
