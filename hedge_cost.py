#!/usr/bin/env python3
"""What does it cost to hedge a deposit -- and on most assets, does it pay you?

THE PIVOT THIS SUPPORTS

Funding carry as a standalone business pays roughly nothing: median 3.8%/yr on
notional against 4.45%/yr to borrow dollars. That result stands and is why the
carry books are being retired as a primary strategy.

But the same 3.8% stops being a disappointing return and becomes a useful
SUBSIDY the moment the main income comes from somewhere else. Deposit an asset
into a protocol paying incentives, short the perp to remove the price risk, and
the hedge is not a cost centre -- on most assets, most of the time, the short
side COLLECTS funding.

	deposit asset A into a programme  ->  earn points
	short perp A                      ->  price risk removed
	funding on that short             ->  usually INCOME, not cost

That is the Ethena structure, and it is why their product yields anything at
all. The points are the return; the hedge pays for itself and then some.

THE SIGN, because it is the whole thing

A SHORT perp RECEIVES funding when the rate is POSITIVE and PAYS when negative.
Funding is positive most of the time -- our five-year sample has it positive on
about 74% of days -- so hedging a long deposit is usually paid for by the
market. The risk is not the average, it is the stretches where funding inverts
and the hedge starts bleeding while your points keep accruing at a fixed rate.

So this ranks candidate deposit assets by three things that matter and are
measurable today:

	what the hedge earns    median funding, annualised, as the short receives it
	how often it inverts    share of observations where the short PAYS
	whether you can hedge   perp depth at the touch, per venue

WHAT THIS DOES NOT COVER, and it is the part that can actually lose the money:
smart-contract risk, protocol failure, lockups that prevent unwinding the
deposit while the hedge stays live, and the token being worth nothing when it
finally unlocks. The hedge protects against price. It protects against nothing
else.

Run from ~/vega-bot:  python3 hedge_cost.py
"""

import collections
import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"
MIN_OBS = 200
BORROW_BENCH = 4.45      # %/yr, USDT lending, the do-nothing benchmark


def opener(p):
    return (gzip.open(p, "rt", encoding="utf-8", errors="replace")
            if p.endswith(".gz") else open(p, "rt", errors="replace"))


def main():
    paths = (sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
             + sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl"))))
    if not paths:
        sys.exit("no journal files -- run from ~/vega-bot")
    print("reading %d journal files" % len(paths))

    rates = collections.defaultdict(list)     # (venue,symbol) -> bps/hr
    depth = collections.defaultdict(list)     # perp top-of-book USD
    vol = collections.defaultdict(list)
    cost = collections.defaultdict(list)

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
                k = (r.get("venue"), r.get("symbol"))
                rates[k].append(rate * 100.0 / iv)
                if r.get("perp_top_usd") is not None:
                    depth[k].append(r["perp_top_usd"])
                if r.get("perp_vol_24h_usd") is not None:
                    vol[k].append(r["perp_vol_24h_usd"])
                if r.get("cost_bps"):
                    cost[k].append(r["cost_bps"])

    rows = []
    for k, vals in rates.items():
        if len(vals) < MIN_OBS:
            continue
        med = statistics.median(vals)
        # A SHORT receives positive funding. Annualise as income to the hedge.
        income = med * 8760.0 / 100.0
        pays = 100.0 * sum(1 for v in vals if v < 0) / len(vals)
        d = statistics.median(depth[k]) if depth.get(k) else 0.0
        v = statistics.median(vol[k]) if vol.get(k) else 0.0
        c = statistics.median(cost[k]) if cost.get(k) else float("nan")
        rows.append((income, k[0], k[1], pays, d, v, c, len(vals)))

    if not rows:
        sys.exit("not enough observations")

    print("%d venue-symbols with %d+ observations\n" % (len(rows), MIN_OBS))

    print("BEST ASSETS TO HEDGE -- the short collects the most, least often inverts")
    print("%-22s %11s %9s %12s %13s %8s"
          % ("venue/symbol", "hedge %/yr", "inverts", "perp touch", "perp vol/d", "cost"))
    for inc, ven, sym, pays, d, v, c, n in sorted(rows, reverse=True)[:25]:
        print("%-22s %10.1f%% %8.0f%% %11.0f %12.0fM %7.1f"
              % (ven + "/" + sym, inc, pays, d, v / 1e6, c))

    print("\nWORST -- hedging these costs you money")
    for inc, ven, sym, pays, d, v, c, n in sorted(rows)[:10]:
        print("%-22s %10.1f%% %8.0f%% %11.0f %12.0fM %7.1f"
              % (ven + "/" + sym, inc, pays, d, v / 1e6, c))

    # The majors are what most deposit programmes actually accept.
    print("\nMAJORS -- what deposit programmes usually take")
    want = {"BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT", "USDCUSDT"}
    for inc, ven, sym, pays, d, v, c, n in sorted(rows, reverse=True):
        if sym in want:
            print("%-22s %10.1f%% %8.0f%% %11.0f %12.0fM %7.1f"
                  % (ven + "/" + sym, inc, pays, d, v / 1e6, c))

    allinc = [r[0] for r in rows]
    print("\nACROSS EVERYTHING: median hedge income %.2f%%/yr, p25 %.2f%%, p75 %.2f%%"
          % (statistics.median(allinc),
             sorted(allinc)[len(allinc) // 4],
             sorted(allinc)[3 * len(allinc) // 4]))

    print("\nWHAT THIS MEANS FOR A FARMING DECISION")
    print("  A programme is worth entering when:")
    print("      points value  +  hedge income  -  round trip  >  %.2f%%/yr" % BORROW_BENCH)
    print("  Hedge income is measured above. The round trip is the cost column,")
    print("  paid once in and once out. Points value is the ONLY unknown, which")
    print("  is the right place for the uncertainty to sit -- it is a bet on a")
    print("  distribution, financed by a hedge that mostly pays for itself.")
    print("\n  The 'inverts' column is the risk that matters. An asset whose")
    print("  funding is negative 40%% of the time will have stretches where the")
    print("  hedge bleeds while points accrue at a fixed rate, and you cannot")
    print("  unwind a locked deposit to stop it.")


if __name__ == "__main__":
    main()
