#!/usr/bin/env python3
"""What the closed positions actually say -- and what they invalidate.

TWO QUESTIONS, BOTH NOW ANSWERABLE FROM REAL CLOSES

1. EXIT COSTS. Every profit-and-loss figure this project has produced assumes
   the exit costs the same as the entry. That was never tested, because nothing
   had closed. It has now, and the first four closes came in at +22%, +21%, +7%
   and -1% against their estimates. If that holds, every net number quoted
   anywhere -- including the five-year majors backtest -- is optimistic.

2. CARRY RATIO. The hypothesis was that reverse positions win when funding
   comfortably exceeds borrow and lose when it does not, and that a minimum
   ratio should gate entry. That was n=2 and a hunch. With real closes it can
   be tested rather than believed.

Both matter more than they sound. The first is a systematic bias in every
figure. The second decides whether to add an entry filter that would have
refused positions the book actually took.

Reads only. Run from ~/vega-bot:  python3 closed_analysis.py
"""

import json
import os
import statistics
import sys
from datetime import datetime, timezone


def load(path):
    if not os.path.exists(path):
        return [], []
    d = json.load(open(path))
    o = d.get("open") or {}
    c = d.get("closed") or []
    o = list(o.values()) if isinstance(o, dict) else list(o)
    c = list(c.values()) if isinstance(c, dict) else list(c)
    return o, c


def hours(a, b):
    try:
        ta = datetime.fromisoformat(a.replace("Z", "+00:00"))
        tb = datetime.fromisoformat(b.replace("Z", "+00:00"))
        return (tb - ta).total_seconds() / 3600.0
    except Exception:
        return float("nan")


def entry_bps_hr(p):
    iv = p.get("interval_hours") or 0
    r = p.get("entry_rate_pct")
    if not iv or r is None:
        return float("nan")
    return r * 100.0 / iv


def carry_ratio(p):
    """|funding at entry| / borrow, both in bps/hr. Above 1 means the funding
    covers the borrow; the question is how much headroom is needed."""
    b = p.get("borrow_bps_hr") or 0.0
    f = abs(entry_bps_hr(p))
    if b <= 0:
        return float("inf")      # no borrow: cash-and-carry
    return f / b


