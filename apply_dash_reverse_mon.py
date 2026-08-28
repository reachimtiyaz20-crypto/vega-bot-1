#!/usr/bin/env python3
"""Hand the reverse directory to the dashboard Server in cmd/monitor.

apply_dash_reverse.py refused this file and correctly wrote nothing: it expected
CrossDataDir on its own line, and here the whole Server literal is one line. A
patch that cannot find its anchor must refuse rather than guess, so the refusal
was the script working, not failing.

Run from ~/vega-bot, after apply_dash_reverse.py.
"""

import os
import shutil
import sys

MON = "cmd/monitor/main.go"

OLD = ("ds := &dashboard.Server{JournalDir: journalDir, DataDir: abs, "
       "CrossDataDir: crossDir, Registry: reg, StartedAt: time.Now()}")

NEW = ("ds := &dashboard.Server{JournalDir: journalDir, DataDir: abs, "
       "CrossDataDir: crossDir, ReverseDataDir: *dashReverse, "
       "Registry: reg, StartedAt: time.Now()}")

FLAG = ('\tdashReverse := flag.String("reverse-data-view", "",\n'
        '\t\t"reverse book directory to DISPLAY on the dashboard. Read-only, and "+\n'
        '\t\t\t"separate from -data: this is the book this box SHOWS, not the one it trades. "+\n'
        '\t\t\t"Empty hides the section entirely, which is deliberately different from "+\n'
        '\t\t\t"showing a reverse book that holds nothing")\n')


def main():
    if not os.path.exists(MON):
        raise SystemExit("run this from ~/vega-bot")

    src = open(MON, encoding="utf-8").read()
    if "dashReverse" in src:
        print("already wired -- nothing to do")
        return 0

    for name, a in (("Server literal", OLD), ("flag.Parse()", "\tflag.Parse()\n")):
        if src.count(a) != 1:
            print("REFUSING -- %s found %d times, want 1" % (name, src.count(a)))
            return 1

    src = src.replace("\tflag.Parse()\n", FLAG + "\tflag.Parse()\n", 1)
    src = src.replace(OLD, NEW, 1)

    shutil.copy2(MON, MON + ".predashrev")
    open(MON, "w", encoding="utf-8").write(src)
    print("  ok  -reverse-data-view flag added")
    print("  ok  dashboard.Server given ReverseDataDir")
    print("\nDefault empty: nothing changes until vega.service passes the flag.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
