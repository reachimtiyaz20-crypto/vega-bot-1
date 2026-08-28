#!/usr/bin/env python3
"""Boros: is the implied funding rate cheap or dear versus what funding realises?

Read-only discovery. Market data needs no auth. Dumps the actual schema rather
than trusting assumed field names -- the approach that caught GMX reporting
per-second factors under names nothing else uses.

Boros publishes an indicator called FUTURE PREMIUM (`fp`) = the implied-versus-
underlying spread. If it is exposed per market, their own data answers the
central question and we only have to check it against our series.
"""

import json, statistics, sys, urllib.error, urllib.parse, urllib.request

BASE = "https://api-boros.pendle.finance/apis"


def get(path, params=None, timeout=25):
    url = BASE + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"User-Agent": "vega-boros/1.0",
                                               "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def show_keys(obj, label, limit=40):
    print("  %s keys:" % label)
    if isinstance(obj, dict):
        for k in sorted(obj)[:limit]:
            v = obj[k]
            s = json.dumps(v) if not isinstance(v, str) else v
            print("    %-30s %s" % (k, s[:70]))
    else:
        print("    (not a dict: %s)" % type(obj))


def pick(m, *names):
    for n in names:
        if isinstance(m, dict) and n in m and m[n] not in (None, ""):
            return m[n]
    return None


def main():
    print("BOROS -- funding rate futures. Read-only probe, no auth.\n")

    d = None
    for p in ("/v1/markets", "/markets"):
        try:
            d = get(p)
            print("markets endpoint: %s\n" % p)
            break
        except urllib.error.HTTPError as e:
            print("%s -> HTTP %s" % (p, e.code))
        except Exception as e:
            print("%s -> %s" % (p, e))
    if d is None:
        sys.exit("markets lookup failed on every path")

    rows = d.get("results") or d.get("markets") or d.get("data") or d
    if isinstance(rows, dict):
        rows = rows.get("markets") or list(rows.values())
    if not isinstance(rows, list) or not rows:
        print("unexpected shape. top level:")
        show_keys(d, "response")
        sys.exit(1)

    print("%d markets returned\n" % len(rows))
    show_keys(rows[0], "sample market")

    print("\nMARKETS")
    print("%-30s %12s %12s %10s %14s"
          % ("market", "implied", "underlying", "premium", "open interest"))
    prem = []
    for m in rows:
        name = str(pick(m, "name", "symbol", "marketName", "marketId"))[:30]
        imp = pick(m, "impliedApr", "impliedAPR", "fixedApr", "markApr")
        und = pick(m, "underlyingApr", "underlyingAPR", "floatingApr")
        oi = pick(m, "openInterest", "oi", "totalOi")
        try:
            p = (float(imp) - float(und)) * 100
        except (TypeError, ValueError):
            p = None
        if p is not None:
            prem.append((p, name, float(imp) * 100, float(und) * 100))

        def f(x):
            try:
                return "%.2f%%" % (float(x) * 100)
            except (TypeError, ValueError):
                return "-" if x is None else str(x)[:12]

        print("%-30s %12s %12s %10s %14s"
              % (name, f(imp), f(und),
                 ("%+.2f%%" % p) if p is not None else "-",
                 str(oi)[:14] if oi is not None else "-"))

    if prem:
        prem.sort()
        print("\nIMPLIED MINUS UNDERLYING (the spread we would trade)")
        print("  median %+.2f%%   min %+.2f%%   max %+.2f%%"
              % (statistics.median([x[0] for x in prem]), prem[0][0], prem[-1][0]))
        print("\n  UNDERPRICED (implied below realised -> long YU):")
        for p, n, i, u in prem[:5]:
            print("    %-28s implied %7.2f%%  underlying %7.2f%%  %+.2f%%" % (n, i, u, p))
        print("\n  OVERPRICED (implied above realised -> short YU):")
        for p, n, i, u in prem[-5:]:
            print("    %-28s implied %7.2f%%  underlying %7.2f%%  %+.2f%%" % (n, i, u, p))

    print("\n\nFUNDING RATE SYMBOLS")
    for p in ("/v1/funding-rate/all-funding-rate-symbols",
              "/funding-rate/all-funding-rate-symbols"):
        try:
            fs = get(p)
            items = fs.get("results") or fs.get("symbols") or fs
            if isinstance(items, list):
                print("  %d symbols: %s" % (len(items), ", ".join(
                    str(x if not isinstance(x, dict) else
                        x.get("symbol") or x.get("name")) for x in items[:25])))
            else:
                show_keys(fs, "funding symbols")
            break
        except Exception as e:
            print("  %s -> %s" % (p, e))

    print("\n\nINDICATORS -- u = underlying APR, fp = future premium")
    mid = None
    for m in rows:
        mid = pick(m, "marketId", "id")
        if mid is not None:
            break
    if mid is None:
        print("  no marketId on any market")
        return
    print("  using marketId %s" % mid)
    for a in ({"marketId": mid, "indicators": "u,fp"},
              {"marketId": mid, "indicator": "u,fp"},
              {"marketId": mid, "codes": "u,fp"},
              {"marketId": mid, "indicators": "u,fp", "resolution": "1h"}):
        try:
            ind = get("/v1/indicators", a)
            print("  WORKED with params: %s" % a)
            show_keys(ind, "indicators response")
            break
        except urllib.error.HTTPError as e:
            print("  %s -> HTTP %s %s" % (a, e.code, e.read().decode()[:200]))
        except Exception as e:
            print("  %s -> %s" % (a, e))


if __name__ == "__main__":
    main()
