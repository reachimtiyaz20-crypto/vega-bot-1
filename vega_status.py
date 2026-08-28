#!/usr/bin/env python3
"""One command, every book, every morning.

Replaces the ad-hoc Python blob written fresh each time somebody wants to know
how the books are doing. Three books, a capacity distribution and the service
states, in one screen.

Reads only. Touches nothing any book writes.

	python3 vega_status.py
"""

import collections
import datetime
import glob
import json
import os
import statistics
import subprocess
import sys

NOW = datetime.datetime.now(datetime.timezone.utc)


def hrs(iso):
    try:
        t = datetime.datetime.fromisoformat(iso.replace("Z", "+00:00"))
        return (NOW - t).total_seconds() / 3600.0
    except Exception:
        return float("nan")


def rule(title):
    print("\n" + title)
    print("-" * len(title))


def services():
    rule("SERVICES")
    for s in ("vega", "vega-reverse", "vega-majors", "vega-hlbook",
              "vega-borrow", "vega-capacity.timer"):
        try:
            out = subprocess.run(["systemctl", "is-active", s],
                                 capture_output=True, text=True, timeout=10)
            state = out.stdout.strip() or "unknown"
        except Exception:
            state = "unknown"
        # vega-borrow is a oneshot driven by a timer, so "inactive" is its
        # healthy resting state, not a fault. Saying so beats a scary word.
        note = ""
        if s == "vega-borrow" and state == "inactive":
            note = "  (oneshot; runs hourly on its timer)"
        print("  %-22s %s%s" % (s, state, note))


def positions_book(path, label, capital=None):
    if not os.path.exists(path):
        print("  %s: no positions file" % label)
        return
    try:
        d = json.load(open(path))
    except Exception as e:
        print("  %s: unreadable (%s)" % (label, e))
        return

    o = d.get("open") or {}
    o = list(o.values()) if isinstance(o, dict) else list(o)
    c = d.get("closed") or []
    c = list(c.values()) if isinstance(c, dict) else list(c)

    net_total = 0.0
    inprofit = under = 0
    rows = []
    for p in o:
        e = p.get("entry_cost_bps") or 0.0
        # Exit is unpaid and therefore ESTIMATED as symmetric with entry. A
        # position has not made money until it has paid to get out.
        net = (p.get("funding_collected_bps") or 0.0) - 2 * e
        net_total += net / 10000.0 * (p.get("notional_usd") or 0.0)
        inprofit, under = (inprofit + 1, under) if net >= 0 else (inprofit, under + 1)
        rows.append((net, p, True))
    won = lost = 0
    for p in c:
        e = p.get("entry_cost_bps") or 0.0
        x = p.get("exit_cost_bps") or 0.0
        net = (p.get("funding_collected_bps") or 0.0) - e - x
        net_total += net / 10000.0 * (p.get("notional_usd") or 0.0)
        won, lost = (won + 1, lost) if net >= 0 else (won, lost + 1)
        rows.append((net, p, False))

    print("  %s: %d open (%d in profit / %d underwater), %d closed (%d won / %d lost)"
          % (label, len(o), inprofit, under, len(c), won, lost))
    print("  net including estimated exits: %+.3f USD%s"
          % (net_total, "" if not capital else "  (%.3f%% on $%.0f)"
             % (net_total / capital * 100, capital)))

    for net, p, is_open in sorted(rows, key=lambda r: -r[0]):
        tag = "OPEN  " if is_open else "CLOSED"
        held = hrs(p.get("opened_at", ""))
        extra = ""
        if not is_open:
            e = p.get("entry_cost_bps") or 0.0
            x = p.get("exit_cost_bps") or 0.0
            # The comparison that validates every P&L number we quote.
            extra = "  exit %.2f vs %.2f estimated (%+.0f%%)" % (
                x, e, (x - e) / e * 100 if e else 0)
            extra += "  [%s]" % p.get("close_reason", "?")
        print("    %s %-20s side %+d %6.1fh  borrow/hr %6.3f  NET %+7.2f bps%s"
              % (tag, p.get("id", "?"), p.get("side", 1), held,
                 p.get("borrow_bps_hr") or 0.0, net, extra))


