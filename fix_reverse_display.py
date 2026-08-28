#!/usr/bin/env python3
"""Stop calling open positions losses, and stop rounding a $400 book to $0.

TWO DISPLAY DEFECTS, BOTH MINE, NEITHER AFFECTING WHAT THE BOOK TRADES.

1. WON / LOST counted OPEN positions.

   A position pays its whole round trip the instant it opens and earns it back
   over hours. It is therefore underwater by construction until it crosses
   break-even. Counting that as a "loss" reported a book of four healthy young
   positions as "1 won / 3 lost" while CLOSED read 0 and the table underneath
   said "Nothing closed yet."

   Won and lost now describe only what has actually closed. Open positions get
   their own honest pair: in profit / underwater.

2. Every dollar figure rounded to whole dollars.

   NET showed "$-0" for -$0.375. FUNDING NET OF BORROW showed "$0" for $0.345.
   The usd helper is right for a book of twenty thousand and useless for one of
   four hundred, and a measurement rig that displays its own P&L as zero is not
   measuring anything. Three decimals on the reverse panel only.

Requires the updated pkg/dashboard/reverse.go (InProfit/Underwater/Won/Lost).

Run from ~/vega-bot.
"""

import os
import shutil
import sys

DASH = "pkg/dashboard/dashboard.go"

OLD_CARD = ('  <div class="card"><div class="k">won / lost</div>'
            '<div class="v">{{.Reverse.Profitable}} / {{.Reverse.Losing}}</div></div>')

NEW_CARD = ('  <div class="card"><div class="k">open: in profit / underwater</div>'
            '<div class="v">{{.Reverse.InProfit}} / {{.Reverse.Underwater}}</div></div>\n'
            '  <div class="card"><div class="k">closed: won / lost</div>'
            '<div class="v">{{.Reverse.Won}} / {{.Reverse.Lost}}</div></div>')

# Money on this panel is measured in cents, not dollars.
MONEY = [
    ("{{usd .Reverse.FundingUSD}}", '{{printf "$%.3f" .Reverse.FundingUSD}}'),
    ("{{usd .Reverse.BorrowUSD}}", '{{printf "$%.3f" .Reverse.BorrowUSD}}'),
    ("{{usd .Reverse.CostUSD}}", '{{printf "$%.3f" .Reverse.CostUSD}}'),
    ("{{usd .Reverse.NetUSD}}", '{{printf "$%.3f" .Reverse.NetUSD}}'),
]

# Appears once in the open table and once in the closed table.
ROW_OLD = "<td>{{usd .NetUSD}}</td>"
ROW_NEW = '<td>{{printf "$%.3f" .NetUSD}}</td>'


def main():
    if not os.path.exists(DASH):
        raise SystemExit("run this from ~/vega-bot")

    src = open(DASH, encoding="utf-8").read()
    if ".Reverse.InProfit" in src:
        print("already fixed -- nothing to do")
        return 0

    checks = [("won/lost card", OLD_CARD, 1)]
    checks += [(o, o, 1) for o, _ in MONEY]
    checks += [("net $ column", ROW_OLD, 2)]
    for name, needle, want in checks:
        n = src.count(needle)
        if n != want:
            print("REFUSING -- %s found %d times, want %d" % (name, n, want))
            return 1

    src = src.replace(OLD_CARD, NEW_CARD, 1)
    for o, n in MONEY:
        src = src.replace(o, n, 1)
    src = src.replace(ROW_OLD, ROW_NEW)   # both tables

    shutil.copy2(DASH, DASH + ".predisplay")
    open(DASH, "w", encoding="utf-8").write(src)
    print("  ok  open positions no longer counted as wins or losses")
    print("  ok  reverse panel shows cents, not rounded dollars")
    print("\nDisplay only. Nothing about what the book trades or records changes.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
