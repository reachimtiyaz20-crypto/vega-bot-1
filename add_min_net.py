#!/usr/bin/env python3
"""Require a real edge at entry, not merely a positive one.

WHAT THE GRID SAID

Entry threshold x hold length, 20 days of journal, borrow charged by the clock,
one round trip of 47.7 bps, non-overlapping positions:

    entry bps/hr      1d     2d     3d     5d     7d    10d    14d
    -8.0            +99%   +78%   +79%   +50%   +25%   +16%    +7%
    -4.0            +25%   +18%   +16%    -2%    -5%    -7%   -12%
    -2.0            -39%   -20%   -13%   -16%   -15%   -16%   -16%

Two results, both against expectation.

SHORTER HOLDS WIN. Funding persists in SIGN -- still negative 83% of the time
at 48 hours -- but decays in MAGNITUDE. You enter at -8 bps/hr and it drifts
toward baseline while borrow keeps ticking. The early hours carry the edge.
Sign persistence was measured and magnitude persistence was inferred from it,
which was simply wrong.

ENTRY QUALITY DOMINATES. -2 loses 39%/yr at the same hold where -8 makes 99%.

WHY HOLD LENGTH ALONE CANNOT FIX IT

The existing gate is NetBps > borrow x hold. Shortening the hold raises the bar
only from r > b + 0.94 to r > b + 1.88 -- about -2 bps/hr, which is the losing
cell. The winning cell needs -8. So a minimum NET is required, separately.

    with min-net 100 bps at a 1-day hold:
        24r - 45 - 24b >= 100  ->  r >= 6.0 + b

which lands entries at -6 to -7 bps/hr, against the -8 cell.

DEFAULT ZERO. Inert unless a unit passes it, so the headline book is untouched
and this can be reverted by removing one flag.

Run from ~/vega-bot.
"""

import os
import re
import shutil
import sys

PAPER = "pkg/funding/paper.go"
MON = "cmd/monitor/main.go"

# The reverse branch subtracts borrow, then accepts. Add the floor.
OLD_GATE = """					over := bb * 24.0 * hold
					if ra.NetBps > over {
						ra.NetBps -= over"""

NEW_GATE = """					over := bb * 24.0 * hold
					// A positive edge is not the same as an edge worth taking.
					// Measured over 20 days: entries at -2 bps/hr lose 39%/yr,
					// entries at -8 make 99%. The difference is entirely entry
					// quality, so require a real margin over borrow rather than
					// merely clearing it.
					if ra.NetBps > over+b.cfg.MinNetBps {
						ra.NetBps -= over"""

FLAG = ('\tminNet := flag.Float64("min-net-bps", 0,\n'
        '\t\t"refuse an entry unless its expected net over the planned hold "+\n'
        '\t\t\t"exceeds borrow by at least this many bps. 0 keeps the old "+\n'
        '\t\t\t"behaviour of accepting anything positive")\n')

ASSIGN = "\tpaperCfg.MinNetBps = *minNet\n"
ANCHOR_ASSIGN = "\tpaperCfg.PlannedHoldDays = *holdLong\n"


def main():
    if not os.path.exists(PAPER):
        raise SystemExit("run this from ~/vega-bot")

    src = open(PAPER, encoding="utf-8").read()
    if "MinNetBps" in src:
        print("already applied -- nothing to do")
        return 0

    if src.count(OLD_GATE) != 1:
        print("REFUSING -- reverse gate found %d times, want 1" % src.count(OLD_GATE))
        print("the block searched for was:\n%s" % OLD_GATE)
        return 1

    # Add the config field next to an existing one so it lands in the right
    # struct without guessing the struct's name.
    m = re.search(r"\n(\s*)NotionalUSD(\s+)float64([^\n]*)\n", src)
    if not m:
        print("REFUSING -- could not find NotionalUSD in the paper config")
        return 1

    field = ('\n%sNotionalUSD%sfloat64%s\n'
             '%s// MinNetBps is the margin an entry must clear ABOVE borrow,\n'
             '%s// over the planned hold. Zero accepts anything positive, which\n'
             '%s// measured at -39%%/yr; requiring a real margin measured at +99%%.\n'
             '%sMinNetBps float64 `json:"min_net_bps,omitempty"`\n'
             % (m.group(1), m.group(2), m.group(3),
                m.group(1), m.group(1), m.group(1), m.group(1)))
    src = src[:m.start()] + field + src[m.end():]
    src = src.replace(OLD_GATE, NEW_GATE, 1)

    shutil.copy2(PAPER, PAPER + ".preminnet")
    open(PAPER, "w", encoding="utf-8").write(src)
    print("  ok  MinNetBps added to the paper config")
    print("  ok  reverse entry gate now requires it")

    msrc = open(MON, encoding="utf-8").read()
    if "*minNet" in msrc:
        print("  --  cmd/monitor already wired")
        return 0
    for name, a in (("flag.Parse()", "\tflag.Parse()\n"),
                    ("PlannedHoldDays assignment", ANCHOR_ASSIGN)):
        if msrc.count(a) != 1:
            print("  REFUSING cmd/monitor -- %s found %d times, want 1"
                  % (name, msrc.count(a)))
            print("  (paper.go IS patched; restore from %s.preminnet if needed)"
                  % PAPER)
            return 1
    msrc = msrc.replace("\tflag.Parse()\n", FLAG + "\tflag.Parse()\n", 1)
    msrc = msrc.replace(ANCHOR_ASSIGN, ANCHOR_ASSIGN + ASSIGN, 1)

    shutil.copy2(MON, MON + ".preminnet")
    open(MON, "w", encoding="utf-8").write(msrc)
    print("  ok  -min-net-bps flag added, default 0 (inert)")
    print("\nDefault 0 changes nothing until a unit passes the flag.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
