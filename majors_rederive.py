#!/usr/bin/env python3
"""Does the majors book still earn 19% at 10x, or did that belong to a regime?

WHY THIS EXISTS

"Majors basis, ~19%/yr at 10x on OKX borrow" has been the one structure we call
viable, and every plan for 20 lakh rests on it. steth_basis.py then measured
Binance ETH funding over the trailing year at +2.38%/yr against a 3.00% cost of
dollars -- which runs the trade BACKWARDS at leverage, because each extra turn
subtracts the difference.

19% at 10x needs roughly 4.6%/yr net funding. We measured half that on ETH. So
either the basket is carried by other coins, or the 19% came from a window that
has closed. This settles which.

THE MODEL, same one used everywhere in this project

    net on capital = L*f - b*(L-1) - round_trip

    f   basket funding, annualised, on notional
    b   3.00%/yr, OKX USDT, measured
    L   leverage

Two things it deliberately does NOT do. It does not model the de-risk rule --
that reduces losses, it does not create carry, and a structure that only works
because of a stop is not a structure. And it does not assume the trailing year
is representative: the whole point is the ROLLING view, because a single window
is how "19%" got quoted in the first place.

WHAT DECIDES IT

    f > b   leverage helps
    f < b   leverage hurts, and the book is better UNLEVERED

That single comparison is the entire question, and it is not close on ETH.

Run from ~/vega-bot:  python3 majors_rederive.py
"""

import json
import os
import statistics
import time
import urllib.request

CACHE = "data/boros"
FAPI = "https://fapi.binance.com"
BORROW = 3.00
ROUND_TRIP_BPS = 45.0
LEVERAGES = [1, 2, 3, 5, 10]
TARGET = 19.0
DEFAULT = ["BTCUSDT", "ETHUSDT", "BNBUSDT", "XRPUSDT", "SOLUSDT"]


def get(url, timeout=30):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-majors/2.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def basket():
    """Prefer the coins the book actually trades over a guess."""
    for p in ("data/majors/majors_state.json",):
        try:
            st = json.load(open(p))
            for k in ("symbols", "coins", "basket", "universe"):
                v = st.get(k)
                if isinstance(v, list) and v:
                    print("  basket from %s: %s" % (p, ", ".join(map(str, v))))
                    return [str(x).upper() for x in v]
        except Exception:
            pass
    try:
        u = open("/etc/systemd/system/vega-majors.service").read()
        if "-symbols " in u:
            v = u.split("-symbols ")[1].split()[0].split(",")
            print("  basket from unit file: %s" % ", ".join(v))
            return [x.strip().upper() for x in v if x.strip()]
    except Exception:
        pass
    print("  no basket found in state or unit -- using default majors")
    return DEFAULT


def funding(sym, years=5):
    os.makedirs(CACHE, exist_ok=True)
    p = os.path.join(CACHE, "binance_%s.json" % sym)
    if os.path.exists(p):
        try:
            d = json.load(open(p))
            if len(d) > 500:
                return d
        except Exception:
            pass
    out, cur = [], int((time.time() - years * 365 * 86400) * 1000)
    for _ in range(400):
        try:
            rows = get("%s/fapi/v1/fundingRate?symbol=%s&startTime=%d&limit=1000"
                       % (FAPI, sym, cur))
        except Exception as e:
            print("    %s fetch stopped: %s" % (sym, e))
            break
        if not rows:
            break
        out += [[int(r["fundingTime"]), float(r["fundingRate"])] for r in rows]
        last = int(rows[-1]["fundingTime"])
        if last <= cur:
            break
        cur = last + 1
        if len(rows) < 1000:
            break
        time.sleep(0.12)
    if out:
        json.dump(out, open(p, "w"))
    return out


