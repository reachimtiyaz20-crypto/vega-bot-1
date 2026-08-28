#!/usr/bin/env python3
"""Put MinNetBps in PaperConfig, where it belongs.

The previous patch anchored on the first `NotionalUSD float64` in paper.go.
That is in Position, not PaperConfig -- both structs carry a notional, and the
regex took the first match. The compiler caught it immediately:

    paper.go:531: b.cfg.MinNetBps undefined (type PaperConfig has no field...)

Which is the right kind of failure: loud, immediate, and impossible to ship.
A field silently added to the wrong struct that still compiled would have been
far worse.

This removes the misplaced field and inserts it into PaperConfig by anchoring
on the type declaration itself rather than on a field name that appears in
several structs.

Run from ~/vega-bot.
"""

import os
import re
import shutil
import sys

PAPER = "pkg/funding/paper.go"

MARKER = "MinNetBps is the margin an entry must clear ABOVE borrow"

FIELD = """
	// MinNetBps is the margin an entry must clear ABOVE borrow, over the
	// planned hold.
	//
	// Measured across 20 days of journal: entries that merely cleared borrow
	// (about -2 bps/hr) returned -39%/yr, while entries with a real margin
	// (about -8 bps/hr) returned +99%. A positive edge is not the same as an
	// edge worth taking, and the difference between those two is entirely
	// entry quality.
	//
	// Zero preserves the old behaviour of accepting anything positive.
	MinNetBps float64 `json:"min_net_bps,omitempty"`
"""


def main():
    if not os.path.exists(PAPER):
        raise SystemExit("run this from ~/vega-bot")
    src = open(PAPER, encoding="utf-8").read()

    # 1. Remove the misplaced block: the comment lines plus the declaration.
    if MARKER in src:
        lines = src.split("\n")
        keep, removed, i = [], 0, 0
        while i < len(lines):
            if MARKER in lines[i]:
                # Drop this comment line, any following comment lines, and the
                # MinNetBps declaration itself.
                while i < len(lines) and lines[i].strip().startswith("//"):
                    i += 1
                    removed += 1
                if i < len(lines) and "MinNetBps" in lines[i]:
                    i += 1
                    removed += 1
                continue
            keep.append(lines[i])
            i += 1
        src = "\n".join(keep)
        print("  ok  removed %d misplaced lines" % removed)
    else:
        print("  --  no misplaced field found")

    if "MinNetBps" in src:
        print("REFUSING -- MinNetBps still present after cleanup; inspect by hand")
        return 1

    # 2. Insert into PaperConfig, anchored on the type declaration.
    m = re.search(r"type\s+PaperConfig\s+struct\s*\{", src)
    if not m:
        print("REFUSING -- could not find 'type PaperConfig struct {'")
        return 1
    src = src[:m.end()] + FIELD.rstrip("\n") + src[m.end():]

    shutil.copy2(PAPER, PAPER + ".prefixfield")
    open(PAPER, "w", encoding="utf-8").write(src)
    print("  ok  MinNetBps inserted into PaperConfig")
    print("\nrebuild to confirm: go build ./... && go test ./pkg/funding/")
    return 0


if __name__ == "__main__":
    sys.exit(main())
