#!/usr/bin/env python3
"""Does the deep, high-capacity end pay 3.5% always -- or 3.5% now and 25% sometimes?

THE QUESTION THIS DECIDES

Measured over 20 days: the $2000+ depth band holds $6.5M -- enough for 20 crore
several times over -- and pays about 3.5% unlevered, 6.25% with portfolio
margin. Against a fund promising 1.5% monthly, that is a third of what is
needed.

But the median coin in EVERY depth band paid exactly 0.125 bps/hr, Binance's
neutral baseline. A market-wide median sitting precisely on neutral is the
signature of an unusually quiet funding regime, and 20 days is no basis for a
conclusion about a market that has regimes.

So: pull two years of actual settled funding from Binance and measure the
distribution instead of arguing about it. If deep-coin funding is 3.5% in every
month of two years, the ceiling is structural and the plan has to change. If it
runs 3.5% in quiet months and 25% in active ones, the strategy is
regime-dependent -- a completely different conclusion with a completely
different plan.

WHY BOTH DIRECTIONS COUNT ON MAJORS

On thin coins, negative funding is unharvestable: nobody lends the coin, or
lends it at 300%/yr. On majors that constraint vanishes -- BTC borrows at
0.4%/yr (0.005 bps/hr), ETH at 1.0%. So a major can be harvested whichever way
funding points, and the quantity that matters is |funding| minus a borrow cost
near zero. This roughly doubles the harvestable amount versus a positive-only
book, and it is a genuine advantage of the deep end that the thin end does not
have.

WHAT IS SETTLED FACT HERE AND WHAT IS NOT

  fact         the funding rates -- these are settled, paid, historical
  fact         the borrow rates, from today's snapshot
  assumption   costs. A 33 bps round trip is our MEASURED median for this
               band, held constant across two years. Fees have fallen over
               that period, so this is conservative.
  assumption   that a position can be held continuously through a month,
               paying the round trip roughly twice a year rather than
               per-settlement. Stated so it can be argued with.

NOT MODELLED: liquidation risk, exchange failure, the basis moving against an
unhedged leg, or funding-rate caps during extreme events. This measures the
carry, not the risk of collecting it.

Run from ~/vega-bot:  python3 funding_history.py
"""

import collections
import json
import statistics
import sys
import time
import urllib.error
import urllib.request

API = "https://fapi.binance.com/fapi/v1/fundingRate"
SYMBOLS = ["BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT", "DOGEUSDT"]
YEARS = 2
COST_ROUNDTRIP_BPS = 33.0     # measured median for the $2000+ band
ROLLS_PER_YEAR = 2.0          # how often a continuous position is rebuilt
UNLEVERED = 2.0
PM_FACTOR = 1.11              # MODELLED, not measured

# Cheap borrow on majors, annual percent. Only used to charge the negative
# side; a coin absent here is not harvested in that direction.
BORROW_ANNUAL = {"BTC": 0.4, "ETH": 1.0, "SOL": 3.0, "BNB": 3.7,
                 "XRP": 3.0, "DOGE": 3.6}

TARGET_YEARLY = 18.0          # 1.5% monthly, the bar the fund has to clear


def fetch(symbol, start_ms, end_ms):
    out = []
    cur = start_ms
    while cur < end_ms:
        url = "%s?symbol=%s&startTime=%d&limit=1000" % (API, symbol, cur)
        req = urllib.request.Request(url, headers={"User-Agent": "vega/1.0"})
        try:
            with urllib.request.urlopen(req, timeout=15) as r:
                rows = json.loads(r.read().decode())
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
            print("  %s: fetch failed (%s) -- partial history" % (symbol, e))
            break
        if not rows:
            break
        out += rows
        last = int(rows[-1]["fundingTime"])
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.25)
        if len(rows) < 1000:
            break
    return out


def base(s):
    return s[:-4] if s.endswith("USDT") else s


def main():
    now = int(time.time() * 1000)
    start = now - YEARS * 365 * 86400 * 1000

    # month -> symbol -> list of bps/hr (signed)
    months = collections.defaultdict(lambda: collections.defaultdict(list))
    print("pulling %d years of settled funding for %d symbols\n" % (YEARS, len(SYMBOLS)))

    for s in SYMBOLS:
        rows = fetch(s, start, now)
        if not rows:
            print("  %-10s no data" % s)
            continue
        for r in rows:
            try:
                rate = float(r["fundingRate"])
                t = int(r["fundingTime"])
            except (KeyError, ValueError):
                continue
            # Binance majors settle every 8h. rate is a fraction per interval.
            bps_hr = rate * 10000.0 / 8.0
            ym = time.strftime("%Y-%m", time.gmtime(t / 1000))
            months[ym][s].append(bps_hr)
        vals = [abs(float(r["fundingRate"])) * 10000.0 / 8.0 for r in rows]
        print("  %-10s %5d settlements, median |f| %.4f bps/hr"
              % (s, len(rows), statistics.median(vals)))

    if not months:
        sys.exit("no funding history retrieved")

    def ret_pct(bps_hr_abs):
        """Annualised return on unlevered capital, costs amortised."""
        gross = bps_hr_abs * 8760.0
        cost = COST_ROUNDTRIP_BPS * ROLLS_PER_YEAR
        return (gross - cost) / 10000.0 / UNLEVERED * 100.0

    print("\n%-9s %10s %10s %10s %10s %9s" %
          ("month", "mean|f|/hr", "%neg", "ret/yr", "with PM", "clears 18%?"))

    rows_out = []
    for ym in sorted(months):
        allv = [v for s in months[ym] for v in months[ym][s]]
        if len(allv) < 30:
            continue
        absv = [abs(v) for v in allv]
        mean_abs = statistics.mean(absv)
        neg = 100.0 * sum(1 for v in allv if v < 0) / len(allv)

        # Charge borrow on the negative share only; majors borrow near zero.
        bmean = statistics.mean(
            [BORROW_ANNUAL.get(base(s), 5.0) * 100.0 / 8760.0 for s in months[ym]])
        eff = mean_abs - bmean * (neg / 100.0)

        r1 = ret_pct(eff)
        r2 = r1 * (UNLEVERED / PM_FACTOR)
        rows_out.append((ym, mean_abs, neg, r1, r2))
        print("%-9s %10.4f %9.1f%% %9.2f%% %9.2f%% %9s"
              % (ym, mean_abs, neg, r1, r2, "YES" if r2 >= TARGET_YEARLY else ""))

    if not rows_out:
        sys.exit("not enough monthly data")

    pm = [r[4] for r in rows_out]
    hits = sum(1 for x in pm if x >= TARGET_YEARLY)
    print("\n%d months measured" % len(rows_out))
    print("  median return with PM:   %.2f%%/yr" % statistics.median(pm))
    print("  best month:              %.2f%%/yr (%s)"
          % (max(pm), rows_out[pm.index(max(pm))][0]))
    print("  worst month:             %.2f%%/yr (%s)"
          % (min(pm), rows_out[pm.index(min(pm))][0]))
    print("  months clearing %.0f%%/yr: %d of %d  (%.0f%%)"
          % (TARGET_YEARLY, hits, len(pm), 100.0 * hits / len(pm)))

    print("\nWHAT THIS DECIDES")
    print("  If few or no months clear 18%/yr, the deep end cannot fund a")
    print("  1.5%/month promise even at $6.5M capacity, and the ceiling is")
    print("  structural rather than a limitation of what we have built.")
    print("  If many months clear it, the strategy is regime-dependent and the")
    print("  question becomes how to recognise the regime -- a real plan.")


if __name__ == "__main__":
    main()
