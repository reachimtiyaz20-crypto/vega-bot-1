#!/usr/bin/env python3
"""Boros v4 -- page the whole market list, then price implied against realised.

v3 settled the question v2 got wrong. The list IS paginated: the response
carries a `resumeToken`, `limit` caps at 200, and ids already run past 201. So
"0 live, 50 matured" was an artifact of reading a quarter of page one, not a
dead protocol. Worth remembering that the artifact looked exactly like a
finding -- contiguous ids, plausible story, completely wrong conclusion.

Two fixes over v3:
  - follow resumeToken to the end instead of stopping at the first page
  - live.sort() compared dicts whenever two markets shared a maturity; sorts
    on an explicit key now

THE QUESTION THIS ANSWERS

  markApr      what the order book charges to take the fixed side (IMPLIED)
  floatingApr  what funding is actually paying right now (REALISED)

  premium = implied - floating

  POSITIVE -> implied above realised -> the SHORT YU side is being paid
  NEGATIVE -> implied below realised -> the LONG YU side is being paid

If the median premium across live markets sits near zero, Boros prices funding
fairly and there is no spread to harvest -- it is then only a clean way to HEDGE
carry we already run, converting a decaying variable yield into a fixed one.
That would still be worth having. It is a different claim from finding an edge,
and I want the two kept apart.

Known from the contract config: takerFee 5.0 bps, kIM 3.125e17 -> 3.2x max
leverage, paymentPeriod 28800s (8h).

Run from ~/vega-bot:  python3 boros4.py
"""

import json
import statistics
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
    req = urllib.request.Request(url, headers={"User-Agent": "vega-boros/4.0",
                                               "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def all_markets():
    out, token, pages = [], None, 0
    while pages < 40:
        p = {"limit": 200}
        if token:
            p["resumeToken"] = token
        d = get("/v1/markets", p)
        rows = d.get("results") or []
        out += rows
        pages += 1
        token = d.get("resumeToken")
        print("  page %d: %d rows (total %d)%s"
              % (pages, len(rows), len(out), "" if token else "  [last]"))
        if not token or not rows:
            break
        time.sleep(0.2)
    return out


def main():
    print("PAGING THE FULL MARKET LIST")
    rows = all_markets()
    print("\n%d markets total\n" % len(rows))

    live, matured = [], 0
    for m in rows:
        im, da = m.get("imData") or {}, m.get("data") or {}
        mat = im.get("maturity")
        if isinstance(mat, (int, float)) and mat > NOW:
            live.append((mat, m.get("marketId") or 0, m, im, da))
        else:
            matured += 1
    live.sort(key=lambda x: (x[0], x[1]))

    print("=" * 78)
    print("LIVE MARKETS: %d   (matured: %d)" % (len(live), matured))
    print("=" * 78)
    if not live:
        print("  still none after paging every market. That is now a real")
        print("  finding rather than an artifact.")
        return

    print("%-32s %-11s %8s %8s %9s %11s %10s"
          % ("market", "matures", "implied", "float", "premium", "OI", "vol24h"))
    prem, byplat = [], {}
    for mat, mid, m, im, da in live:
        imp, flo = da.get("markApr"), da.get("floatingApr")
        oi, vol = da.get("notionalOI"), da.get("volume24h")
        p = None
        if isinstance(imp, (int, float)) and isinstance(flo, (int, float)):
            p = (imp - flo) * 100
            prem.append((p, str(im.get("name"))[:32], imp * 100, flo * 100, oi))
            plat = (m.get("platform") or {}).get("name") or "?"
            byplat.setdefault(plat, []).append(p)
        print("%-32s %-11s %7s%% %7s%% %8s%% %11s %10s"
              % (str(im.get("name"))[:32],
                 time.strftime("%Y-%m-%d", time.gmtime(mat)),
                 ("%.2f" % (imp * 100)) if isinstance(imp, (int, float)) else "-",
                 ("%.2f" % (flo * 100)) if isinstance(flo, (int, float)) else "-",
                 ("%+.2f" % p) if p is not None else "-",
                 ("%.0f" % oi) if isinstance(oi, (int, float)) else "-",
                 ("%.0f" % vol) if isinstance(vol, (int, float)) else "-"))

    if not prem:
        print("\n  no market carried both markApr and floatingApr")
        return

    vals = sorted(x[0] for x in prem)
    print("\n" + "=" * 78)
    print("IMPLIED MINUS REALISED, across %d live markets" % len(vals))
    print("=" * 78)
    print("  median %+.2f%%   mean %+.2f%%   min %+.2f%%   max %+.2f%%"
          % (statistics.median(vals), statistics.mean(vals), vals[0], vals[-1]))
    pos = sum(1 for v in vals if v > 0)
    print("  implied ABOVE realised on %d of %d (%.0f%%)"
          % (pos, len(vals), 100.0 * pos / len(vals)))

    print("\n  by venue:")
    for plat, vs in sorted(byplat.items()):
        print("    %-14s n=%-4d median %+.2f%%" % (plat, len(vs),
                                                   statistics.median(vs)))

    prem.sort()
    print("\n  LONG YU is paid (implied below realised):")
    for p, n, i, f, oi in prem[:6]:
        print("    %-32s implied %6.2f%%  float %6.2f%%  %+6.2f%%  OI %s"
              % (n, i, f, p, ("%.0f" % oi) if isinstance(oi, (int, float)) else "-"))
    print("\n  SHORT YU is paid (implied above realised):")
    for p, n, i, f, oi in prem[-6:]:
        print("    %-32s implied %6.2f%%  float %6.2f%%  %+6.2f%%  OI %s"
              % (n, i, f, p, ("%.0f" % oi) if isinstance(oi, (int, float)) else "-"))

    print("\nHOW TO READ IT")
    print("  A 5.0 bps taker fee is charged on the way in and again on the way")
    print("  out. Any premium smaller than ~10 bps annualised over the holding")
    print("  period is not a trade, it is a fee donation. The spread has to")
    print("  clear the round trip before anything else is worth discussing --")
    print("  the same test that killed maker volume at 0.72 bps.")

    print("\n" + "=" * 78)
    print("INDICATORS")
    print("=" * 78)
    mid = live[0][2].get("marketId")
    base = {"marketId": mid, "timeFrame": "1h"}
    for a in (dict(base, select="u,fp"), dict(base, select="u"),
              dict(base, select="underlyingApr"), dict(base)):
        try:
            ind = get("/v1/indicators", a)
            print("  WORKED: %s" % a)
            print(json.dumps(ind, indent=2)[:2000])
            break
        except urllib.error.HTTPError as e:
            print("  %s -> HTTP %s\n%s\n" % (a, e.code, e.read().decode()[:700]))
        except Exception as e:
            print("  %s -> %s" % (a, e))


if __name__ == "__main__":
    main()
