#!/usr/bin/env python3
"""Does Bybit portfolio margin net a hedged pair? Read-only, no order code."""

import hashlib, hmac, json, os, time
import urllib.error, urllib.parse, urllib.request

ENV = "/root/.bybit-main.env"
BASE = "https://api.bybit.com"
RECV = "20000"


def load_creds():
    if not os.path.exists(ENV):
        raise SystemExit("no credentials at %s" % ENV)
    kv = {}
    for line in open(ENV):
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        kv[k.strip().upper()] = v.strip().strip('"').strip("'")
    key = next((kv[k] for k in ("BYBIT_API_KEY", "BYBIT_KEY", "API_KEY",
                                "BYBIT_MAIN_KEY") if k in kv), None)
    sec = next((kv[k] for k in ("BYBIT_API_SECRET", "BYBIT_SECRET",
                                "API_SECRET", "BYBIT_MAIN_SECRET") if k in kv), None)
    if not key or not sec:
        raise SystemExit("no key/secret found. keys present: %s" % ", ".join(sorted(kv)))
    return key, sec


def get(path, params, key, sec):
    qs = urllib.parse.urlencode(params) if params else ""
    ts = str(int(time.time() * 1000))
    sign = hmac.new(sec.encode(), (ts + key + RECV + qs).encode(),
                    hashlib.sha256).hexdigest()
    url = BASE + path + (("?" + qs) if qs else "")
    req = urllib.request.Request(url, headers={
        "X-BAPI-API-KEY": key, "X-BAPI-TIMESTAMP": ts,
        "X-BAPI-RECV-WINDOW": RECV, "X-BAPI-SIGN": sign,
        "User-Agent": "vega-pm/1.0"})
    try:
        with urllib.request.urlopen(req, timeout=25) as r:
            return json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return {"retCode": e.code, "retMsg": e.read().decode()[:300]}
    except Exception as e:
        return {"retCode": -1, "retMsg": str(e)}


def f(x, d=0.0):
    try:
        return float(x)
    except (TypeError, ValueError):
        return d


def main():
    key, sec = load_creds()
    print("credentials loaded (not shown)\n")

    print("=" * 70)
    print("1. ACCOUNT CONFIGURATION")
    print("=" * 70)
    a = get("/v5/account/info", {}, key, sec)
    if a.get("retCode") != 0:
        print("  ERROR %s: %s" % (a.get("retCode"), a.get("retMsg")))
        if str(a.get("retCode")) == "10024":
            print("\n  10024 is the jurisdiction block. Hard stop, not a bug.")
        return
    r = a.get("result") or {}
    names = {1: "classic", 3: "UTA 1.0", 4: "UTA 1.0 Pro",
             5: "UTA 2.0", 6: "UTA 2.0 Pro"}
    mm = r.get("marginMode")
    print("  account       %s" % names.get(r.get("unifiedMarginStatus"),
                                           "unknown (%s)" % r.get("unifiedMarginStatus")))
    print("  margin mode   %s" % mm)
    print("  spot hedging  %s" % r.get("spotHedgingStatus"))
    if mm != "PORTFOLIO_MARGIN":
        print("\n  PORTFOLIO MARGIN IS NOT ON -- nothing below tests netting.")

    print("\n" + "=" * 70)
    print("2. MARGIN AND EQUITY")
    print("=" * 70)
    w = get("/v5/account/wallet-balance", {"accountType": "UNIFIED"}, key, sec)
    if w.get("retCode") != 0:
        print("  ERROR %s: %s" % (w.get("retCode"), w.get("retMsg")))
        return
    lst = (w.get("result") or {}).get("list") or []
    if not lst:
        print("  no UNIFIED account returned")
        return
    acc = lst[0]
    im = f(acc.get("totalInitialMargin"))
    print("  total equity              %12.2f" % f(acc.get("totalEquity")))
    print("  total initial margin      %12.2f" % im)
    print("  total maintenance margin  %12.2f" % f(acc.get("totalMaintenanceMargin")))
    for fld in ("totalInitialMarginByMp", "totalMaintenanceMarginByMp",
                "accountIMRate", "accountMMRate", "accountLTV"):
        if acc.get(fld) not in (None, ""):
            print("  %-25s %12s" % (fld, acc.get(fld)))

    coins = [c for c in (acc.get("coin") or []) if f(c.get("walletBalance"))]
    if coins:
        print("\n  balances:")
        for c in coins:
            print("    %-8s wallet %12.6f  usd %10.2f"
                  % (c.get("coin"), f(c.get("walletBalance")), f(c.get("usdValue"))))

    print("\n" + "=" * 70)
    print("3. OPEN POSITIONS")
    print("=" * 70)
    perp = 0.0
    p = get("/v5/position/list", {"category": "linear", "settleCoin": "USDT"}, key, sec)
    if p.get("retCode") == 0:
        pos = [x for x in ((p.get("result") or {}).get("list") or []) if f(x.get("size"))]
        if not pos:
            print("  no open perp positions")
        for x in pos:
            v = f(x.get("positionValue"))
            perp += v
            print("  %-12s %-5s value %10.2f  lev %sx"
                  % (x.get("symbol"), x.get("side"), v, x.get("leverage")))
    else:
        print("  ERROR %s: %s" % (p.get("retCode"), p.get("retMsg")))

    spot = sum(abs(f(c.get("usdValue"))) for c in coins
               if c.get("coin") not in ("USDT", "USDC", "USD"))
    print("\n  spot notional  %10.2f" % spot)
    print("  perp notional  %10.2f" % perp)

    print("\n" + "=" * 70)
    print("4. DOES IT NET?")
    print("=" * 70)
    gross = spot + perp
    if gross < 20:
        print("  Nothing to measure. Move ~$200 into the UTA, buy ~$100 spot")
        print("  BTC, short a BTC perp of the SAME notional, then run again.")
        return
    hedged = min(spot, perp)
    ratio = im / gross if gross else 0.0
    print("  gross notional both legs   %10.2f" % gross)
    print("  hedged overlap             %10.2f" % hedged)
    print("  initial margin required    %10.2f" % im)
    print("  margin / gross             %10.1f%%" % (ratio * 100))
    if ratio > 0:
        print("  implied max leverage       %10.1fx" % (1.0 / ratio))
    print("")
    if hedged < 0.3 * gross:
        print("  Legs are not matched -- this does not test netting. Match the")
        print("  notionals and rerun.")
    elif ratio < 0.15:
        print("  NETTING WORKS. Majors book ceiling is the ~19% case.")
    elif ratio < 0.35:
        print("  PARTIAL NETTING. Re-derive the book with THIS number.")
    else:
        print("  NO NETTING. Each leg margined separately, leverage 2-3x,")
        print("  honest ceiling is single digits. Better to know now.")
    print("\n  Caveat: BTC is the friendliest pair. Alts net worse, not better.")


if __name__ == "__main__":
    main()
