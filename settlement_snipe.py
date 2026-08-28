#!/usr/bin/env python3
"""Is the funding payment at settlement worth more than the fees to capture it?

THE IDEA, WHICH IS NOT CARRY

Carry holds a hedged position for days and collects funding continuously. It
loses to two things we have measured repeatedly: DECAY -- CHIP went 127.6%/yr
to 61.3% in half an hour -- and BORROW, which ate the entire edge on the CEX
book.

Settlement sniping avoids both. Enter the perp short seconds before the funding
timestamp, collect the payment, exit seconds after. Nothing is held, so nothing
decays. No spot leg, so nothing is borrowed.

	funding on a rich alt   0.4-0.8% per settlement  =  40-80 bps
	two perp round trips    ~11 bps Bybit, ~9 bps Hyperliquid
	exposure                seconds

WHAT THIS MEASURES, AND WHAT IT CANNOT

MEASURABLE from our journal: how large the funding payment is per settlement,
how often it clears the fee hurdle, and on which coins. That is the first-order
question and it decides whether the idea is worth anything.

NOT MEASURABLE here: the price move during the seconds you are exposed. Our
journal polls every five minutes; sniping lives at the second scale. If price
moves 30 bps against an unhedged short in those seconds, it swamps a 40 bps
payment. That is why the practitioner's claim rests on execution rather than
signal -- and why his unanswered PnL question matters.

Also not measurable: whether YOUR order fills at settlement. Liquidity thins
exactly then, everyone else is doing the same thing, and the fill you model is
not the fill you get.

So a positive result here means "the gross payment exists". It does not mean
the strategy works. It means the strategy is not immediately dead, which is
more than five of the six leads before it managed.

Run from ~/vega-bot:  nice -n 10 python3 settlement_snipe.py
"""

import collections
import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"
NOTIONAL = 50.0
MIN_TOP_FRAC = 0.25
MIN_VOL = 250_000.0

# Two perp round trips: in and out, both legs are the same perp.
# Bybit perp taker measured at 5.50 bps -> 11.00 for a round trip.
# Hyperliquid published at 4.50 -> 9.00.
FEE_TIERS = [("hyperliquid 4.5 taker", 9.0),
             ("bybit 5.5 taker", 11.0),
             ("plus 10 bps slippage", 21.0),
             ("plus 25 bps slippage", 36.0)]


def opener(p):
    return (gzip.open(p, "rt", encoding="utf-8", errors="replace")
            if p.endswith(".gz") else open(p, "rt", errors="replace"))


def main():
    paths = (sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
             + sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl"))))
    if not paths:
        sys.exit("no journal files -- run from ~/vega-bot")
    print("reading %d journal files" % len(paths))

    need = NOTIONAL * MIN_TOP_FRAC
    # per (venue,symbol): list of |payment| in bps per settlement
    pay = collections.defaultdict(list)
    tradeable = {}
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
                iv = r.get("interval_hours") or 0
                rate = r.get("funding_rate_pct")
                if not iv or rate is None:
                    continue
                k = (r.get("venue"), r.get("symbol"))
                # THE PAYMENT ITSELF, not a rate. rate_pct is already per
                # settlement interval, so this is what changes hands each time.
                pay[k].append(abs(rate) * 100.0)
                ok = (r.get("spot_available") and r.get("liq_measured")
                      and (r.get("perp_top_usd") or 0) >= need
                      and (r.get("perp_vol_24h_usd") or 0) >= MIN_VOL)
                tradeable[k] = tradeable.get(k, False) or bool(ok)
                n += 1

    if not pay:
        sys.exit("no funding observations")
    print("%d observations across %d venue-symbols\n" % (n, len(pay)))

    allpay = [v for vs in pay.values() for v in vs]
    allpay.sort()

    def q(x):
        return allpay[min(int(x * (len(allpay) - 1)), len(allpay) - 1)]

    print("SIZE OF A SINGLE FUNDING PAYMENT, in bps of notional")
    print("  median %.2f   p75 %.2f   p90 %.2f   p99 %.2f   max %.2f"
          % (q(0.5), q(0.75), q(0.9), q(0.99), allpay[-1]))

    print("\nHOW OFTEN A PAYMENT CLEARS THE FEE HURDLE")
    print("%-26s %12s %14s" % ("cost of the round trip", "clears", "of all obs"))
    for label, fee in FEE_TIERS:
        c = sum(1 for v in allpay if v > fee)
        print("%-26s %11.2f%% %13d" % (label, 100.0 * c / len(allpay), c))

    # Only count symbols we could actually trade at $50.
    trad = [v for k, vs in pay.items() if tradeable.get(k) for v in vs]
    if trad:
        trad.sort()
        print("\nSAME, BUT ONLY ON SYMBOLS THAT PASS OUR LIQUIDITY GATES")
        print("  %d observations on %d tradeable venue-symbols"
              % (len(trad), sum(1 for k in pay if tradeable.get(k))))
        for label, fee in FEE_TIERS:
            c = sum(1 for v in trad if v > fee)
            print("  %-26s %6.2f%% clear  (%d)"
                  % (label, 100.0 * c / len(trad), c))

    # Per symbol: how many rich settlements, and how rich.
    print("\nBEST SYMBOLS -- settlements above 11 bps (one Bybit round trip)")
    rows = []
    for k, vs in pay.items():
        rich = [v for v in vs if v > 11.0]
        if len(rich) < 5:
            continue
        rows.append((len(rich), statistics.median(rich), max(rich),
                     100.0 * len(rich) / len(vs), k, tradeable.get(k)))
    rows.sort(reverse=True)
    print("%-24s %8s %10s %10s %9s %s"
          % ("venue/symbol", "count", "median", "max", "share", "tradeable"))
    for cnt, med, mx, share, k, tr in rows[:20]:
        print("%-24s %8d %9.1f %9.1f %8.0f%% %s"
              % (k[0] + "/" + k[1], cnt, med, mx, share,
                 "yes" if tr else "NO"))

    # The economics, per event.
    print("\nWHAT ONE SNIPE EARNS, net of two round trips")
    print("(payment less fees, in bps on notional -- no price risk modelled)")
    print("%-26s %12s %12s %12s"
          % ("cost", "median net", "p90 net", "p99 net"))
    for label, fee in FEE_TIERS:
        rich = [v - fee for v in allpay if v > fee]
        if len(rich) < 20:
            continue
        rich.sort()
        print("%-26s %11.1f %11.1f %11.1f"
              % (label, statistics.median(rich),
                 rich[int(0.9 * (len(rich) - 1))],
                 rich[int(0.99 * (len(rich) - 1))]))

    print("\nHOW TO JUDGE IT")
    print("  If only a fraction of a percent of settlements clear 11 bps, the")
    print("  idea is dead regardless of how fast anyone executes -- there is")
    print("  nothing to be fast about. That is what the unanswered capacity")
    print("  question in that thread was pointing at.")
    print("\n  If a meaningful share clears, the gross payment exists and the")
    print("  question becomes execution: can you fill at settlement, when")
    print("  liquidity thins and everyone else is trying the same thing.")
    print("  This journal polls every five minutes and cannot see that.")
    print("\n  And note the 'tradeable' column. Six times now the richest")
    print("  funding has sat on symbols that fail our liquidity gates.")


if __name__ == "__main__":
    main()