def majors():
    rule("MAJORS BOOK  (the one that scales)")
    path = "data/majors/majors_state.json"
    if not os.path.exists(path):
        print("  no state file -- is vega-majors running?")
        return
    st = json.load(open(path))
    inv = st.get("invested")
    days = st.get("days") or []
    complete = max(len(days) - 1, 0)
    trail = 0.0
    if complete:
        hist = days[:-1][-14:]
        vals = [d["sum_bps_hr"] / d["n"] for d in hist if d.get("n")]
        trail = sum(vals) / len(vals) if vals else 0.0
    f = st.get("funding_bps") or 0.0
    cost = st.get("cost_bps") or 0.0
    net = f - cost
    # Notional and leverage are not in state; read them from the unit file so
    # this never drifts from what is actually running.
    notional, lev = 1000.0, 5.0
    try:
        unit = open("/etc/systemd/system/vega-majors.service").read()
        for tok, name in (("-notional ", "n"), ("-leverage ", "l")):
            if tok in unit:
                v = float(unit.split(tok)[1].split()[0])
                if name == "n":
                    notional = v
                else:
                    lev = v
    except Exception:
        pass
    cap = notional / lev if lev else notional
    print("  position: %s   $%.0f notional at %gx (capital $%.0f)"
          % ("LONG BASIS" if inv else "FLAT", notional, lev, cap))
    print("  de-risk signal: trailing %+.4f bps/hr over %d of 14 complete days"
          % (trail, min(complete, 14)))
    if complete < 14:
        print("                  (rule cannot act until the window fills)")
    print("  funding %+.2f  cost %.2f  net %+.2f bps  = %+.3f%% on capital"
          % (f, cost, net, net / 10000.0 * notional / cap * 100 if cap else 0))
    print("  entries %d, exits %d" % (st.get("entries", 0), st.get("exits", 0)))
    if net < 0 and st.get("entries", 0) == 1 and not st.get("exits"):
        print("  NOTE: negative is expected early -- the round trip is charged")
        print("        at entry and earned back over the hold.")


def hlbook():
    rule("CROSS-VENUE BOOK  (Hyperliquid perp short / CEX spot long)")
    path = "data/hlbook/state.json"
    if not os.path.exists(path):
        print("  no state file -- is vega-hlbook running?")
        return
    try:
        st = json.load(open(path))
    except Exception as e:
        print("  unreadable (%s)" % e)
        return

    o = list((st.get("open") or {}).values())
    c = st.get("closed") or []
    won = sum(1 for p in c if (p.get("net_bps") or 0) >= 0)

    open_net = sum((p.get("funding_bps") or 0) - (p.get("cost_bps") or 0)
                   for p in o)
    closed_net = sum(p.get("net_bps") or 0 for p in c)
    cap = sum(p.get("capital_usd") or 0 for p in o)

    print("  %d open, %d closed (%d won / %d lost)"
          % (len(o), len(c), won, len(c) - won))
    print("  open %+.1f bps (entry cost charged upfront), closed %+.1f bps"
          % (open_net, closed_net))
    if cap:
        print("  capital deployed $%.0f" % cap)

    rows = []
    for p in o:
        net = (p.get("funding_bps") or 0) - (p.get("cost_bps") or 0)
        rows.append((net, p, True))
    for p in c:
        rows.append((p.get("net_bps") or 0, p, False))

    for net, p, is_open in sorted(rows, key=lambda r: -r[0]):
        tag = "OPEN  " if is_open else "CLOSED"
        held = ((NOW.timestamp() * 1000 - (p.get("opened_ms") or 0))
                / 3600000.0) if is_open else (p.get("held_days") or 0) * 24
        extra = ""
        if not is_open:
            extra = "  [%s]" % (p.get("close_reason") or "?")
        print("    %s %-12s %-7s %6.1fh  entry %6.1f%%/yr  now %6.1f%%/yr"
              "  NET %+8.2f bps%s"
              % (tag, p.get("coin", "?"), p.get("spot_venue", "?"), held,
                 p.get("entry_f_pct_yr") or 0, p.get("last_f") or 0,
                 net, extra))

    if len(c) < 10:
        print("  NOTE: %d closes. The reverse book read +23.9%% at four and"
              % len(c))
        print("        +4.6%% at seven. Judge this at thirty, not before.")


def capacity():
    rule("CAPACITY OF THE ALT OPPORTUNITY SET")
    path = "data/capacity/log.jsonl"
    if not os.path.exists(path):
        print("  no samples yet")
        return
    rows = []
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        try:
            rows.append(json.loads(line))
        except Exception:
            continue
    live = [r for r in rows if not r.get("stale")]
    if not live:
        print("  %d samples, all stale" % len(rows))
        return
    caps = sorted(r.get("capacity_usd", 0.0) for r in live)

    def q(p):
        return caps[min(int(p * (len(caps) - 1)), len(caps) - 1)]

    print("  %d samples (%d stale)" % (len(live), len(rows) - len(live)))
    print("  capacity  min $%.0f   p25 $%.0f   median $%.0f   p75 $%.0f   max $%.0f"
          % (caps[0], q(0.25), q(0.5), q(0.75), caps[-1]))
    pf = [r.get("profitable", 0) for r in live]
    print("  profitable candidates  median %d  max %d"
          % (statistics.median(pf), max(pf)))


def main():
    if not os.path.isdir("data"):
        sys.exit("run from ~/vega-bot")
    print("VEGA STATUS  %s UTC" % NOW.strftime("%Y-%m-%d %H:%M"))
    services()
    rule("REVERSE BOOK  (works, holds pocket change)")
    positions_book("data/reverse/positions.json", "reverse", capital=1600.0)
    rule("HEADLINE BOOK  (alt cash-and-carry, the original thesis)")
    positions_book("data/positions.json", "headline", capital=400.0)
    majors()
    hlbook()
    capacity()
    print("\nExit costs on OPEN positions are estimates, assumed symmetric with")
    print("entry. The 'exit vs estimated' figure on CLOSED positions is the")
    print("test of that assumption, and of every net number quoted anywhere.")


if __name__ == "__main__":
    main()
