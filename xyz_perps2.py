#!/usr/bin/env python3
"""Equity/commodity perp funding, v2 -- fixes the naming bug that voided v1.

WHAT WENT WRONG IN v1

The dex meta already returns names carrying the dex prefix ("xyz:NVDA"). I
prefixed them again, asked for funding on "xyz:xyz:NVDA", and got nothing back
for all 272 builder markets. The only two rows that printed were HMSTR and SPX
from the MAIN universe -- crypto memecoins my keyword filter caught by accident,
not equity perps. Their split came out 18/53/29 against 19/52/29 hours, exactly
uniform, which is correct for a 24/7 asset and tells us nothing about equities.

So v1 measured the method, not the question.

v2 fixes the name, and also stops wasting minutes on dead markets: it ranks by
open interest FIRST and only pulls history for markets carrying real size.

THE QUESTION, unchanged

    funding earned while the underlying is OPEN     hedgeable carry
    funding earned while the underlying is CLOSED   gap risk in a carry costume

US regular session is 32.5 of 168 hours = 19%. A market whose OPEN share sits
near 19% earns evenly around the clock. One far below 19% pays you mainly in
hours when the underlying cannot be traded, which is not carry -- it is rent on
an unhedgeable overnight position.

Keep the scale in mind: 30%/yr is 0.0034% per HOUR. A single 5% earnings gap is
1,470 hours of funding.

Run from ~/vega-bot:  python3 xyz_perps2.py
"""

import datetime
import json
import statistics
import time
import urllib.request

HL = "https://api.hyperliquid.xyz/info"
DAYS = 45
OPEN_UTC = (13.5, 20.0)
MIN_OI = 200_000.0
TOP = 25


def post(body, timeout=30):
    req = urllib.request.Request(
        HL, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "User-Agent": "vega-xyz/2.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def oi_usd(ctx):
    try:
        return float(ctx.get("openInterest")) * float(
            ctx.get("markPx") or ctx.get("oraclePx"))
    except (TypeError, ValueError):
        return None


def discover():
    out = {}
    try:
        dexs = post({"type": "perpDexs"})
        names = [x.get("name") if isinstance(x, dict) else x for x in (dexs or [])]
    except Exception as e:
        print("  perpDexs -> %s" % e)
        names = []
    for dex in [n for n in names if n]:
        try:
            d = post({"type": "metaAndAssetCtxs", "dex": dex})
            uni = d[0].get("universe") if isinstance(d, list) else None
            ctxs = d[1] if isinstance(d, list) and len(d) > 1 else []
            if not uni:
                continue
            for i, m in enumerate(uni):
                nm = m.get("name", "")
                # THE v1 BUG: nm already contains the dex prefix. Do not add it.
                key = nm if ":" in nm else "%s:%s" % (dex, nm)
                out[key] = (dex, ctxs[i] if i < len(ctxs) else {})
            print("  %-6s %3d markets" % (dex, len(uni)))
        except Exception as e:
            print("  %-6s -> %s" % (dex, e))
    return out


def funding_hist(coin, dex, start_ms):
    out, cur, seen = [], start_ms, set()
    for attempt in ({"type": "fundingHistory", "coin": coin, "startTime": cur},):
        pass
    for _ in range(200):
        rows = None
        for body in ({"type": "fundingHistory", "coin": coin, "startTime": cur},
                     {"type": "fundingHistory", "coin": coin,
                      "startTime": cur, "dex": dex}):
            try:
                r = post(body)
                if r:
                    rows = r
                    break
            except Exception:
                continue
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
        time.sleep(0.08)
    out.sort()
    return out


def bucket(ms):
    t = datetime.datetime.fromtimestamp(ms / 1000.0, datetime.timezone.utc)
    if t.weekday() >= 5:
        return "weekend"
    h = t.hour + t.minute / 60.0
    return "open" if OPEN_UTC[0] <= h < OPEN_UTC[1] else "closed"


def main():
    print("DISCOVERING")
    found = discover()
    print("  %d builder markets total" % len(found))

    sized = []
    for k, (dex, ctx) in found.items():
        o = oi_usd(ctx)
        if o and o >= MIN_OI:
            sized.append((o, k, dex))
    sized.sort(reverse=True)
    print("\n  %d markets with OI >= $%.0fk" % (len(sized), MIN_OI / 1000))
    if not sized:
        print("  nothing carries size. That alone answers the capacity question.")
        return
    sized = sized[:TOP]

    start = int((time.time() - DAYS * 86400) * 1000)
    print("\n" + "=" * 76)
    print("TOP %d BY OPEN INTEREST -- funding by session, %d days" % (len(sized), DAYS))
    print("=" * 76)
    print("%-20s %12s %9s %9s %9s %9s"
          % ("market", "OI $", "all %/yr", "OPEN", "CLOSED", "WKND"))

    rows = []
    for o, k, dex in sized:
        h = funding_hist(k, dex, start)
        if len(h) < 50:
            print("%-20s %12.0f  (no history: %d)" % (k[:20], o, len(h)))
            continue
        py = 24 * 365.0
        b = {"open": [], "closed": [], "weekend": []}
        for t, r in h:
            b[bucket(t)].append(r * py * 100.0)
        allv = [r * py * 100.0 for _, r in h]

        def m(x):
            return statistics.mean(b[x]) if b[x] else float("nan")

        print("%-20s %12.0f %8.1f%% %8.1f%% %8.1f%% %8.1f%%"
              % (k[:20], o, statistics.mean(allv), m("open"), m("closed"),
                 m("weekend")))
        rows.append((k, b, allv, o))

    if not rows:
        print("\n  no funding history on any sized market.")
        return

    print("\n" + "=" * 76)
    print("SHARE OF FUNDING BY SESSION   (hours: open 19%, closed 52%, wknd 29%)")
    print("=" * 76)
    print("%-20s %9s %9s %9s %12s" % ("market", "open", "closed", "wknd", "verdict"))
    for k, b, allv, o in rows:
        tot = sum(abs(x) for kk in b for x in b[kk])
        if tot <= 0:
            continue
        sh = {kk: 100.0 * sum(abs(x) for x in b[kk]) / tot for kk in b}
        v = "even" if sh["open"] >= 15 else ("CLOSED-HRS" if sh["open"] < 10
                                             else "skewed")
        print("%-20s %8.0f%% %8.0f%% %8.0f%% %12s"
              % (k[:20], sh["open"], sh["closed"], sh["weekend"], v))

    tot_oi = sum(r[3] for r in rows)
    print("\n  total OI across measured markets: $%.0f" % tot_oi)
    print("  our whole book at 20 lakh is ~$23k, so capacity is not the")
    print("  binding constraint here -- which makes this worth finishing.")

    print("\n" + "=" * 76)
    print("READ")
    print("=" * 76)
    print("""  'even' means funding accrues around the clock and can be hedged
  during the session. That would be the first genuinely uncompeted
  structure we have found, and it deserves a paper book immediately.

  'CLOSED-HRS' means the yield is rent on an unhedgeable overnight
  position. Same shape as settlement sniping: the payment exists
  because the risk it compensates actually happens.

  Either way the next question is the hedge. Shorting one of these
  perps leaves you short a single stock, and the offset has to be real
  equity in a brokerage account -- which is the part your UAE company
  might unlock and almost no crypto arbitrageur has.""")


if __name__ == "__main__":
    main()
