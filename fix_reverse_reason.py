#!/usr/bin/env python3
"""Record the edge we actually decided on, not the one before borrow.

THE DECISION WAS RIGHT. THE RECORD OF IT WAS NOT.

exchange.Assess builds its Reason text before borrow exists as a concept, and
the reverse branch then subtracts borrow from NetBps but leaves the text alone.
So the journal says one thing and the arithmetic says another:

  binance/ONTUSDT   recorded "net +85.6 bps"   true net after borrow  +21.0
  binance/SANDUSDT  recorded "net +155.7 bps"  true net after borrow +150.1

On ONT that is a 4x overstatement, and it is written into the permanent record
of a project whose entire purpose is a truthful one. Four previous bots looked
profitable on paper. This is precisely the mechanism by which that happens: not
a wrong decision, but a decision recorded more favourably than it was made.

The fix keeps the original text, demoted and labelled, so nothing is lost -- a
record that quietly replaces one number with another is its own kind of lie.

No behaviour change. NetBps was already correct; only the prose was wrong.

Run from ~/vega-bot.
"""

import os
import shutil
import sys

SRC = "pkg/funding/paper.go"

OLD = 'ra.Reason = "REVERSE (short spot): " + ra.Reason'

NEW = (
    'ra.Reason = fmt.Sprintf(\n'
    '\t\t\t\t\t\t\t\t"REVERSE (short spot): net %+.1f bps over %gd hold "+\n'
    '\t\t\t\t\t\t\t\t\t"AFTER %.1f bps borrow at %.3f bps/hr | pre-borrow read: %s",\n'
    '\t\t\t\t\t\t\t\tra.NetBps, hold, over, bb, ra.Reason)'
)


def main():
    if not os.path.exists(SRC):
        raise SystemExit("run this from ~/vega-bot")

    src = open(SRC, encoding="utf-8").read()
    if "pre-borrow read" in src:
        print("already fixed -- nothing to do")
        return 0

    n = src.count(OLD)
    if n != 1:
        print("REFUSING -- reverse Reason line found %d times, want 1" % n)
        return 1

    # The rewrite reads ra.NetBps AFTER the subtraction, so it must sit below
    # it. Verify rather than assume: if the order ever flips, this silently
    # starts recording the pre-borrow number again, which is the exact bug.
    sub = "ra.NetBps -= over"
    if src.index(sub) > src.index(OLD):
        print("REFUSING -- 'ra.NetBps -= over' no longer precedes the Reason line")
        return 1

    src = src.replace(OLD, NEW, 1)
    shutil.copy2(SRC, SRC + ".prereason")
    open(SRC, "w", encoding="utf-8").write(src)
    print("  ok  reverse entries now record net AFTER borrow, borrow shown explicitly")
    print("  ok  original Assess text kept as 'pre-borrow read'")
    print("\nNo behaviour change: NetBps was already correct, only the text was wrong.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
