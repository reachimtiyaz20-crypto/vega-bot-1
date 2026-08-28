#!/usr/bin/env python3
"""Equity and commodity perps: is the funding carry, or unhedgeable gap risk?

THE CLAIM

Hyperliquid's xyz: markets (NVDA, BRENTOIL, gold, indices) settle funding hourly
against an oracle that tracks the underlying. One source headlines "30% yields
on NVDA". Our own Boros pull showed BRENTOIL floating funding at -20.44%/yr.

THE MECHANISM, and why it might genuinely be uncompeted

When the stock market closes, the ORACLE FREEZES on the closing print while the
perp keeps trading 24/7. Post-close news pushes the perp away from that frozen
price, funding spikes, and it stays spiked until the next open. Crypto
arbitrageurs cannot hedge it because they have no brokerage account; equity
traders cannot touch it because they have no Hyperliquid. That is a real
structural gap rather than another crowded trade.

THE SUSPICION THIS TESTS

The same source describes the yield as payment for "traders willing to take the
contra side of a crowded post-close directional bet." That is being PAID TO HOLD
RISK YOU CANNOT HEDGE -- precisely the structure that made settlement sniping
-15.3 bps, where the payment turned out to be compensation for a move that then
actually happened.

So the question is not how big the funding is. It is WHEN it accrues:

    funding earned while the underlying is OPEN     hedgeable, real carry
    funding earned while the underlying is CLOSED   gap risk in a carry costume

If the yield lives in closed hours, we are being paid to hold an unhedged
overnight position in a single stock, and the first bad earnings gap takes back
a year of it. If it is spread evenly, or concentrated in open hours, it is a
real trade and worth pursuing hard.

MARKET HOURS USED (UTC, US equities on EDT)
    regular session   13:30 - 20:00, Mon-Fri
    everything else   closed, and weekends flagged separately

Commodities keep near-24h sessions, so their 'closed' bucket means weekends
only. Reported separately rather than mixed in.

Run from ~/vega-bot:  python3 xyz_perps.py
"""

import datetime
import json
import statistics
import time
import urllib.request

HL = "https://api.hyperliquid.xyz/info"
DAYS = 45
OPEN_UTC = (13.5, 20.0)      # US equity regular session, EDT


