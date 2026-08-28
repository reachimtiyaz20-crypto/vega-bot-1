#!/usr/bin/env python3
"""What does the majors trade pay at VIP fees and with leverage?

THE GAP THIS FILLS

funding_history.py concluded that 0 of 25 months cleared 18%/yr and that the
deep end therefore cannot fund a 1.5%/month promise. That analysis held
LEVERAGE AT 1x and fees at retail, and said so only in passing. Both are
choices, not facts, and Mohammed was right to challenge them.

A delta-neutral basis position earns funding on NOTIONAL. Double the notional
against the same capital and the return on capital roughly doubles. Real basis
desks run 3-5x. Concluding "the market does not pay 18%" while holding leverage
at 1x was measuring one corner of the space and reporting it as the whole.

WHAT LEVERAGE ACTUALLY COSTS -- and this is NOT modelled below

  liquidation      a hedged book is delta-neutral in PRICE but not in BASIS.
                   Spot and perp can diverge, and at 5x a sustained basis
                   dislocation wipes the position. Basis blowouts are what
                   killed levered basis funds in 2022.
  margin silos     spot and perp margin usually live in separate pools. A
                   perp leg can liquidate while the spot leg sits fine and
                   solvent, so unified or portfolio margin is a PRECONDITION
                   for leverage, not an enhancement.
  funding flips    at 5x, a month of adverse funding costs 5x as much too.
                   The grid below harvests |funding| by switching sides, which
                   assumes you can switch instantly and for free. You cannot.
  exchange risk    5x means 5x the exposure to one venue failing.

So read the grid as WHAT THE CARRY PAYS, not what you would net. Every number
here ignores the tail that decides whether a levered book survives to collect.

FEES ARE A GRID, NOT AN ASSUMPTION

Rather than guess your VIP tier, this sweeps round-trip cost from 33 bps
(retail, our measured median) down to 8 bps (deep VIP / market maker). Find the
row matching fees you can actually verify on your own account.

Caches the funding history so repeated runs cost nothing.

Run from ~/vega-bot:  python3 fee_leverage_grid.py
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
YEARS = 2
CACHE = "data/history/funding_2y.json"

# Round-trip cost in bps. 33 is OUR MEASURED median for the $2000+ band.
# The rest are indicative VIP tiers and MUST be checked against the real
# account before anyone relies on them.
FEE_TIERS = [("retail (measured)", 33.0), ("VIP 1", 26.0), ("VIP 3", 20.0),
             ("VIP 5", 14.0), ("VIP 9 / MM", 8.0)]

# Notional per dollar of capital. 0.5 is unlevered (2:1). 0.9 is the modelled
# portfolio-margin factor. Above that is genuine leverage.
LEVERAGE = [("unlevered", 0.5), ("PM 1.11x", 0.9), ("2x", 2.0),
            ("3x", 3.0), ("5x", 5.0), ("8x", 8.0)]

ROLLS_PER_YEAR = 2.0
TARGET = 18.0
BORROW_ANNUAL = {"BTC": 0.4, "ETH": 1.0, "SOL": 3.0, "BNB": 3.7,
                 "XRP": 3.0, "DOGE": 3.6}


def base(s):
    return s[:-4] if s.endswith("USDT") else s


def fetch(symbol, start_ms, end_ms):
    out, cur = [], start_ms
    while cur < end_ms:
        url = "%s?symbol=%s&startTime=%d&limit=1000" % (API, symbol, cur)
        req = urllib.request.Request(url, headers={"User-Agent": "vega/1.0"})
        try:
            with urllib.request.urlopen(req, timeout=15) as r:
                rows = json.loads(r.read().decode())
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError):
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


def load():
    if os.path.exists(CACHE):
        age_h = (time.time() - os.path.getmtime(CACHE)) / 3600
        if age_h < 24:
            print("using cached funding history (%.1fh old)" % age_h)
            return json.load(open(CACHE))
    now = int(time.time() * 1000)
    start = now - YEARS * 365 * 86400 * 1000
    data = {}
    print("fetching %d years for %d symbols" % (YEARS, len(SYMBOLS)))
    for s in SYMBOLS:
        rows = fetch(s, start, now)
        data[s] = [(int(r["fundingTime"]), float(r["fundingRate"])) for r in rows]
        print("  %-10s %d settlements" % (s, len(rows)))
    os.makedirs(os.path.dirname(CACHE), exist_ok=True)
    json.dump(data, open(CACHE, "w"))
    return data


def main():
    data = load()

    # month -> list of (signed bps/hr, borrow bps/hr for that symbol)
    months = collections.defaultdict(list)
    for s, rows in data.items():
        b_hr = BORROW_ANNUAL.get(base(s), 5.0) * 100.0 / 8760.0
        for t, rate in rows:
            ym = time.strftime("%Y-%m", time.gmtime(t / 1000))
            months[ym].append((rate * 10000.0 / 8.0, b_hr))

    # Effective harvestable bps/hr per month: |funding| less borrow on the
    # negative side only. Majors borrow near zero, which is the whole reason
    # both directions are harvestable here and not on thin coins.
    eff = {}
    for ym, vals in months.items():
        if len(vals) < 30:
            continue
        eff[ym] = statistics.mean(
            [abs(f) - (b if f < 0 else 0.0) for f, b in vals])
    if not eff:
        sys.exit("not enough monthly data")

    keys = sorted(eff)
    print("\n%d months, %s to %s" % (len(keys), keys[0], keys[-1]))
    print("median harvestable funding: %.4f bps/hr\n"
          % statistics.median([eff[k] for k in keys]))

    print("MEDIAN ANNUALISED RETURN ON CAPITAL")
    hdr = "%-18s" % "round trip"
    for name, _ in LEVERAGE:
        hdr += "%11s" % name
    print(hdr)

    best = {}
    for fname, fee in FEE_TIERS:
        row = "%-18s" % ("%s %.0f bps" % (fname, fee))
        for lname, lev in LEVERAGE:
            rets = []
            for k in keys:
                gross = eff[k] * 8760.0 * lev          # bps/yr on capital
                cost = fee * ROLLS_PER_YEAR * lev      # costs scale with notional
                rets.append((gross - cost) / 100.0)    # -> percent
            m = statistics.median(rets)
            row += "%10.1f%%" % m
            best[(fname, lname)] = rets
        print(row)

    print("\nMONTHS OUT OF %d CLEARING %.0f%%/yr" % (len(keys), TARGET))
    hdr = "%-18s" % "round trip"
    for name, _ in LEVERAGE:
        hdr += "%11s" % name
    print(hdr)
    for fname, fee in FEE_TIERS:
        row = "%-18s" % ("%s %.0f bps" % (fname, fee))
        for lname, lev in LEVERAGE:
            rets = best[(fname, lname)]
            row += "%10d " % sum(1 for x in rets if x >= TARGET)
        print(row)

    print("\nWORST MONTH, at each leverage (retail fees)")
    for lname, lev in LEVERAGE:
        rets = best[("retail (measured)", lname)]
        i = rets.index(min(rets))
        print("  %-10s %7.1f%%/yr  (%s)" % (lname, min(rets), keys[i]))

    print("\nREAD THIS BEFORE BELIEVING ANY NUMBER ABOVE")
    print("  Costs scale with notional, so leverage does NOT improve the")
    print("  cost-to-gross ratio -- it multiplies both. What it multiplies is")
    print("  the return on YOUR capital, and equally the loss when funding")
    print("  turns against the book.")
    print("  Liquidation, basis dislocation, margin silos and venue failure")
    print("  are NOT modelled. At 5x those are the terms that decide the")
    print("  outcome, and this grid is silent on all of them.")


if __name__ == "__main__":
    main()