def main():
    o, c = load("data/reverse/positions.json")
    if not c:
        sys.exit("no closed positions yet")

    print("REVERSE BOOK: %d closed, %d open\n" % (len(c), len(o)))

    # ---------- 1. exit cost vs estimate ----------
    print("EXIT COST vs THE SYMMETRIC ESTIMATE")
    print("%-22s %10s %10s %9s  %s" %
          ("position", "entry bps", "exit bps", "vs est", "reason"))
    ratios = []
    for p in sorted(c, key=lambda x: x.get("closed_at", "")):
        e = p.get("entry_cost_bps") or 0.0
        x = p.get("exit_cost_bps") or 0.0
        if e <= 0:
            continue
        r = x / e
        ratios.append(r)
        print("%-22s %10.2f %10.2f %8.0f%%  %s"
              % (p.get("id", "?"), e, x, (r - 1) * 100,
                 (p.get("close_reason") or "?")[:34]))

    if ratios:
        mean_r = statistics.mean(ratios)
        med_r = statistics.median(ratios)
        print("\n  mean %.3fx   median %.3fx   worst %.3fx"
              % (mean_r, med_r, max(ratios)))
        print("  => exits cost %.0f%% MORE than the estimate on average"
              % ((mean_r - 1) * 100))
        print("\n  IMPACT ON EVERY FIGURE PUBLISHED SO FAR")
        # A round trip is entry + exit. If exit is inflated by k, the round
        # trip rises by k/2 as a fraction.
        for label, rt in (("reverse book (45 bps)", 45.0),
                          ("majors (33 bps)", 33.0),
                          ("cross-venue (25 bps)", 25.0)):
            extra = rt / 2.0 * (mean_r - 1.0)
            print("    %-24s round trip %.1f -> %.1f bps  (+%.1f)"
                  % (label, rt, rt + extra, extra))
        print("\n  On the majors five-year test that is roughly %.2f%%/yr of"
              % (33.0 / 2.0 * (mean_r - 1.0) * 2 / 100.0))
        print("  additional cost at two rolls a year -- small, but it moves")
        print("  every number in the wrong direction and should be applied.")

    # ---------- 2. carry ratio vs outcome ----------
    print("\n\nCARRY RATIO AT ENTRY vs REALISED OUTCOME")
    print("(ratio = |funding| / borrow at entry, both bps/hr. inf = no borrow)")
    print("%-22s %8s %10s %10s %10s %9s" %
          ("position", "side", "ratio", "held h", "net bps", "verdict"))
    rows = []
    for p in c:
        e = p.get("entry_cost_bps") or 0.0
        x = p.get("exit_cost_bps") or 0.0
        net = (p.get("funding_collected_bps") or 0.0) - e - x
        cr = carry_ratio(p)
        h = hours(p.get("opened_at", ""), p.get("closed_at", ""))
        rows.append((cr, net, p, h))
    for cr, net, p, h in sorted(rows, reverse=True):
        print("%-22s %8d %10s %10.1f %+10.2f %9s"
              % (p.get("id", "?"), p.get("side", 1),
                 "inf" if cr == float("inf") else "%.2f" % cr,
                 h, net, "WON" if net >= 0 else "lost"))

    fin = [(cr, net) for cr, net, _, _ in rows if cr != float("inf")]
    if len(fin) >= 2:
        won = [cr for cr, net in fin if net >= 0]
        lost = [cr for cr, net in fin if net < 0]
        print("\n  winners' ratios: %s" % (", ".join("%.2f" % x for x in sorted(won)) or "none"))
        print("  losers'  ratios: %s" % (", ".join("%.2f" % x for x in sorted(lost)) or "none"))
        if won and lost:
            lo_w, hi_l = min(won), max(lost)
            if lo_w > hi_l:
                print("  CLEAN SEPARATION at ratio %.2f -- every winner above,"
                      " every loser below." % ((lo_w + hi_l) / 2))
                print("  With n=%d this is suggestive, NOT established. A single"
                      % len(fin))
                print("  counterexample would break it, and four closes cannot")
                print("  distinguish a rule from a coincidence.")
            else:
                print("  NO clean separation: winners and losers overlap.")
                print("  The carry-ratio filter is not supported by these closes.")

    # ---------- 3. what the closes actually earned ----------
    print("\n\nREALISED PERFORMANCE, closed positions only")
    nets = [(p.get("funding_collected_bps") or 0.0)
            - (p.get("entry_cost_bps") or 0.0)
            - (p.get("exit_cost_bps") or 0.0) for p in c]
    usd = sum(n / 10000.0 * (p.get("notional_usd") or 0.0)
              for n, p in zip(nets, c))
    cap = sum(p.get("capital_usd") or 0.0 for p in c)
    hrs = [hours(p.get("opened_at", ""), p.get("closed_at", "")) for p in c]
    mean_h = statistics.mean([h for h in hrs if h == h]) if hrs else 0
    print("  %d closed, %d won / %d lost" %
          (len(c), sum(1 for n in nets if n >= 0), sum(1 for n in nets if n < 0)))
    print("  total %+.2f bps, mean %+.2f bps, median %+.2f bps"
          % (sum(nets), statistics.mean(nets), statistics.median(nets)))
    print("  $%+.3f on $%.0f of capital over a mean hold of %.1fh"
          % (usd, cap, mean_h))
    if cap > 0 and mean_h > 0:
        per_yr = usd / cap * (8760.0 / mean_h) * 100.0
        print("  annualised on capital while deployed: %+.1f%%" % per_yr)
        print("  (backtest said ~23%%; capacity, not return, is the binding limit)")

    print("\nNOTE: four closes. Every conclusion here is provisional and should")
    print("be re-run as the book accumulates. The exit-cost finding is the one")
    print("worth acting on now, because it biases figures already published.")


if __name__ == "__main__":
    main()
