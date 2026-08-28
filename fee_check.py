#!/usr/bin/env python3
"""What the round trip actually costs, read from the account rather than assumed.

WHY IT MATTERS HERE

The cross-venue book charges a flat 40 bps a round trip, a number I picked. At
a 7-day hold that assumption is the difference between a real edge and a
rounding error:

	CHIP at 127.6%/yr, held 7 days:  245 bps gross
	                  at 40 bps:     205 bps net  ->  ~53%/yr on capital
	                  at 70 bps:     175 bps net  ->  ~45%/yr on capital
	                  at 25 bps:     220 bps net  ->  ~57%/yr on capital

Not fatal either way, which is worth knowing -- but the whole point of this rig
is that nobody has to guess.

THE FOUR LEGS

	open  Hyperliquid perp short   taker
	open  CEX spot buy             taker
	close CEX spot sell            taker
	close Hyperliquid perp buy     taker

Hyperliquid publishes 0.045% taker on perps at base tier, dropping with 14-day
volume past $5M. Bybit's spot rate depends on the account, so it is read here
rather than looked up.

MEXC is worth checking by hand: it has run zero-fee promotions on many spot
pairs, and CASHCAT's spot leg lives there. If MEXC spot is genuinely 0%, the
round trip drops by roughly half.

READ ONLY. Signed GET requests, nothing else.

	export BYBIT_KEY=...
	export BYBIT_SECRET=...
	python3 fee_check.py
"""

import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = "https://api.bybit.com"
RECV = "5000"

# Published, base tier. https://hyperliquidguide.com/guides/fees
HL_PERP_TAKER_BPS = 4.5
HL_PERP_MAKER_BPS = 1.5

COINS = ["CHIP", "XMR", "CASHCAT", "GRASS", "VVV", "APEX"]


def signed_get(key, secret, path, query):
    ts = str(int(time.time() * 1000))
    sign = hmac.new(secret.encode(), (ts + key + RECV + query).encode(),
                    hashlib.sha256).hexdigest()
    req = urllib.request.Request(
        "%s%s?%s" % (BASE, path, query),
        headers={"X-BAPI-API-KEY": key, "X-BAPI-TIMESTAMP": ts,
                 "X-BAPI-RECV-WINDOW": RECV, "X-BAPI-SIGN": sign,
                 "User-Agent": "vega-fees/1.0"})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.loads(r.read().decode())


def show(key, secret, category, symbol=None):
    q = "category=%s" % category
    if symbol:
        q += "&symbol=%s" % symbol
    try:
        r = signed_get(key, secret, "/v5/account/fee-rate", q)
    except urllib.error.HTTPError as e:
        print("  HTTP %s: %s" % (e.code, e.read().decode()[:200]))
        return None
    except urllib.error.URLError as e:
        print("  network: %s" % e)
        return None
    if r.get("retCode") != 0:
        print("  retCode %s: %s" % (r.get("retCode"), r.get("retMsg")))
        return None
    rows = (r.get("result") or {}).get("list") or []
    if not rows:
        print("  no rows returned")
        return None
    out = {}
    for row in rows[:8]:
        try:
            t = float(row.get("takerFeeRate", 0)) * 10000
            m = float(row.get("makerFeeRate", 0)) * 10000
        except (TypeError, ValueError):
            continue
        sym = row.get("symbol") or row.get("baseCoin") or "(default)"
        out[sym] = (t, m)
        print("    %-14s taker %5.2f bps   maker %5.2f bps" % (sym, t, m))
    return out


def main():
    key = os.environ.get("BYBIT_KEY", "").strip()
    secret = os.environ.get("BYBIT_SECRET", "").strip()
    if not key or not secret:
        sys.exit("set BYBIT_KEY and BYBIT_SECRET (read-only key is enough)")

    print("BYBIT SPOT FEES (your account)")
    spot = show(key, secret, "spot")
    print("\nBYBIT DERIVATIVES FEES (for reference; the perp leg is on Hyperliquid)")
    show(key, secret, "linear")

    print("\nHYPERLIQUID, published base tier")
    print("    perp           taker %5.2f bps   maker %5.2f bps"
          % (HL_PERP_TAKER_BPS, HL_PERP_MAKER_BPS))
    print("    (tiers fall past $5M of 14-day volume; you are at base)")

    # Take the worst spot taker seen, so the estimate is not flattered.
    spot_taker = None
    if spot:
        spot_taker = max(v[0] for v in spot.values())

    print("\nROUND TRIP FOR THE CROSS-VENUE TRADE")
    if spot_taker is None:
        print("  spot rate unavailable -- cannot compute")
        return
    legs = [("open  HL perp short  (taker)", HL_PERP_TAKER_BPS),
            ("open  CEX spot buy   (taker)", spot_taker),
            ("close CEX spot sell  (taker)", spot_taker),
            ("close HL perp buy    (taker)", HL_PERP_TAKER_BPS)]
    total = 0.0
    for label, bps in legs:
        total += bps
        print("  %-32s %5.2f bps" % (label, bps))
    print("  %-32s %5.2f bps" % ("TOTAL", total))
    print("\n  hlbook.py currently assumes 40.00 bps")
    if total > 40:
        print("  -> UNDERSTATED by %.1f bps. Every net figure is optimistic."
              % (total - 40))
    else:
        print("  -> conservative by %.1f bps. Real nets are slightly better."
              % (40 - total))

    print("\n  WHAT THIS DOES TO THE LIVE POSITIONS (7-day hold)")
    print("  %-10s %10s %12s %12s" % ("coin", "entry %/yr", "net bps", "on capital"))
    entries = {"CHIP": 127.6, "XMR": 80.0, "CASHCAT": 60.6, "GRASS": 55.8}
    for c, f in entries.items():
        gross = f * 7.0 / 365.0 * 100.0      # bps over 7 days
        net = gross - total
        on_cap = net / 10000.0 * (365.0 / 7.0) / 2.0 * 100.0
        print("  %-10s %9.1f%% %11.1f %11.1f%%" % (c, f, net, on_cap))

    print("\n  These use ENTRY rates. Measured decay says realised will be")
    print("  lower -- CASHCAT entered at 164%% and realised 109%% over a week;")
    print("  XMR entered at 81%% and realised 37%%. The book will settle it.")

    print("\nSTILL TO CHECK BY HAND")
    print("  MEXC spot fee -- CASHCAT's leg lives there and MEXC has run")
    print("  zero-fee spot promotions. If it is 0%%, the round trip on that")
    print("  pair drops by roughly half.")
    print("  Slippage is NOT in any of this. Depth was measured within 25 bps")
    print("  of the ask, so accumulating a position spends some of that band.")


if __name__ == "__main__":
    main()
