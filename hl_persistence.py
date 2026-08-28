#!/usr/bin/env python3
"""Do Hyperliquid's rich funding rates persist, and can the spot leg be built?

WHAT PROMPTED THIS

Checking an aggregator's claims against Hyperliquid's own data killed the
aggregator -- AXS advertised at -1980%/yr is actually +11%, six of ten coins
are not listed at all -- but it also overturned something I had asserted seven
times: that extreme funding lives in empty markets.

On Hyperliquid it is inverted. Markets under $100k of open interest have a
MEDIAN funding of 0.0%; funding needs participants to exist at all. The rich
rates sit on markets with real size:

	XMR       +93.6%/yr    open interest $72.6M    24h volume $13.5M
	CASHCAT   +89.5%/yr    open interest $36.4M    24h volume $36.3M
	VVV       +82.0%/yr    open interest $30.8M    24h volume  $6.3M
	CHIP     +102.6%/yr    open interest  $6.9M    24h volume  $4.2M

A short perp collecting 93% against a spot long bought with cash -- no
borrowing, since you own the spot -- is roughly 47%/yr on capital IF THE RATE
HOLDS. That conditional is the whole question, and it is exactly the one our
own book got wrong: entries at -8 bps/hr decayed toward baseline within hours
while borrow kept ticking.

WHAT IS MEASURED

	1. History. Hourly funding since as far back as the API serves, so a
	   snapshot is not mistaken for a rate.
	2. Decay. Given funding is rich now, what is it 1, 6, 24, 72, 168 hours
	   later? Magnitude, not sign -- conflating those was the error that made
	   the two-day hold look sensible for months.
	3. What a delta-neutral short would ACTUALLY have earned holding for
	   various periods, versus what the snapshot advertised.

WHAT THIS DOES NOT COVER

	spot leg     Hyperliquid has spot for only some assets. XMR and CASHCAT
	             almost certainly need a CEX for the long side, which means
	             capital on two venues, no margin netting, and transfer time
	             between them. VVV is known to have Bybit spot -- our own
	             reverse book holds bybit-VVVUSDT.
	fees         Hyperliquid's taker fee and the CEX spot round trip are not
	             charged here. Add roughly 30-45 bps a round trip.
	capacity     open interest is the payment. A position comparable to the
	             smaller side moves the rate against itself.

Public endpoint, no keys.  Run:  python3 hl_persistence.py
"""

import json
import statistics
import sys
import time
import urllib.error
import urllib.request

API = "https://api.hyperliquid.xyz/info"
COINS = ["XMR", "CASHCAT", "VVV", "CHIP", "APEX", "IMX", "FARTCOIN", "JTO",
         "MON", "LIT", "ACE", "MINA"]
DAYS = 45
HORIZONS = [1, 6, 24, 72, 168]
RICH_PCT_YR = 40.0        # "rich" = above this, annualised


def post(payload):
    req = urllib.request.Request(
        API, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json",
                 "User-Agent": "vega-hl/1.0"})
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.loads(r.read().decode())


def history(coin, start_ms, end_ms):
    """Hourly funding, as annualised percent. Positive = SHORT receives."""
    out, cur = [], start_ms
    for _ in range(40):
        if cur >= end_ms:
            break
        try:
            rows = post({"type": "fundingHistory", "coin": coin,
                         "startTime": cur, "endTime": end_ms})
        except Exception:
            break
        if not rows:
            break
        for r in rows:
            try:
                out.append((int(r["time"]),
                            float(r["fundingRate"]) * 8760.0 * 100.0))
            except (KeyError, ValueError, TypeError):
                continue
        last = max(int(r["time"]) for r in rows)
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.15)
        if len(rows) < 500:
            break
    out.sort()
    return out


def main():
    now = int(time.time() * 1000)
    start = now - DAYS * 86400 * 1000
    print("pulling %d days of hourly funding for %d coins\n" % (DAYS, len(COINS)))

    series = {}
    for c in COINS:
        h = history(c, start, now)
        if len(h) < 100:
            print("  %-10s only %d points -- skipped" % (c, len(h)))
            continue
        series[c] = h
        v = [x for _, x in h]
        pos = 100.0 * sum(1 for x in v if x > 0) / len(v)
        print("  %-10s %5d hrs  median %7.1f%%  mean %7.1f%%  positive %3.0f%%"
              % (c, len(v), statistics.median(v), statistics.mean(v), pos))

    if not series:
        sys.exit("no usable history")

    # ---- decay: given rich now, what later? ----
    print("\n\nDECAY -- given funding is above %.0f%%/yr, what is it later?"
          % RICH_PCT_YR)
    print("(magnitude, not sign. conflating those is what made a 2-day hold")
    print(" look sensible on the CEX book for months)")
    print("%-10s %9s" % ("coin", "samples"), end="")
    for h in HORIZONS:
        print("%10s" % ("+%dh" % h), end="")
    print()

    for c, rows in series.items():
        idx = {t: i for i, (t, _) in enumerate(rows)}
        base_ts = [t for t, v in rows if v >= RICH_PCT_YR]
        if len(base_ts) < 10:
            print("%-10s %9s   (rarely rich)" % (c, len(base_ts)))
            continue
        print("%-10s %9d" % (c, len(base_ts)), end="")
        for h in HORIZONS:
            later = []
            for t in base_ts:
                tgt = t + h * 3600000
                # nearest sample within 90 minutes of the target
                best = None
                for tt, vv in rows:
                    if abs(tt - tgt) <= 90 * 60000:
                        best = vv
                        break
                    if tt > tgt + 90 * 60000:
                        break
                if best is not None:
                    later.append(best)
            if len(later) < 5:
                print("%10s" % "-", end="")
            else:
                print("%9.0f%%" % statistics.median(later), end="")
        print()

    # ---- what a hold would actually have earned ----
    print("\n\nREALISED, holding a short from any rich entry")
    print("annualised, funding only -- fees NOT charged (add ~30-45 bps a round")
    print("trip, which at these rates is small but not nothing)")
    print("%-10s %9s" % ("coin", "entries"), end="")
    for h in HORIZONS:
        print("%10s" % ("%dh" % h), end="")
    print()

    for c, rows in series.items():
        vals = [(t, v) for t, v in rows]
        base_ts = [t for t, v in vals if v >= RICH_PCT_YR]
        if len(base_ts) < 10:
            continue
        print("%-10s %9d" % (c, len(base_ts)), end="")
        for h in HORIZONS:
            earned = []
            for t in base_ts:
                end = t + h * 3600000
                seg = [v for tt, v in vals if t <= tt < end]
                if len(seg) >= max(1, h // 2):
                    earned.append(statistics.mean(seg))
            if len(earned) < 5:
                print("%10s" % "-", end="")
            else:
                print("%9.0f%%" % statistics.median(earned), end="")
        print()

    print("\nHOW TO READ IT")
    print("  The decay table is the answer. If a coin rich now is still rich")
    print("  in 24 hours, the snapshot means something and a short hold")
    print("  collects most of what it advertises. If it halves within six")
    print("  hours, the headline is a moment and not a rate -- which is")
    print("  exactly what our own CEX book found at -8 bps/hr.")
    print("\n  Then the spot leg decides feasibility. VVV has Bybit spot (our")
    print("  reverse book holds it). XMR and CASHCAT almost certainly need a")
    print("  CEX, meaning capital on two venues with no margin netting.")


if __name__ == "__main__":
    main()
