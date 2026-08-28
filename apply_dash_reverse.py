#!/usr/bin/env python3
"""Put the reverse book on the dashboard.

THE BUG THIS FIXES IS A LYING PANEL, NOT A MISSING ONE.

config/capital.json lists the reverse book, so budgets() renders a row for it.
But budgets() scans only DataDir (data/) and CrossDataDir (data/union/), and the
reverse ledger is at data/reverse/capital_reverse.json. So the row reconciles
config against nothing, prints "no position opened yet", and would go on
printing that after reverse opened twenty positions.

A panel that asserts something false is worse than an absent one. That is the
same defect as the cross-venue book that rendered as live for 38 hours after its
writer stopped.

Two changes:

  1. budgets() also scans the reverse directory, so the budget row tells the
     truth about holds and free capital.
  2. A new section reads data/reverse/positions.json and shows the positions
     themselves -- with funding and BORROW in separate columns, because a
     position earning 40 and paying 35 is not the position earning 6 and paying
     1, though both net 5.

Requires pkg/dashboard/reverse.go to be in place first.

Run from ~/vega-bot.
"""

import os
import re
import shutil
import sys

DASH = "pkg/dashboard/dashboard.go"
CAP = "pkg/dashboard/capital.go"
MON = "cmd/monitor/main.go"

# ---------------------------------------------------------------- 1. Server

SERVER_OLD = "\tCrossDataDir string\n}"
SERVER_NEW = """\tCrossDataDir string
\t// ReverseDataDir holds the reverse book's positions.json and capital ledger.
\t//
\t// Empty means the section is not rendered at all. That is deliberately
\t// distinct from "pointed at a reverse book that has opened nothing": a
\t// dashboard that cannot see a book and a book with no positions must never
\t// look the same, which is exactly the confusion the "no position opened yet"
\t// budget row was creating while reverse ran unwatched in data/reverse/.
\tReverseDataDir string
}"""

# -------------------------------------------------------------- 2. Snapshot

SNAP_OLD = "\tBudgets []BudgetView\n}"
SNAP_NEW = """\tBudgets []BudgetView
\t// --- reverse carry, short spot / long perp, from data/reverse/ ---
\tReverse ReverseView
}"""

# ------------------------------------------------------------- 3. populate

CALL_OLD = "\tsnap.Budgets = s.budgets()\n"
CALL_NEW = "\tsnap.Budgets = s.budgets()\n\tsnap.Reverse = s.loadReverse()\n"

# ---------------------------------------------------------- 4. budgets dirs

DIRS_OLD = """\tdirs := []string{s.DataDir}
\tif s.CrossDataDir != "" && s.CrossDataDir != s.DataDir {
\t\tdirs = append(dirs, s.CrossDataDir)
\t}
"""

DIRS_NEW = """\tdirs := []string{s.DataDir}
\tif s.CrossDataDir != "" && s.CrossDataDir != s.DataDir {
\t\tdirs = append(dirs, s.CrossDataDir)
\t}
\t// The reverse book keeps its ledger in its own directory. Leaving it out is
\t// what made the reverse budget row read "no position opened yet"
\t// unconditionally -- it found the book in config/capital.json and then looked
\t// for its ledger in two directories that could never contain it.
\tif s.ReverseDataDir != "" && s.ReverseDataDir != s.DataDir &&
\t\ts.ReverseDataDir != s.CrossDataDir {
\t\tdirs = append(dirs, s.ReverseDataDir)
\t}
"""

# ------------------------------------------------------------- 5. template

