#!/usr/bin/env python3
"""Make the concurrency cap a flag, because it is now the binding constraint.

MaxConcurrent was 5, chosen when candidates were scarce. This morning the
borrow universe went from 25 lendable coins to 132 and the reverse book was
already full within minutes -- so the funnel got wider and nothing could pour
through it. Five slots against a 2-day planned hold caps throughput at 2.5
positions per day; the backtest that justified this strategy produced about 11.

$500 of a $1,600 book is deployed and $1,100 sits idle, not because capital is
short but because a number set for a different situation was never revisited.

Default stays 5, so the headline book is unaffected and the comparison between
the two books survives. Only the reverse unit passes the flag.

WHAT THIS DOES NOT FIX: twelve positions may be twelve copies of one trade. If
negative funding is a market-wide regime rather than a per-coin quirk, they
turn together and we learn one thing twelve times while believing we learned
twelve. Worth checking for venue and sector clustering once a dozen exist.

Run from ~/vega-bot.
"""

import os
import shutil
import sys

MON = "cmd/monitor/main.go"

FLAG = (
    '\tmaxConc := flag.Int("max-concurrent", 5,\n'
    '\t\t"how many positions the book may hold at once. The capital ledger is "+\n'
    '\t\t\t"still the authority: this cannot spend money the book does not have")\n'
)

ANCHOR = "\tpaperCfg.PlannedHoldDays = *holdLong\n"
ADD = "\tpaperCfg.MaxConcurrent = *maxConc\n"


def main():
    if not os.path.exists(MON):
        raise SystemExit("run this from ~/vega-bot")

    src = open(MON, encoding="utf-8").read()
    if "*maxConc" in src:
        print("already added -- nothing to do")
        return 0

    for name, a in (("flag.Parse()", "\tflag.Parse()\n"),
                    ("PlannedHoldDays assignment", ANCHOR)):
        if src.count(a) != 1:
            print("REFUSING -- %s found %d times, want 1" % (name, src.count(a)))
            return 1

    # The assignment must land BEFORE NewBook, or the book is built with the
    # old value and the log line reports a cap that is not being enforced --
    # which is worse than no flag at all.
    if src.index(ANCHOR) > src.index("funding.NewBook(abs, paperCfg)"):
        print("REFUSING -- PlannedHoldDays assignment no longer precedes NewBook")
        return 1

    src = src.replace("\tflag.Parse()\n", FLAG + "\tflag.Parse()\n", 1)
    src = src.replace(ANCHOR, ANCHOR + ADD, 1)

    shutil.copy2(MON, MON + ".premaxconc")
    open(MON, "w", encoding="utf-8").write(src)
    print("  ok  -max-concurrent flag added, default 5")
    print("  ok  applied to paperCfg before NewBook")
    print("\nInert until a unit passes it. Headline book unchanged.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
