#!/usr/bin/env python3
"""VEGA cross-venue book: short Hyperliquid perp, long CEX spot. PAPER ONLY.

WHAT THIS MEASURES AND WHY IT EXISTS

Every previous lead died at the same wall -- the rate was spectacular and the
trade could not be built. COTI at -109 bps/hr with no lender. STORJ with $5.10
on the touch. Thirty-five new listings, zero spot pairs. AAVE at 16,766% on
empty open interest.

This one cleared every check:

	CASHCAT  positive funding in EVERY hour of 45 days, median 58.8%/yr
	         Hyperliquid open interest $36.4M, daily volume $36.3M
	         spot on MEXC: $6,016 within 25 bps of the ask
	         -> ~$12,000 of capital at ~44%/yr after a 40 bps round trip

	XMR      $99,242 of spot within 25 bps on KuCoin -> ~$198k of capital,
	         but only ~8%/yr. Capacity and return trade against each other,
	         as they have all the way through.

So the question is no longer "does an edge exist" but "does the measured edge
survive contact with settled data". That is what this book is for, and it is
the same job the reverse book did for CEX carry -- where a 23.9% figure at four
closes became 4.6% at seven.

THE STRUCTURE

	short perp on Hyperliquid   collects funding, hourly
	long spot on a CEX          bought with cash -- NO BORROWING, which is
	                            what makes this different from the CEX carry
	                            book where borrow ate the edge
	capital sits on BOTH venues and cannot net

HOW IT ACCOUNTS

	funding accrues at the rate observed BEFORE each interval, never after
	the round trip is charged AT ENTRY, in full, both legs
	a position is not profitable until it has paid to get out
	a failed fetch is UNKNOWN and blocks entry -- never treated as zero

PAPER ONLY. There is no order placement in this file. Adding it should be a
separate, deliberate change.

	python3 hlbook.py --once     one cycle, prints and exits
	python3 hlbook.py            loop forever, for systemd
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

HL = "https://api.hyperliquid.xyz/info"
DATA = "data/hlbook"
STATE = os.path.join(DATA, "state.json")
JOURNAL = os.path.join(DATA, "journal")

# --- policy ---
POSITION_USD = 500.0          # paper notional per position
MAX_POSITIONS = 6
ENTER_PCT_YR = 40.0           # annualised funding to open
EXIT_PCT_YR = 10.0            # close if it decays below this
EXIT_GRACE_H = 12             # ...and stays below for this long
HOLD_DAYS = 7.0
# MEASURED 2026-08-27, no longer assumed: Bybit spot taker 10.00 bps each
# way, Hyperliquid perp taker 4.50 bps each way = 29.00 bps of fees. The
# remaining ~11 bps is a slippage allowance -- depth was measured within 25 bps
# of the ask, and a $500 buy into CASHCAT's $4,859 band spends part of it.
# The original 40 was a guess that happened to land in the right place for the
# wrong reason, which is worth recording rather than quietly keeping.
ROUND_TRIP_BPS = 40.0
MIN_OI_USD = 1_000_000.0
DEPTH_BAND_BPS = 25
POLL_SEC = 900                # 15 minutes

SPOT_VENUES = ["mexc", "kucoin", "gate", "bybit"]


def now_ms():
    return int(time.time() * 1000)


def hl_post(payload, timeout=25):
    req = urllib.request.Request(
        HL, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json",
                 "User-Agent": "vega-hlbook/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def get(url, timeout=12):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-hlbook/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


# ---- spot depth adapters: ASK side, since the spot leg is bought ----

def sp_mexc(c):
    return get("https://api.mexc.com/api/v3/depth?symbol=%sUSDT&limit=500" % c).get("asks")


def sp_kucoin(c):
    d = get("https://api.kucoin.com/api/v1/market/orderbook/level2_100?symbol=%s-USDT" % c)
    return ((d.get("data") or {}).get("asks")) or None


def sp_gate(c):
    return get("https://api.gateio.ws/api/v4/spot/order_book"
               "?currency_pair=%s_USDT&limit=100" % c).get("asks")


def sp_bybit(c):
    d = get("https://api.bybit.com/v5/market/orderbook"
            "?category=spot&symbol=%sUSDT&limit=200" % c)
    return ((d.get("result") or {}).get("a")) or None


ADAPTERS = {"mexc": sp_mexc, "kucoin": sp_kucoin, "gate": sp_gate,
            "bybit": sp_bybit}


def depth_within(levels, bps):
    """USD resting within `bps` of the best ask. None if unreadable."""
    if not levels:
        return None
    try:
        best = float(levels[0][0])
    except (ValueError, IndexError, TypeError):
        return None
    lim = best * (1 + bps / 10000.0)
    tot = 0.0
    for row in levels:
        try:
            p, s = float(row[0]), float(row[1])
        except (ValueError, IndexError, TypeError):
            continue
        if p > lim:
            break
        tot += p * s
    return tot


def best_spot(coin):
    """(venue, usd_depth) for the deepest venue, or (None, None) if unknown.

    Splitting one hedge across venues triples accounts, transfers and
    counterparty exposure for a single position, so only the best is used.
    """
    best_v, best_d = None, 0.0
    seen_any = False
    for v in SPOT_VENUES:
        fn = ADAPTERS.get(v)
        if not fn:
            continue
        try:
            lv = fn(coin)
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError,
                ValueError, KeyError, TypeError):
            continue
        d = depth_within(lv, DEPTH_BAND_BPS)
        if d is None:
            continue
        seen_any = True
        if d > best_d:
            best_v, best_d = v, d
        time.sleep(0.1)
    if not seen_any:
        # Unknown, not zero. Blocks entry rather than permitting it.
        return None, None
    return best_v, best_d


def load_state():
    if not os.path.exists(STATE):
        return {"open": {}, "closed": [], "seq": 0, "started": now_ms()}
    try:
        return json.load(open(STATE))
    except Exception as e:
        # A corrupt state file must not silently become a fresh book.
        sys.exit("state.json unreadable (%s) -- move it aside deliberately" % e)


def save_state(st):
    os.makedirs(DATA, exist_ok=True)
    tmp = STATE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(st, f, indent=1)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, STATE)


def journal(rec):
    os.makedirs(JOURNAL, exist_ok=True)
    day = time.strftime("%Y-%m-%d", time.gmtime())
    with open(os.path.join(JOURNAL, day + ".jsonl"), "a") as f:
        f.write(json.dumps(rec) + "\n")


def market_snapshot():
    """coin -> {funding_pct_yr, oi_usd, px, vol}. Empty dict on failure."""
    try:
        meta, ctxs = hl_post({"type": "metaAndAssetCtxs"})
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError,
            ValueError) as e:
        print("WARN hyperliquid snapshot failed: %s" % e)
        return {}
    out = {}
    for u, c in zip((meta or {}).get("universe") or [], ctxs or []):
        name = u.get("name")
        if not name:
            continue
        try:
            px = float(c.get("markPx") or 0.0)
            out[name] = {
                "f": float(c.get("funding") or 0.0) * 8760.0 * 100.0,
                "oi": float(c.get("openInterest") or 0.0) * px,
                "px": px,
                "vol": float(c.get("dayNtlVlm") or 0.0),
            }
        except (TypeError, ValueError):
            continue
    return out


def cycle(st, verbose=True):
    ts = now_ms()
    mk = market_snapshot()
    if not mk:
        journal({"type": "poll_failed", "ts_ms": ts})
        return

    # ---------- accrue on open positions ----------
    for pid, p in list(st["open"].items()):
        m = mk.get(p["coin"])
        if not m:
            # Market vanished from the snapshot: record, do not accrue.
            p["missing"] = p.get("missing", 0) + 1
            continue
        hrs = (ts - p["last_ms"]) / 3600000.0
        if 0 < hrs <= 6:
            # Accrue at the rate seen BEFORE this interval, never after.
            p["funding_bps"] += p["last_f"] / 8760.0 * 100.0 * hrs
        p["last_ms"] = ts
        p["last_f"] = m["f"]

        held_d = (ts - p["opened_ms"]) / 86400000.0
        net = p["funding_bps"] - p["cost_bps"]

        reason = None
        if held_d >= HOLD_DAYS:
            reason = "planned hold of %g days reached" % HOLD_DAYS
        elif m["f"] < EXIT_PCT_YR:
            p["below_h"] = p.get("below_h", 0.0) + hrs
            if p["below_h"] >= EXIT_GRACE_H:
                reason = ("funding below %.0f%%/yr for %.0fh"
                          % (EXIT_PCT_YR, p["below_h"]))
        else:
            p["below_h"] = 0.0

        if reason:
            p["closed_ms"] = ts
            p["close_reason"] = reason
            p["net_bps"] = net
            p["held_days"] = held_d
            st["closed"].append(p)
            del st["open"][pid]
            journal({"type": "hl_close", "ts_ms": ts, **p})
            if verbose:
                print("CLOSE %-14s net %+8.2f bps after %.1fd  [%s]"
                      % (p["coin"], net, held_d, reason))

    # ---------- look for entries ----------
    if len(st["open"]) < MAX_POSITIONS:
        held = {p["coin"] for p in st["open"].values()}
        cands = [(c, m) for c, m in mk.items()
                 if c not in held and m["f"] >= ENTER_PCT_YR
                 and m["oi"] >= MIN_OI_USD]
        cands.sort(key=lambda kv: -kv[1]["f"])

        for coin, m in cands[:4]:
            if len(st["open"]) >= MAX_POSITIONS:
                break
            venue, depth = best_spot(coin)
            if depth is None:
                journal({"type": "hl_refuse", "ts_ms": ts, "coin": coin,
                         "why": "SPOT_DEPTH_UNKNOWN", "funding": m["f"]})
                continue
            if depth < POSITION_USD:
                journal({"type": "hl_refuse", "ts_ms": ts, "coin": coin,
                         "why": "SPOT_TOO_THIN", "funding": m["f"],
                         "depth_usd": depth, "need": POSITION_USD})
                continue

            st["seq"] += 1
            pid = "%s-%d" % (coin, st["seq"])
            p = {"id": pid, "coin": coin, "spot_venue": venue,
                 "spot_depth_usd": round(depth, 2),
                 "opened_ms": ts, "last_ms": ts, "last_f": m["f"],
                 "entry_f_pct_yr": m["f"], "oi_usd": round(m["oi"], 0),
                 "notional_usd": POSITION_USD,
                 "capital_usd": POSITION_USD * 2,
                 "funding_bps": 0.0, "cost_bps": ROUND_TRIP_BPS,
                 "below_h": 0.0}
            st["open"][pid] = p
            journal({"type": "hl_open", "ts_ms": ts, **p})
            if verbose:
                print("OPEN  %-14s funding %6.1f%%/yr  spot %s $%.0f  OI $%.1fM"
                      % (coin, m["f"], venue, depth, m["oi"] / 1e6))

    # ---------- summary ----------
    open_net = sum(p["funding_bps"] - p["cost_bps"] for p in st["open"].values())
    closed_net = sum(p.get("net_bps", 0.0) for p in st["closed"])
    rec = {"type": "hl_poll", "ts_ms": ts, "open": len(st["open"]),
           "closed": len(st["closed"]),
           "open_net_bps": round(open_net, 2),
           "closed_net_bps": round(closed_net, 2)}
    journal(rec)
    if verbose:
        won = sum(1 for p in st["closed"] if p.get("net_bps", 0) >= 0)
        print("poll: %d open, %d closed (%d won), open %+.1f bps, closed %+.1f bps"
              % (len(st["open"]), len(st["closed"]), won, open_net, closed_net))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--once", action="store_true")
    args = ap.parse_args()

    if not os.path.isdir("data"):
        sys.exit("run from ~/vega-bot")

    print("VEGA hlbook -- PAPER ONLY, no order placement exists in this file")
    print("short Hyperliquid perp / long CEX spot, no borrowing")
    print("policy: enter above %.0f%%/yr, exit below %.0f%% for %dh, "
          "hold %g days, %d max, $%.0f each, %.0f bps round trip"
          % (ENTER_PCT_YR, EXIT_PCT_YR, EXIT_GRACE_H, HOLD_DAYS,
             MAX_POSITIONS, POSITION_USD, ROUND_TRIP_BPS))

    st = load_state()
    print("state: %d open, %d closed\n" % (len(st["open"]), len(st["closed"])))

    if args.once:
        cycle(st)
        save_state(st)
        return

    while True:
        try:
            cycle(st)
            save_state(st)
        except KeyboardInterrupt:
            save_state(st)
            print("\nstopped cleanly")
            return
        except Exception as e:
            print("ERROR in cycle: %s" % e)
        time.sleep(POLL_SEC)


if __name__ == "__main__":
    main()