SECTION = """
<h2>REVERSE CARRY &mdash; SHORT SPOT / LONG PERP</h2>
{{if not .Reverse.Configured}}
<p class="muted">Not wired. Pass <code>-reverse-data</code> to the dashboard to
show the reverse book. This is NOT the same as "reverse has no positions".</p>
{{else if .Reverse.Err}}
<p class="bad">Reverse book unreadable: {{.Reverse.Err}}</p>
{{else if not .Reverse.Started}}
<p class="muted">Reverse book configured at <code>{{.Reverse.Dir}}</code> but it
has written no positions.json yet. Either the service has not opened a first
position, or it is not running.</p>
{{else}}
<div class="cards">
  <div class="card"><div class="k">open</div><div class="v">{{.Reverse.OpenCount}}</div></div>
  <div class="card"><div class="k">closed</div><div class="v">{{.Reverse.ClosedCount}}</div></div>
  <div class="card"><div class="k">funding net of borrow</div><div class="v">{{usd .Reverse.FundingUSD}}</div></div>
  <div class="card"><div class="k">borrow paid</div><div class="v">{{usd .Reverse.BorrowUSD}}</div></div>
  <div class="card"><div class="k">round trips</div><div class="v">{{usd .Reverse.CostUSD}}</div></div>
  <div class="card"><div class="k">net</div><div class="v">{{usd .Reverse.NetUSD}}</div></div>
  <div class="card"><div class="k">capital deployed</div><div class="v">{{usd .Reverse.CapitalUSD}}</div></div>
  <div class="card"><div class="k">return on capital</div><div class="v">{{printf "%.3f%%" .Reverse.ReturnPct}}</div></div>
  <div class="card"><div class="k">won / lost</div><div class="v">{{.Reverse.Profitable}} / {{.Reverse.Losing}}</div></div>
  <div class="card"><div class="k">avg held</div><div class="v">{{printf "%.2f d" .Reverse.MeanHoldDays}}</div></div>
</div>

<p class="muted">Funding is shown ALREADY NET OF BORROW; the borrow column is
what was subtracted. Exit cost on an open position is an ESTIMATE (marked ~),
assumed symmetric with entry &mdash; a position has not made money until it has
paid to get out. A NEGATIVE rate is what this book wants: shorts are being paid.</p>

<h3>Open</h3>
{{if .Reverse.Open}}
<table>
<tr><th>symbol</th><th>venue</th><th>opened</th><th>held d</th><th>rate %</th>
<th>borrow bps/hr</th><th>funding bps</th><th>borrow bps</th>
<th>entry bps</th><th>exit bps</th><th>net bps</th><th>net $</th><th>ret %</th></tr>
{{range .Reverse.Open}}
<tr>
<td>{{.Symbol}}</td><td>{{.Venue}}</td><td>{{.OpenedAt}}</td>
<td>{{printf "%.2f" .HeldDays}}</td>
<td>{{printf "%.4f" .RatePct}}</td>
<td>{{printf "%.3f" .BorrowBpsHr}}</td>
<td>{{printf "%.2f" .NetCarryBps}}</td>
<td>{{printf "%.2f" .BorrowPaidBps}}</td>
<td>{{printf "%.2f" .EntryCostBps}}</td>
<td>{{if .ExitEstimate}}~{{end}}{{printf "%.2f" .ExitCostBps}}</td>
<td class="{{if lt .NetBps 0.0}}bad{{else}}good{{end}}">{{printf "%.2f" .NetBps}}</td>
<td>{{usd .NetUSD}}</td>
<td>{{printf "%.3f" .ReturnPct}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="muted">No reverse positions open. The book is running and enforcing
its budget; it has not found a candidate that clears the borrow cost.</p>
{{end}}

<h3>Closed</h3>
{{if .Reverse.Closed}}
<table>
<tr><th>symbol</th><th>venue</th><th>closed</th><th>held d</th><th>reason</th>
<th>funding bps</th><th>borrow bps</th><th>net bps</th><th>net $</th><th>ret %</th></tr>
{{range .Reverse.Closed}}
<tr>
<td>{{.Symbol}}</td><td>{{.Venue}}</td><td>{{.ClosedAt}}</td>
<td>{{printf "%.2f" .HeldDays}}</td><td>{{.Reason}}</td>
<td>{{printf "%.2f" .NetCarryBps}}</td>
<td>{{printf "%.2f" .BorrowPaidBps}}</td>
<td class="{{if lt .NetBps 0.0}}bad{{else}}good{{end}}">{{printf "%.2f" .NetBps}}</td>
<td>{{usd .NetUSD}}</td>
<td>{{printf "%.3f" .ReturnPct}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="muted">Nothing closed yet.</p>
{{end}}
{{end}}
"""


