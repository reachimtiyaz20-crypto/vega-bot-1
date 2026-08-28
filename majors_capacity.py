#!/usr/bin/env python3
"""What do DEEP coins pay, and how much can they hold?

WE HAVE BEEN MEASURING ONE END OF THE MARKET.

Every finding so far concerns thin altcoins with violent funding: HOME at -14
bps/hr, STORJ at -24, COTI at -109. They are capacity-bound by construction --
the crowded shorting that drives funding negative is the same pressure that
empties the book and stops anyone lending. Total measured capacity: ~$171.

The opposite end is unexamined. BTC, ETH, SOL and their peers have deep books,
cheap borrow, and boring funding. Nobody squeezes BTC's lending market. If the
boring end pays even 8-10% with portfolio margin, that is a business at size,
and it is the exact inverse of everything we have optimised for.

I expect the answer to disappoint -- majors sit near Binance's 0.125 bps/hr
baseline, which is ~1.1%/yr gross against a ~40 bps round trip, and 30 days of
that is 90 bps of funding for 40 bps of cost. But the last four things I
believed without measuring were all wrong, so this measures.

BUCKETS BY DEPTH, NOT BY NAME

"Major" is not a property we can query; depth is. Coins are bucketed by median
spot top-of-book, which is exactly the quantity that bounded capacity at $171.
A coin holding $5,000 at the touch is a different business from one holding $50
regardless of whether anyone calls it a major.

FOR EACH BUCKET

  funding      median and p90 of |bps/hr|, and how often it is negative
  cost         median measured round trip at $50
  net/30d      what a patient 30-day hold clears after cost
  capacity     median top-of-book, and the sum across the bucket
  ret/yr       on unlevered capital (2x notional) and with PM (1.11x)

The PM figure is MODELLED, not measured -- 1.11 is a blanket factor and Bybit
prices margin by risk tier. It is shown because it bounds the upside, and
labelled because it is not a fact yet.

Run from ~/vega-bot:  nice -n 10 python3 majors_capacity.py
"""

import collections
import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"
HOLD_DAYS = 30.0
PM_FACTOR = 1.11        # MODELLED. See note above.
UNLEVERED = 2.0

BUCKETS = [
    ("$2000+", 2000, float("inf")),
    ("$500-2000", 500, 2000),
    ("$100-500", 100, 500),
    ("$25-100", 25, 100),
    ("under $25", 0, 25),
]


def opener(p):
    return (gzip.open(p, "rt", encoding="utf-8", errors="replace")
            if p.endswith(".gz") else open(p, "rt", errors="replace"))


def pct(v, q):
    if not v:
        return float("nan")
    s = sorted(v)
    i = int(round(q * (len(s) - 1)))
    return s[max(0, min(i, len(s) - 1))]


def main():
    paths = (sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
             + sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl"))))
    if not paths:
        sys.exit("no journal files -- run from ~/vega-bot")
    print("reading %d journal files" % len(paths))

    # per (venue,symbol): depths, |funding| bps/hr, signed bps/hr, costs
    depth = collections.defaultdict(list)
    absf = collections.defaultdict(list)
    signed = collections.defaultdict(list)
    costs = collections.defaultdict(list)

    n = 0
    for p in paths:
        with opener(p) as f:
            for line in f:
                if '"obs"' not in line:
                    continue
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if r.get("type") != "obs":
                    continue
                iv, rate = r.get("interval_hours") or 0, r.get("funding_rate_pct")
                if not iv or rate is None:
                    continue
                if not r.get("spot_available"):
                    continue
                top = r.get("spot_top_usd")
                if top is None:
                    continue
                k = (r.get("venue"), r.get("symbol"))
                b = rate * 100.0 / iv
                depth[k].append(top)
                absf[k].append(abs(b))
                signed[k].append(b)
                if r.get("cost_bps"):
                    costs[k].append(r["cost_bps"])
                n += 1

    print("%d observations across %d venue-symbols\n" % (n, len(depth)))

    rows = collections.defaultdict(list)
    for k, ds in depth.items():
        md = statistics.median(ds)
        for name, lo, hi in BUCKETS:
            if lo <= md < hi:
                rows[name].append(k)
                break

    print("%-12s %6s %10s %9s %9s %10s %11s %9s %9s" %
          ("depth band", "pairs", "med depth", "med|f|/hr", "p90|f|/hr",
           "%neg", "med cost", "net/30d", "ret/yr"))

    for name, lo, hi in BUCKETS:
        ks = rows.get(name) or []
        if not ks:
            continue
        med_depth = statistics.median([statistics.median(depth[k]) for k in ks])
        allabs = [x for k in ks for x in absf[k]]
        allsig = [x for k in ks for x in signed[k]]
        allcost = [x for k in ks for x in costs[k]] or [40.0]
        mf = statistics.median(allabs)
        p9 = pct(allabs, 0.9)
        neg = 100.0 * sum(1 for x in allsig if x < 0) / max(len(allsig), 1)
        mc = statistics.median(allcost)

        # A patient hold, taking whichever side the funding favours: this is
        # the best case, assuming the sign persists for the whole 30 days,
        # which it will not. It is a CEILING on the boring end.
        gross = mf * 24.0 * HOLD_DAYS
        net30 = gross - mc
        ret_yr = (net30 / 10000.0) * (365.0 / HOLD_DAYS) / UNLEVERED * 100.0

        print("%-12s %6d %10.0f %9.3f %9.3f %9.1f%% %10.1f %11.1f %8.2f%%" %
              (name, len(ks), med_depth, mf, p9, neg, mc, net30, ret_yr))

    print("\nret/yr is on UNLEVERED capital (2x notional), assuming the funding")
    print("sign holds for the whole 30 days -- a ceiling, not a forecast.")
    print("With portfolio margin at the MODELLED 1.11x factor, multiply by %.2f."
          % (UNLEVERED / PM_FACTOR))

    print("\nCAPACITY BY BAND (sum of median top-of-book, binance+bybit only):")
    for name, lo, hi in BUCKETS:
        ks = rows.get(name) or []
        if not ks:
            continue
        tot = sum(statistics.median(depth[k]) for k in ks)
        print("  %-12s %4d pairs   $%.0f" % (name, len(ks), tot))

    print("\nThe question this answers: is there a band with BOTH a usable")
    print("return AND depth to hold real money? If every row with good capacity")
    print("has a poor return, the ceiling is structural and no amount of")
    print("building moves it.")


if __name__ == "__main__":
    main()