def to_daily(rows):
    """day (UTC epoch days) -> annualised funding percent for that day."""
    if len(rows) < 100:
        return {}
    gaps = [rows[i + 1][0] - rows[i][0] for i in range(min(300, len(rows) - 1))]
    gaps = [g for g in gaps if g > 0]
    per_year = 365.0 * 24 * 3600_000.0 / statistics.median(gaps)
    acc = {}
    for t, r in rows:
        d = int(t // 86400_000)
        acc.setdefault(d, []).append(r * per_year * 100.0)
    return {d: sum(v) / len(v) for d, v in acc.items()}


def net(f, L):
    return L * f - BORROW * (L - 1) - ROUND_TRIP_BPS / 100.0


def main():
    if not os.path.isdir("data"):
        raise SystemExit("run from ~/vega-bot")

    print("BASKET")
    syms = basket()

    print("\nLOADING FUNDING (5y, cached in data/boros/)")
    series = {}
    for s in syms:
        rows = funding(s)
        d = to_daily(rows)
        if len(d) > 200:
            series[s] = d
            days = sorted(d)
            print("  %-10s %5d days  from %s  mean %+.2f%%/yr"
                  % (s, len(d),
                     time.strftime("%Y-%m-%d", time.gmtime(days[0] * 86400)),
                     statistics.mean(d.values())))
        else:
            print("  %-10s unavailable" % s)
    if not series:
        raise SystemExit("no funding data")

    alldays = sorted(set().union(*[set(d) for d in series.values()]))
    bask = {}
    for d in alldays:
        vals = [s[d] for s in series.values() if d in s]
        if vals:
            bask[d] = sum(vals) / len(vals)
    days = sorted(bask)

    print("\n" + "=" * 74)
    print("PER COIN, TRAILING YEAR vs THE 3.00%% COST OF DOLLARS")
    print("=" * 74)
    cut = days[-1] - 365
    print("%-10s %11s %11s %11s %10s"
          % ("symbol", "mean", "median", "vs borrow", "days > b"))
    for s, d in sorted(series.items()):
        v = [d[k] for k in d if k >= cut]
        if len(v) < 60:
            continue
        above = sum(1 for x in v if x > BORROW)
        print("%-10s %10.2f%% %10.2f%% %+10.2f%% %9.0f%%"
              % (s, statistics.mean(v), statistics.median(v),
                 statistics.mean(v) - BORROW, 100.0 * above / len(v)))

    tv = [bask[k] for k in days if k >= cut]
    f_now = statistics.mean(tv)
    print("%-10s %10.2f%% %10.2f%% %+10.2f%% %9.0f%%"
          % ("BASKET", f_now, statistics.median(tv), f_now - BORROW,
             100.0 * sum(1 for x in tv if x > BORROW) / len(tv)))

    print("\n" + "=" * 74)
    print("THE BOOK ON THE TRAILING YEAR")
    print("=" * 74)
    print("  basket funding %+.2f%%/yr   borrow %.2f%%/yr   round trip %.0f bps"
          % (f_now, BORROW, ROUND_TRIP_BPS))
    print("\n%8s %14s" % ("leverage", "net on capital"))
    for L in LEVERAGES:
        r = net(f_now, L)
        flag = "" if r > 0 else "   <- loses"
        print("%7dx %13.1f%%%s" % (L, r, flag))

    need = (TARGET + BORROW * 9 + ROUND_TRIP_BPS / 100.0) / 10.0
    print("\n  To hit %.0f%% at 10x the basket must pay %.2f%%/yr." % (TARGET, need))
    print("  It is paying %.2f%%. Shortfall %.2f points." % (f_now, need - f_now))

    print("\n" + "=" * 74)
    print("ROLLING 365-DAY WINDOWS -- was 19%% EVER there, and when?")
    print("=" * 74)
    win, i = [], 0
    for j, d in enumerate(days):
        while days[i] < d - 365:
            i += 1
        if j - i < 300:
            continue
        seg = [bask[k] for k in days[i:j + 1]]
        win.append((d, sum(seg) / len(seg)))
    if not win:
        print("  not enough history")
        return

    r10 = [net(f, 10) for _, f in win]
    r5 = [net(f, 5) for _, f in win]
    r1 = [net(f, 1) for _, f in win]
    s10 = sorted(r10)

    def q(v, p):
        s = sorted(v)
        return s[min(int(p * (len(s) - 1)), len(s) - 1)]

    print("  %d rolling years\n" % len(win))
    print("%10s %10s %10s %10s" % ("", "1x", "5x", "10x"))
    for lab, p in (("worst", 0.0), ("p25", .25), ("median", .5),
                   ("p75", .75), ("best", 1.0)):
        print("%10s %9.1f%% %9.1f%% %9.1f%%"
              % (lab, q(r1, p), q(r5, p), q(r10, p)))
    print("%10s %9.1f%% %9.1f%% %9.1f%%"
          % ("NOW", net(f_now, 1), net(f_now, 5), net(f_now, 10)))

    hit = sum(1 for x in r10 if x >= TARGET)
    print("\n  windows clearing %.0f%% at 10x: %d of %d (%.0f%%)"
          % (TARGET, hit, len(r10), 100.0 * hit / len(r10)))
    pos10 = sum(1 for x in r10 if x > 0)
    print("  windows merely POSITIVE at 10x: %d of %d (%.0f%%)"
          % (pos10, len(r10), 100.0 * pos10 / len(r10)))

    print("\n  by year -- basket funding and what 10x would have paid")
    byyr = {}
    for d, f in win:
        byyr.setdefault(time.gmtime(d * 86400).tm_year, []).append(f)
    for y in sorted(byyr):
        f = statistics.mean(byyr[y])
        print("    %d   funding %+6.2f%%   10x %+8.1f%%" % (y, f, net(f, 10)))

    print("\n" + "=" * 74)
    print("READ")
    print("=" * 74)
    print("""  If the recent years sit below the 3.00%% borrow while the early ones
  sit above it, then 19%% was real once and is not now, and quoting it
  is quoting a regime rather than a strategy. That is the same shape as
  HLP 78%% -> 7.25%% and basis 25%% -> under 5%%.

  And note which way leverage points. When funding is below the cost of
  dollars, the UNLEVERED book is the best version of this trade -- not
  the boldest one. Every turn past 1x is paying 3%% to earn less.""")


if __name__ == "__main__":
    main()