def patch(path, pairs, backup):
    """Replace each (name, old, new) exactly once, or refuse the whole file."""
    src = open(path, encoding="utf-8").read()
    for name, old, _ in pairs:
        n = src.count(old)
        if n != 1:
            print("  REFUSING %s -- %s found %d times, want 1" % (path, name, n))
            return None
    for _, old, new in pairs:
        src = src.replace(old, new, 1)
    shutil.copy2(path, path + backup)
    open(path, "w", encoding="utf-8").write(src)
    return True


def main():
    if not os.path.exists(DASH):
        raise SystemExit("run this from ~/vega-bot")
    if not os.path.exists("pkg/dashboard/reverse.go"):
        raise SystemExit("pkg/dashboard/reverse.go missing -- copy it up first")

    src = open(DASH, encoding="utf-8").read()
    if "ReverseDataDir" in src:
        print("already wired -- nothing to do")
        return 0

    # The template lives in dashboard.go as a Go string. Anchor on the closing
    # body tag rather than on any section heading: headings get reworded, and a
    # patch that silently matches the wrong section is worse than one that
    # refuses.
    if src.count("</body>") != 1:
        print("REFUSING -- </body> found %d times, want 1" % src.count("</body>"))
        return 1

    ok = patch(DASH, [
        ("Server.CrossDataDir", SERVER_OLD, SERVER_NEW),
        ("Snapshot.Budgets", SNAP_OLD, SNAP_NEW),
        ("snap.Budgets call", CALL_OLD, CALL_NEW),
        ("</body>", "</body>", SECTION + "</body>"),
    ], ".predashrev")
    if not ok:
        return 1
    print("  ok  Server.ReverseDataDir, Snapshot.Reverse, loadReverse() call, template section")

    if not patch(CAP, [("budgets dirs", DIRS_OLD, DIRS_NEW)], ".predashrev"):
        print("  !!  dashboard.go patched but capital.go was not -- restore from"
              " %s.predashrev before rebuilding" % DASH)
        return 1
    print("  ok  budgets() now scans the reverse directory")

    # cmd/monitor: flag, then hand it to the Server. Construction style varies,
    # so match either a struct literal field or a plain assignment.
    msrc = open(MON, encoding="utf-8").read()
    if "dashReverse" in msrc:
        print("  --  cmd/monitor already has -reverse-data-view")
        return 0

    if msrc.count("\tflag.Parse()\n") != 1:
        print("  REFUSING cmd/monitor -- flag.Parse() not found exactly once")
        return 1

    flag_decl = ('\tdashReverse := flag.String("reverse-data-view", "",\n'
                 '\t\t"reverse book directory to DISPLAY on the dashboard. Read-only, and '
                 'separate from -data: this is the box\'s own book, not the one it trades")\n')
    msrc = msrc.replace("\tflag.Parse()\n", flag_decl + "\tflag.Parse()\n", 1)

    lit = re.compile(r"\n(\s*)CrossDataDir:(\s*)([^\n]*?),\n")
    asn = re.compile(r"\n(\s*)(\w+)\.CrossDataDir\s*=\s*([^\n]*)\n")

    if len(lit.findall(msrc)) == 1:
        msrc = lit.sub(lambda m: "\n%sCrossDataDir:%s%s,\n%sReverseDataDir: *dashReverse,\n"
                       % (m.group(1), m.group(2), m.group(3), m.group(1)), msrc, count=1)
    elif len(asn.findall(msrc)) == 1:
        msrc = asn.sub(lambda m: "\n%s%s.CrossDataDir = %s\n%s%s.ReverseDataDir = *dashReverse\n"
                       % (m.group(1), m.group(2), m.group(3),
                          m.group(1), m.group(2)), msrc, count=1)
    else:
        print("  REFUSING cmd/monitor -- could not find exactly one CrossDataDir"
              " assignment. Add this by hand next to it:")
        print("      ReverseDataDir: *dashReverse,")
        print("  (dashboard.go and capital.go ARE patched and are fine.)")
        return 1

    shutil.copy2(MON, MON + ".predashrev")
    open(MON, "w", encoding="utf-8").write(msrc)
    print("  ok  cmd/monitor -reverse-data-view flag, wired to the dashboard Server")
    print("\nDefault is empty: the section stays hidden until vega.service asks for it.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
