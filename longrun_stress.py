#!/usr/bin/env python3
"""Five years, including every crisis the two-year window politely omitted.

WHY TWO YEARS WAS NOT ENOUGH

Fixed 10x compounded to 36.1% CAGR with a 5.5% maximum drawdown over
2024-08 to 2026-08. That is the target return with mild risk -- and it is a
fair-weather reading, because the sample contains no crisis.

A static long-basis book is long spot and short perp. It COLLECTS funding when
funding is positive, and PAYS when negative. Funding goes deeply negative in
exactly one situation: price is crashing, longs are being liquidated, and
everyone wants to be short. So the book's worst moments coincide precisely with
the market's worst moments, and at 10x it pays ten times over.

Five years reaches back through:

  2021-05  the leverage flush
  2022-05  LUNA / UST
  2022-06  3AC and Celsius
  2022-11  FTX
  2023-03  USDC depeg

If 10x survives those with a drawdown you could sit through, the strategy is
real. If it does not, then 36% CAGR was a description of a calm market and the
honest maximum is far lower.

WHAT THIS STILL CANNOT SEE

Funding is only one of the two ways this trade loses. The other is BASIS
DISLOCATION -- spot and perp diverging far enough to liquidate the hedge -- and
measuring that needs paired spot and perp price history, not funding. During
FTX, perps traded several percent away from spot on some venues. At 10x,
capital is 10% of notional, so a 10% dislocation is total loss regardless of
what funding did.

So a good result here is necessary and NOT sufficient. It rules out one killer,
not both.

Run from ~/vega-bot:  python3 longrun_stress.py
"""

import collections
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request

API = "https://fapi.binance.com/fapi/v1/fundingRate"
SYMBOLS = ["BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT", "DOGEUSDT"]
YEARS = 5
CACHE = "data/history/funding_5y.json"
ROUNDTRIP_BPS = 33.0
LEVS = [1.0, 2.0, 3.0, 5.0, 8.0, 10.0]

CRISES = {
    "2021-05": "leverage flush",
    "2022-05": "LUNA / UST",
    "2022-06": "3AC / Celsius",
    "2022-11": "FTX",
    "2023-03": "USDC depeg",
}


def fetch(symbol, start_ms, end_ms):
    out, cur = [], start_ms
    for _ in range(400):
        if cur >= end_ms:
            break
        url = "%s?symbol=%s&startTime=%d&limit=1000" % (API, symbol, cur)
        req = urllib.request.Request(url, headers={"User-Agent": "vega/1.0"})
        try:
            with urllib.request.urlopen(req, timeout=20) as r:
                rows = json.loads(r.read().decode())
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError):
            break
        if not rows:
            break
        out += [(int(r["fundingTime"]), float(r["fundingRate"])) for r in rows]
        last = int(rows[-1]["fundingTime"])
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.2)
        if len(rows) < 1000:
            break
    return out


def load():
    if os.path.exists(CACHE):
        age_h = (time.time() - os.path.getmtime(CACHE)) / 3600
        if age_h < 168:
            print("using cached 5y history (%.0fh old)" % age_h)
            return json.load(open(CACHE))
    now = int(time.time() * 1000)
    start = now - YEARS * 365 * 86400 * 1000
    data = {}
    print("fetching %d years for %d symbols" % (YEARS, len(SYMBOLS)))
    for s in SYMBOLS:
        rows = fetch(s, start, now)
        data[s] = rows
        if rows:
            first = time.strftime("%Y-%m", time.gmtime(rows[0][0] / 1000))
            print("  %-10s %6d settlements from %s" % (s, len(rows), first))
        else:
            print("  %-10s none" % s)
    os.makedirs(os.path.dirname(CACHE), exist_ok=True)
    json.dump(data, open(CACHE, "w"))
    return data


def main():
    data = load()

    daily = collections.defaultdict(list)
    for sym, rows in data.items():
        for t, rate in rows:
            d = time.strftime("%Y-%m-%d", time.gmtime(t / 1000))
            daily[d].append(rate * 10000.0 / 8.0)
    days = sorted(daily)
    if len(days) < 500:
        sys.exit("not enough history")
    series = [statistics.mean(daily[d]) for d in days]

    print("\n%d days, %s to %s" % (len(days), days[0], days[-1]))
    print("median daily funding %.4f bps/hr" % statistics.median(series))
    neg = 100.0 * sum(1 for x in series if x < 0) / len(series)
    print("days with negative funding: %.1f%%\n" % neg)

    # monthly accrual per unit of leverage, cost charged once at entry
    month_bps = collections.defaultdict(float)
    for i, d in enumerate(days):
        month_bps[d[:7]] += series[i] * 24.0
    mkeys = sorted(month_bps)

    print("%-8s %9s %8s %10s %11s %9s %9s" %
          ("leverage", "final", "CAGR", "worst mo", "max dd", "mo>=18%", "mo>=35%"))
    curves = {}
    for lev in LEVS:
        equity, peak, maxdd = 1.0, 1.0, 0.0
        rets = []
        for j, k in enumerate(mkeys):
            bps = month_bps[k] * lev
            if j == 0:
                bps -= ROUNDTRIP_BPS * lev
            r = bps / 10000.0
            rets.append((k, r))
            equity *= (1.0 + r)
            if equity <= 0:
                equity = 0.0
                maxdd = 1.0
                break
            peak = max(peak, equity)
            maxdd = max(maxdd, (peak - equity) / peak)
        curves[lev] = rets
        years = len(days) / 365.0
        cagr = (equity ** (1.0 / years) - 1.0) * 100.0 if equity > 0 else -100.0
        ann = [r * 12 * 100 for _, r in rets]
        print("%7.0fx %8.2fx %7.1f%% %9.1f%% %10.1f%% %9d %9d" %
              (lev, equity, cagr, min(ann), maxdd * 100.0,
               sum(1 for x in ann if x >= 18.0),
               sum(1 for x in ann if x >= 35.0)))

    print("\nWORST TEN MONTHS at 10x (actual monthly loss, not annualised)")
    worst = sorted(curves[10.0], key=lambda kv: kv[1])[:10]
    for k, r in worst:
        tag = CRISES.get(k, "")
        print("  %-9s %7.2f%%   %s" % (k, r * 100.0, tag))

    print("\nCRISIS MONTHS at 10x")
    d10 = dict(curves[10.0])
    for k in sorted(CRISES):
        if k in d10:
            print("  %-9s %7.2f%%   %s" % (k, d10[k] * 100.0, CRISES[k]))
        else:
            print("  %-9s      --   %s (outside the window)" % (k, CRISES[k]))

    print("\nmax dd is the number that decides this. A drawdown you cannot sit")
    print("through is a drawdown you exit at the bottom, and the CAGR above")
    print("assumes you never do.")
    print("\nBASIS DISLOCATION IS STILL UNMEASURED. Funding is one of the two")
    print("ways this loses; the other needs paired spot and perp prices. A good")
    print("result here rules out one killer, not both.")


if __name__ == "__main__":
    main()
