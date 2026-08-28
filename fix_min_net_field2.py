#!/usr/bin/env python3
"""Move MinNetBps out of Position and into PaperConfig. Precisely.

Two failed attempts on this, both from pattern-matching instead of looking:

  1. anchored on the first `NotionalUSD float64` -- which is in Position, not
     PaperConfig, because both structs carry a notional
  2. the cleanup guarded on any occurrence of "MinNetBps", which matched the
     LEGITIMATE b.cfg.MinNetBps in the gate, so it refused mid-way

Observed state:
    line  37  // MinNetBps is the margin an entry must clear ABOVE borrow,
    lines 38-42  continuation comments
    line  43  MinNetBps float64 `json:"min_net_bps,omitempty"`
    line 531  if ra.NetBps > over+b.cfg.MinNetBps {     <- keep this

So: delete the declaration block wherever it currently is, insert it into
PaperConfig, and verify by content at every step rather than by position.

Run from ~/vega-bot.
"""

import os
import re
import shutil
import sys

PAPER = "pkg/funding/paper.go"

FIELD = """
	// MinNetBps is the margin an entry must clear ABOVE borrow, over the
	// planned hold.
	//
	// Measured across 20 days of journal: entries that merely cleared borrow
	// (around -2 bps/hr) returned -39%/yr, while entries with a real margin
	// (around -8 bps/hr) returned +99%. A positive edge is not the same as an
	// edge worth taking, and the whole difference is entry quality.
	//
	// Zero preserves the old behaviour of accepting anything positive.
	MinNetBps float64 `json:"min_net_bps,omitempty"`
"""


def main():
    if not os.path.exists(PAPER):
        raise SystemExit("run this from ~/vega-bot")
    lines = open(PAPER, encoding="utf-8").read().split("\n")

    # --- locate the declaration line (not the usage) ---
    decl = [i for i, l in enumerate(lines)
            if "MinNetBps" in l and "float64" in l and "b.cfg" not in l]
    if len(decl) != 1:
        print("REFUSING -- found %d declaration lines, want exactly 1" % len(decl))
        for i in decl:
            print("   line %d: %s" % (i + 1, lines[i].strip()))
        return 1
    d = decl[0]

    # Walk backwards over the comment block immediately above it.
    start = d
    while start > 0 and lines[start - 1].strip().startswith("//"):
        start -= 1
    # Drop one blank line above the comment block, if present.
    if start > 0 and lines[start - 1].strip() == "":
        start -= 1

    print("removing lines %d-%d:" % (start + 1, d + 1))
    for i in range(start, d + 1):
        print("   %s" % lines[i])
    del lines[start:d + 1]

    src = "\n".join(lines)

    # Sanity: the gate must survive, the declaration must be gone.
    if "b.cfg.MinNetBps" not in src:
        print("REFUSING -- the gate usage was removed; that is the one line to keep")
        return 1
    if re.search(r"^\s*MinNetBps\s+float64", src, re.M):
        print("REFUSING -- a declaration still remains after removal")
        return 1

    # --- insert into PaperConfig ---
    m = re.search(r"type\s+PaperConfig\s+struct\s*\{", src)
    if not m:
        print("REFUSING -- could not find 'type PaperConfig struct {'")
        return 1
    src = src[:m.end()] + FIELD.rstrip("\n") + src[m.end():]

    shutil.copy2(PAPER, PAPER + ".prefix2")
    open(PAPER, "w", encoding="utf-8").write(src)
    print("\n  ok  declaration moved into PaperConfig")
    print("  ok  gate at b.cfg.MinNetBps left intact")
    print("\nrebuild: go build ./... && go vet ./pkg/funding/")
    return 0


if __name__ == "__main__":
    sys.exit(main())
