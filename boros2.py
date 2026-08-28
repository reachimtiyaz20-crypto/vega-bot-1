#!/usr/bin/env python3
"""Boros probe v2 -- learn the real schema instead of guessing at it.

v1 established the API is live and open: 50 markets, no auth. It failed on two
things, both now known rather than assumed:

  APRs are NESTED under `data`, not flat on the market object.
  /v1/indicators requires `timeFrame` (5m/1h/1d/1w) and `select`.

It also surfaced something that matters more than either: market 2 is
"Binance BTCUSDT 26 Sep 2025" -- matured a year ago. These are DATED
instruments, so most of the 50 are dead. Any premium computed across all of
them would be averaging expired contracts. This version separates live from
matured before it computes anything.

Prints full error bodies. v1 truncated at 200 chars and cut off the list of
valid `select` values, which is exactly the information needed.
"""

import json, sys, time, urllib.error, urllib.parse, urllib.request

BASE = "https://api-boros.pendle.finance/apis"


def get(path, params=None, timeout=25):
    url = BASE + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"User-Agent": "vega-boros/2.0",
                                               "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def main():
    d = get("/v1/markets")
    rows = d.get("results") or d.get("markets") or d.get("data")
    if isinstance(rows, dict):
        rows = list(rows.values())
    print("%d markets\n" % len(rows))

    print("=" * 70)
    print("FULL SHAPE OF ONE MARKET -- every field, so we stop guessing")
    print("=" * 70)
    print(json.dumps(rows[0], indent=2)[:4000])

    # Which of the 50 are actually alive?
    print("\n" + "=" * 70)
    print("ALL MARKETS -- name, maturity, OI, volume")
    print("=" * 70)
    now = time.time()
    live = []
    for m in rows:
        md = m.get("metadata") or {}
        im = m.get("imData") or {}
        da = m.get("data") or {}
        name = im.get("name") or md.get("name") or str(m.get("marketId"))
        mat = m.get("maturity") or (m.get("config") or {}).get("maturity") or im.get("maturity")
        try:
            matf = float(mat)
            alive = matf > now
            mats = time.strftime("%Y-%m-%d", time.gmtime(matf))
        except (TypeError, ValueError):
            alive, mats = None, str(mat)[:12]
        oi = da.get("notionalOI")
        vol = da.get("volume24h")
        if alive:
            live.append(m)
        print("  id %-4s %-34s mat %-12s %-7s OI %-12s vol %s"
              % (m.get("marketId"), str(name)[:34], mats,
                 "LIVE" if alive else ("MATURED" if alive is False else "?"),
                 ("%.0f" % oi) if isinstance(oi, (int, float)) else "-",
                 ("%.0f" % vol) if isinstance(vol, (int, float)) else "-"))

    print("\n  %d live, %d matured" % (len(live), len(rows) - len(live)))

    print("\n" + "=" * 70)
    print("DATA BLOCK of the first LIVE market (this holds the rates)")
    print("=" * 70)
    if live:
        print("  name: %s" % ((live[0].get("imData") or {}).get("name")))
        print(json.dumps(live[0].get("data") or {}, indent=2)[:2500])
        mid = live[0].get("marketId")
    else:
        print("  NO LIVE MARKETS -- every one has matured.")
        mid = rows[0].get("marketId")

    print("\n" + "=" * 70)
    print("FUNDING RATE SYMBOLS")
    print("=" * 70)
    try:
        fs = get("/v1/funding-rate/all-funding-rate-symbols")
        syms = fs.get("fundingRateSymbols") or []
        print("  %d symbols" % len(syms))
        for s in syms[:40]:
            print("    %-30s %-10s %s" % (s.get("fundingRateSymbol"),
                                          s.get("assetSymbol"),
                                          s.get("exchange") or s.get("platform")))
    except Exception as e:
        print("  %s" % e)

    print("\n" + "=" * 70)
    print("INDICATORS -- timeFrame is required; find the right `select`")
    print("=" * 70)
    print("  marketId %s" % mid)
    base = {"marketId": mid, "timeFrame": "1h"}
    tries = [dict(base, select="u,fp"), dict(base, select="u"),
             dict(base, select=["u", "fp"]), dict(base, select="u&select=fp"),
             dict(base, indicators="u,fp"), dict(base)]
    for a in tries:
        try:
            ind = get("/v1/indicators", a)
            print("\n  WORKED: %s" % a)
            print(json.dumps(ind, indent=2)[:3000])
            return
        except urllib.error.HTTPError as e:
            print("\n  %s\n  -> HTTP %s\n%s" % (a, e.code, e.read().decode()[:1200]))
        except Exception as e:
            print("\n  %s -> %s" % (a, e))


if __name__ == "__main__":
    main()
