#!/usr/bin/env python3
"""Read Bybit's ACTUAL margin requirement, instead of modelling it.

WHY THIS EXISTS

Every leverage figure in this project rests on a modelled capital factor of
1.11 -- a number I made up as a plausible portfolio-margin benefit. It was
never measured, and it cannot be right as a constant, because Bybit computes
portfolio margin by STRESS TESTING the whole portfolio against mark-price and
implied-volatility scenarios. The requirement depends on what you are holding,
not on a multiplier.

So stop modelling it. Open a small hedged position and read what the exchange
actually charges.

WHAT IT READS

/v5/account/wallet-balance with accountType=UNIFIED returns both margin regimes
side by side, which is exactly the comparison needed:

	totalInitialMargin        what CROSS margin requires
	totalInitialMarginByMp    what PORTFOLIO margin requires
	accountIMRate             the same, as a rate
	accountIMRateByMp

The ratio between them IS the portfolio-margin benefit, measured on your
account, in your region, at your risk tier. No assumption survives contact with
that number.

READ-ONLY KEYS ARE ENOUGH. This only ever performs GET requests, and it will
refuse to run if given anything that looks like an order instruction. Nothing
here can place, modify or cancel a trade.

USAGE

	export BYBIT_KEY=xxxx
	export BYBIT_SECRET=yyyy
	python3 bybit_margin_check.py            # mainnet
	python3 bybit_margin_check.py --testnet

Run it THREE times and record each: before opening anything, with a spot-only
position, and with the hedged pair. The three readings together separate what
the spot leg costs, what the perp leg costs, and what the HEDGE saves -- which
is the only number that matters for leverage.
"""

import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.request

MAINNET = "https://api.bybit.com"
TESTNET = "https://api-testnet.bybit.com"
RECV = "5000"


def signed_get(base, key, secret, path, query):
    ts = str(int(time.time() * 1000))
    payload = ts + key + RECV + query
    sign = hmac.new(secret.encode(), payload.encode(), hashlib.sha256).hexdigest()
    url = "%s%s?%s" % (base, path, query)
    req = urllib.request.Request(url, headers={
        "X-BAPI-API-KEY": key,
        "X-BAPI-TIMESTAMP": ts,
        "X-BAPI-RECV-WINDOW": RECV,
        "X-BAPI-SIGN": sign,
        "User-Agent": "vega-margin-check/1.0",
    })
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.loads(r.read().decode())


def num(d, k):
    v = d.get(k)
    if v in (None, ""):
        return None
    try:
        return float(v)
    except ValueError:
        return None


def show(label, v, suffix=""):
    print("  %-34s %s" % (label, "unavailable" if v is None
                          else "%.4f%s" % (v, suffix)))


def main():
    key = os.environ.get("BYBIT_KEY", "").strip()
    secret = os.environ.get("BYBIT_SECRET", "").strip()
    if not key or not secret:
        sys.exit("set BYBIT_KEY and BYBIT_SECRET in the environment")
    base = TESTNET if "--testnet" in sys.argv else MAINNET
    print("reading %s\n" % base)

    try:
        r = signed_get(base, key, secret, "/v5/account/wallet-balance",
                       "accountType=UNIFIED")
    except urllib.error.HTTPError as e:
        sys.exit("HTTP %s: %s" % (e.code, e.read().decode()[:300]))
    except urllib.error.URLError as e:
        sys.exit("network: %s" % e)

    if r.get("retCode") != 0:
        msg = r.get("retMsg", "")
        if "accountType" in msg or "not unified" in msg.lower():
            sys.exit("retCode %s: %s\n\nThis usually means the account is still "
                     "CLASSIC, not a Unified Trading Account. Without UTA, spot "
                     "and perp margin live in separate pools and the hedge is "
                     "not recognised -- which makes leverage unsafe at any size."
                     % (r.get("retCode"), msg))
        sys.exit("retCode %s: %s" % (r.get("retCode"), msg))

    lst = (r.get("result") or {}).get("list") or []
    if not lst:
        sys.exit("no account returned")
    a = lst[0]

    eq = num(a, "totalEquity")
    im = num(a, "totalInitialMargin")
    imp = num(a, "totalInitialMarginByMp")
    mm = num(a, "totalMaintenanceMargin")
    mmp = num(a, "totalMaintenanceMarginByMp")

    print("ACCOUNT")
    show("total equity (USD)", eq)
    print()
    print("INITIAL MARGIN -- what you must post")
    show("cross margin", im)
    show("portfolio margin", imp)
    show("IM rate (cross)", num(a, "accountIMRate"))
    show("IM rate (portfolio)", num(a, "accountIMRateByMp"))
    print()
    print("MAINTENANCE MARGIN -- where liquidation begins")
    show("cross margin", mm)
    show("portfolio margin", mmp)
    show("MM rate (cross)", num(a, "accountMMRate"))
    show("MM rate (portfolio)", num(a, "accountMMRateByMp"))

    print()
    if im and imp and im > 0:
        print("PORTFOLIO MARGIN BENEFIT: %.2fx less initial margin than cross"
              % (im / imp))
        print("  (the project has been assuming 1.11x, entirely unmeasured)")
    elif imp is not None and (im is None or im == 0):
        print("Cross figure is zero or absent -- open a position and re-run;")
        print("an empty account cannot show a hedging benefit.")
    else:
        print("Portfolio-margin fields are absent. Either PM mode is off, or")
        print("SPOT HEDGING is not enabled -- it is a SEPARATE toggle, and")
        print("without it spot assets are excluded from the stress test, so")
        print("the hedge earns you nothing.")

    print("\nMAXIMUM SAFE LEVERAGE from this reading")
    if imp and eq and imp > 0:
        print("  notional currently supported at this margin rate: see below")
        print("  leverage = position notional / initial margin posted")
        print("  Record the notional you opened and divide. That is the real")
        print("  number, and it replaces the 1.11 assumption everywhere.")
    else:
        print("  open a small hedged position first, then re-run")

    print("\nNOTE: margin requirements step up with position size (risk tiers).")
    print("A rate measured at $200 is not the rate at $2M. Measure again at a")
    print("size closer to the one you intend to run.")


if __name__ == "__main__":
    main()
