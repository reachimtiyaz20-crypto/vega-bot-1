#!/usr/bin/env python3
"""Boros probe v3 -- is the market list paginated, or is Boros actually dead?

v2 reported "0 live, 50 matured" with marketIds running 2..51. That is EXACTLY
fifty rows on a contiguous id range, which is the signature of a default page
size, not of a protocol that stopped listing. Reporting "Boros has no live
markets" off that would have been the same class of mistake as the +39.5 bps
settlement figure: a real number read without checking what produced it.

So this asks the question properly before drawing any conclusion:

  1. what does the TOP LEVEL of the response contain -- total? resumeToken?
  2. does limit/skip change the row count?
  3. only then: which markets are live, and what is implied minus realised?

Also fixes /v1/indicators, which needs timeFrame (5m/1h/1d/1w) plus select.
Full error bodies, because v1 truncated the list of valid select values.

WHAT v2 ALREADY ESTABLISHED, from the contract config rather than from memory:

    takerFee   5e14 / 1e18   =  5.0 bps
    kIM        3.125e17      =  3.2x max leverage  (I had said 1.2x -- wrong)
    paymentPeriod  28800     =  8h settlement
    markApr 4.51% vs floatingApr 3.44% on the matured BTC market
                             =  implied sits ~1.07% ABOVE realised

That last line is the shape of the answer on one dead market: implied above
realised means the SHORT YU side is the one being paid, and a long-YU
"mispricing" trade would have lost. Consistent with every other structure we
have measured -- the market prices funding slightly rich, not cheap.

Run from ~/vega-bot:  python3 boros3.py
"""

import json
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = "https://api-boros.pendle.finance/apis"
NOW = time.time()


def get(path, params=None, timeout=30):
    url = BASE + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"User-Agent": "vega-boros/3.0",
                                               "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def rows_of(d):
    r = d.get("results") or d.get("markets") or d.get("data")
    if isinstance(r, dict):
        r = list(r.values())
    return r if isinstance(r, list) else []


def main():
    print("=" * 68)
    print("1. IS IT PAGINATED?")
    print("=" * 68)
    d = get("/v1/markets")
    print("  top-level keys: %s" % sorted(d.keys()))
    for k, v in d.items():
        if k not in ("results", "markets", "data"):
            print("    %-20s %s" % (k, json.dumps(v)[:120]))
    print("  default row count: %d" % len(rows_of(d)))

    best = d
    for p in ({"limit": 500}, {"limit": 200}, {"limit": 100},
              {"skip": 50, "limit": 500}, {"isWhitelisted": "true"}):
        try:
            t = get("/v1/markets", p)
            n = len(rows_of(t))
            ids = [m.get("marketId") for m in rows_of(t)]
            print("  %-28s -> %3d rows, ids %s..%s"
                  % (json.dumps(p), n,
                     min(ids) if ids else "-", max(ids) if ids else "-"))
            if n > len(rows_of(best)):
                best = t
        except urllib.error.HTTPError as e:
            print("  %-28s -> HTTP %s %s" % (json.dumps(p), e.code,
                                             e.read().decode()[:150]))
        except Exception as e:
            print("  %-28s -> %s" % (json.dumps(p), e))

    rows = rows_of(best)
    print("\n  using the largest result set: %d markets" % len(rows))

    print("\n" + "=" * 68)
    print("2. LIVE MARKETS ONLY")
    print("=" * 68)
    live = []
    for m in rows:
        im, da = m.get("imData") or {}, m.get("data") or {}
        mat = im.get("maturity")
        if isinstance(mat, (int, float)) and mat > NOW:
            live.append((mat, m, im, da))
    live.sort()

    if not live:
        newest = max((im.get("maturity") or 0)
                     for m in rows for im in [m.get("imData") or {}])
        print("  NO live markets across %d rows." % len(rows))
        print("  newest maturity anywhere: %s (%.0f days ago)"
              % (time.strftime("%Y-%m-%d", time.gmtime(newest)),
                 (NOW - newest) / 86400))
        print("\n  If pagination above returned no more than 50 rows on every")
        print("  attempt, this is real and Boros has not listed a new market")
        print("  in months -- which would make it unusable for us regardless")
        print("  of how good the mechanism is. Worth confirming on the app UI")
        print("  before writing it off.")
    else:
        print("%-34s %-12s %9s %9s %9s %12s"
              % ("market", "matures", "implied", "floating", "premium", "OI"))
        prem = []
        for mat, m, im, da in live:
            imp = da.get("markApr")
            flo = da.get("floatingApr")
            p = (imp - flo) * 100 if isinstance(imp, (int, float)) and \
                isinstance(flo, (int, float)) else None
            if p is not None:
                prem.append(p)
            print("%-34s %-12s %8s%% %8s%% %8s%% %12s"
                  % (str(im.get("name"))[:34],
                     time.strftime("%Y-%m-%d", time.gmtime(mat)),
                     ("%.2f" % (imp * 100)) if isinstance(imp, (int, float)) else "-",
                     ("%.2f" % (flo * 100)) if isinstance(flo, (int, float)) else "-",
                     ("%+.2f" % p) if p is not None else "-",
                     ("%.0f" % da["notionalOI"])
                     if isinstance(da.get("notionalOI"), (int, float)) else "-"))
        if prem:
            prem.sort()
            print("\n  IMPLIED MINUS FLOATING across %d live markets" % len(prem))
            print("  median %+.2f%%   min %+.2f%%   max %+.2f%%"
                  % (prem[len(prem) // 2], prem[0], prem[-1]))
            print("\n  Positive = implied ABOVE realised = short YU is the paid")
            print("  side. Negative = long YU is paid. If the median sits near")
            print("  zero the market is efficient and there is no spread to")
            print("  harvest -- only a clean way to HEDGE carry we already run.")

    print("\n" + "=" * 68)
    print("3. INDICATORS -- timeFrame required, find the `select` values")
    print("=" * 68)
    mid = (live[0][1].get("marketId") if live else rows[0].get("marketId"))
    print("  marketId %s" % mid)
    base = {"marketId": mid, "timeFrame": "1h"}
    for a in (dict(base, select="u,fp"), dict(base, select="u"),
              dict(base, select="underlyingApr"), dict(base)):
        try:
            ind = get("/v1/indicators", a)
            print("\n  WORKED: %s" % a)
            print(json.dumps(ind, indent=2)[:2500])
            break
        except urllib.error.HTTPError as e:
            print("\n  %s -> HTTP %s\n%s" % (a, e.code, e.read().decode()[:900]))
        except Exception as e:
            print("\n  %s -> %s" % (a, e))


if __name__ == "__main__":
    main()