def post(body, timeout=30):
    req = urllib.request.Request(
        HL, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json",
                 "User-Agent": "vega-xyz/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def discover():
    """Find the equity/commodity markets without assuming the API shape."""
    found = {}

    # 1. the plain universe -- some builder markets appear here with a prefix
    try:
        d = post({"type": "metaAndAssetCtxs"})
        uni = d[0].get("universe") if isinstance(d, list) else None
        ctxs = d[1] if isinstance(d, list) and len(d) > 1 else []
        if uni:
            print("  main universe: %d markets" % len(uni))
            for i, m in enumerate(uni):
                n = m.get("name", "")
                c = ctxs[i] if i < len(ctxs) else {}
                found[n] = c
    except Exception as e:
        print("  metaAndAssetCtxs -> %s" % e)

    # 2. builder-deployed perp dexs
    try:
        dexs = post({"type": "perpDexs"})
        names = []
        for x in (dexs or []):
            if isinstance(x, dict) and x.get("name"):
                names.append(x["name"])
            elif isinstance(x, str):
                names.append(x)
        print("  perp dexs: %s" % (", ".join(map(str, names)) or "(none)"))
        for dex in names:
            if not dex:
                continue
            try:
                d = post({"type": "metaAndAssetCtxs", "dex": dex})
                uni = d[0].get("universe") if isinstance(d, list) else None
                ctxs = d[1] if isinstance(d, list) and len(d) > 1 else []
                if uni:
                    print("    dex '%s': %d markets" % (dex, len(uni)))
                    for i, m in enumerate(uni):
                        n = "%s:%s" % (dex, m.get("name", ""))
                        found[n] = ctxs[i] if i < len(ctxs) else {}
            except Exception as e:
                print("    dex '%s' -> %s" % (dex, e))
    except Exception as e:
        print("  perpDexs -> %s" % e)

    return found


def funding_hist(coin, start_ms):
    out, cur, seen = [], start_ms, set()
    for _ in range(300):
        try:
            rows = post({"type": "fundingHistory", "coin": coin,
                         "startTime": cur})
        except Exception:
            break
        if not rows:
            break
        new = 0
        for r in rows:
            t = int(r["time"])
            if t in seen:
                continue
            seen.add(t)
            out.append((t, float(r["fundingRate"])))
            new += 1
        last = max(int(r["time"]) for r in rows)
        if new == 0 or last <= cur:
            break
        cur = last + 1
        time.sleep(0.1)
    out.sort()
    return out


def bucket(ms):
    t = datetime.datetime.fromtimestamp(ms / 1000.0, datetime.timezone.utc)
    if t.weekday() >= 5:
        return "weekend"
    h = t.hour + t.minute / 60.0
    return "open" if OPEN_UTC[0] <= h < OPEN_UTC[1] else "closed"


def main():
    print("DISCOVERING MARKETS")
    found = discover()
    xyz = {k: v for k, v in found.items()
           if ":" in k or any(t in k.upper() for t in
                              ("NVDA", "AAPL", "TSLA", "BRENT", "GOLD",
                               "SILVER", "SPX", "NDX", "OIL", "MSTR", "META"))}
    if not xyz:
        print("\n  no equity/commodity markets found. Names seen:")
        print("  " + ", ".join(sorted(found)[:60]))
        return
    print("\n  %d candidate markets: %s" % (len(xyz), ", ".join(sorted(xyz))))

    start = int((time.time() - DAYS * 86400) * 1000)
    print("\n" + "=" * 78)
    print("FUNDING BY SESSION -- %d days" % DAYS)
    print("=" * 78)
    print("%-18s %7s %9s %9s %9s %9s %11s"
          % ("market", "n", "all %/yr", "OPEN", "CLOSED", "WEEKEND",
             "OI $"))

    rows = []
    for coin in sorted(xyz):
        h = funding_hist(coin, start)
        if len(h) < 100:
            print("%-18s %7s  (no history)" % (coin[:18], len(h)))
            continue
        per_year = 24 * 365.0
        b = {"open": [], "closed": [], "weekend": []}
        for t, r in h:
            b[bucket(t)].append(r * per_year * 100.0)
        allv = [r * per_year * 100.0 for _, r in h]
        ctx = xyz[coin] or {}
        oi = ctx.get("openInterest")
        px = ctx.get("markPx") or ctx.get("oraclePx")
        try:
            oi_usd = float(oi) * float(px)
        except (TypeError, ValueError):
            oi_usd = None

        def m(k):
            return statistics.mean(b[k]) if b[k] else float("nan")

        print("%-18s %7d %8.1f%% %8.1f%% %8.1f%% %8.1f%% %11s"
              % (coin[:18], len(h), statistics.mean(allv),
                 m("open"), m("closed"), m("weekend"),
                 ("%.0f" % oi_usd) if oi_usd else "-"))
        rows.append((coin, b, allv, oi_usd))

    if not rows:
        return

    print("\n" + "=" * 78)
    print("WHERE THE MONEY IS EARNED")
    print("=" * 78)
    print("  Share of TOTAL funding accrued in each session. The US regular")
    print("  session is only 32.5 of 168 hours a week (19%), so anything")
    print("  near 19%% in the OPEN column means funding is spread evenly.\n")
    print("%-18s %10s %10s %10s" % ("market", "open", "closed", "weekend"))
    for coin, b, allv, _ in rows:
        tot = sum(abs(x) for k in b for x in b[k])
        if tot <= 0:
            continue
        sh = {k: 100.0 * sum(abs(x) for x in b[k]) / tot for k in b}
        print("%-18s %9.0f%% %9.0f%% %9.0f%%"
              % (coin[:18], sh["open"], sh["closed"], sh["weekend"]))

    print("\n  hours in each bucket: open 19%, closed 52%, weekend 29%")

    print("\n" + "=" * 78)
    print("READ")
    print("=" * 78)
    print("""  If OPEN share is near 19%, funding accrues evenly around the clock
  and the yield is not a closed-market phenomenon. That would make this
  a real carry we can hedge during the session -- the first structure in
  weeks with a reason to be uncompeted rather than just a big number.

  If OPEN share is far below 19%, the yield lives in hours when the
  underlying cannot be traded. You would be collecting funding for
  holding an unhedged single-name position over news, and one earnings
  gap on NVDA can move 10%+ -- against a funding rate quoted in tens of
  percent per YEAR, which is basis points per hour.

  Do that arithmetic before believing any headline: 30%/yr is 0.0034%
  per hour. A 5% overnight gap is 1,470 hours of funding. That is the
  whole trade, and it is why the rate is what it is.""")


if __name__ == "__main__":
    main()
