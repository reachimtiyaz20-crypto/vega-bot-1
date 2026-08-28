#!/usr/bin/env python3
"""The last unmeasured way this strategy dies.

WHY BASIS, AND WHY NOW

Five years of settled funding say the majors book returns 22.8%/yr at 5x and
37.8% at 8x with the de-risk rule. Every one of those figures assumes the hedge
HOLDS. It measures what funding paid and says nothing about whether the two
legs stayed together while it was being paid.

A cash-and-carry is long spot and short perpetual. Its profit and loss from
price is:

    pnl = -(basis_now - basis_entry) x notional,  basis = (perp - spot) / spot

Price itself cancels -- that is the point of the hedge. What does not cancel is
the BASIS. If the perpetual richens against spot while you are short it, you
lose, and you lose on NOTIONAL, which is where leverage bites:

    at  5x   capital is 20.0% of notional -> a 20.0% basis move is total loss
    at  8x   capital is 12.5% of notional -> 12.5% ends it
    at 10x   capital is 10.0% of notional -> 10.0% ends it

Funding cannot save you from this. It accrues in basis points per hour while a
dislocation moves in whole percent in minutes.

WHAT IS MEASURED

  1. Daily close-to-close basis across five years: the ordinary distribution.
  2. The worst ADVERSE EXCURSION inside any 30-day window -- the actual hold
     period -- because entry basis is what you are marked against, not zero.
  3. Hourly basis through LUNA, 3AC, FTX and the USDC depeg, where daily
     closes hide everything that matters.
  4. An intraday OUTER BOUND from perp high against spot low. Those two
     extremes are not simultaneous, so this overstates the true worst case --
     it is a ceiling on the damage, deliberately, because a floor would be
     the wrong error to make here.

WHAT IT STILL WILL NOT TELL YOU

Exchange mark-price mechanics differ from raw last-trade prices, and
liquidation runs off the mark, not off this. Venues also cap and smooth marks
precisely to prevent wick liquidations. So a dislocation appearing here may not
have liquidated in practice -- and conversely, an exchange that halts trading
denies you the exit regardless of what the numbers did.

Run from ~/vega-bot:  python3 basis_risk.py
"""

import json
import statistics
import sys
import time
import urllib.error
import urllib.request

SPOT = "https://api.binance.com/api/v3/klines"
PERP = "https://fapi.binance.com/fapi/v1/klines"
COINS = ["BTC", "ETH", "SOL", "BNB", "XRP", "DOGE"]
YEARS = 5
HOLD_DAYS = 30

CRISES = [
    ("2022-05-05", "2022-05-20", "LUNA / UST"),
    ("2022-06-10", "2022-06-25", "3AC / Celsius"),
    ("2022-11-05", "2022-11-25", "FTX"),
    ("2023-03-08", "2023-03-18", "USDC depeg"),
]


def http(url):
    req = urllib.request.Request(url, headers={"User-Agent": "vega/1.0"})
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.loads(r.read().decode())


def klines(base, symbol, interval, start_ms, end_ms):
    out, cur = [], start_ms
    for _ in range(60):
        if cur >= end_ms:
            break
        try:
            rows = http("%s?symbol=%s&interval=%s&startTime=%d&limit=1000"
                        % (base, symbol, interval, cur))
        except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError):
            break
        if not rows:
            break
        for r in rows:
            # openTime, open, high, low, close
            out.append((int(r[0]), float(r[2]), float(r[3]), float(r[4])))
        last = int(rows[-1][0])
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.15)
        if len(rows) < 1000:
            break
    return out


def basis_series(coin, interval, start_ms, end_ms):
    """Return {ts: (basis_close_bps, basis_outer_bps)}.

    close basis  = (perp close - spot close) / spot close
    outer basis  = (perp HIGH - spot LOW) / spot LOW -- an overstatement, used
                   as a ceiling on intraday damage rather than an estimate.
    """
    sym = coin + "USDT"
    sp = {t: (h, l, c) for t, h, l, c in klines(SPOT, sym, interval, start_ms, end_ms)}
    pp = {t: (h, l, c) for t, h, l, c in klines(PERP, sym, interval, start_ms, end_ms)}
    out = {}
    for t in sorted(set(sp) & set(pp)):
        sh, sl, sc = sp[t]
        ph, pl, pc = pp[t]
        if sc <= 0 or sl <= 0:
            continue
        out[t] = ((pc - sc) / sc * 10000.0, (ph - sl) / sl * 10000.0)
    return out


def pct(v, q):
    if not v:
        return float("nan")
    s = sorted(v)
    i = int(round(q * (len(s) - 1)))
    return s[max(0, min(i, len(s) - 1))]


def main():
    now = int(time.time() * 1000)
    start = now - YEARS * 365 * 86400 * 1000

    print("fetching daily spot and perp prices, %d coins, %d years\n"
          % (len(COINS), YEARS))

    worst_overall = []
    for coin in COINS:
        s = basis_series(coin, "1d", start, now)
        if len(s) < 300:
            print("  %-6s insufficient history (%d days)" % (coin, len(s)))
            continue
        ts = sorted(s)
        close = [s[t][0] for t in ts]

        # ADVERSE EXCURSION inside a 30-day hold. You are marked against the
        # basis at ENTRY, so what matters is how far it can richen against you
        # after you are already in, not its absolute level.
        worst = 0.0
        worst_at = None
        for i in range(len(ts)):
            entry = close[i]
            hi = max(close[i:i + HOLD_DAYS]) if i + 1 < len(ts) else entry
            move = hi - entry          # positive = perp richened = we lose
            if move > worst:
                worst = move
                worst_at = time.strftime("%Y-%m-%d", time.gmtime(ts[i] / 1000))
        worst_overall.append((worst, coin, worst_at))

        print("  %-6s %5d days | basis bps: median %+7.1f  p1 %+8.1f  p99 %+8.1f"
              "  min %+9.1f  max %+9.1f"
              % (coin, len(ts), statistics.median(close), pct(close, 0.01),
                 pct(close, 0.99), min(close), max(close)))
        print("         worst adverse excursion in any %dd window: %+.0f bps"
              " (%.2f%%) entering %s"
              % (HOLD_DAYS, worst, worst / 100.0, worst_at))

    if not worst_overall:
        sys.exit("no usable history")

    print("\n" + "=" * 66)
    w, coin, when = max(worst_overall)
    print("WORST ADVERSE BASIS EXCURSION OBSERVED: %+.0f bps (%.2f%%) on %s, %s"
          % (w, w / 100.0, coin, when))
    print("\nleverage    capital as %% of notional    survives %.2f%% move?" % (w / 100.0))
    for lev in (3, 5, 8, 10, 15):
        capital_pct = 100.0 / lev
        ok = "yes" if capital_pct > w / 100.0 else "NO -- LIQUIDATED"
        print("  %2dx                    %5.1f%%              %s" % (lev, capital_pct, ok))

    print("\n" + "=" * 66)
    print("HOURLY BASIS THROUGH EACH CRISIS (close basis, and intraday ceiling)\n")
    for a, b, label in CRISES:
        s_ms = int(time.mktime(time.strptime(a, "%Y-%m-%d")) * 1000)
        e_ms = int(time.mktime(time.strptime(b, "%Y-%m-%d")) * 1000)
        print("%s  (%s to %s)" % (label, a, b))
        for coin in ("BTC", "ETH"):
            s = basis_series(coin, "1h", s_ms, e_ms)
            if not s:
                print("    %-5s no data" % coin)
                continue
            cl = [v[0] for v in s.values()]
            ou = [v[1] for v in s.values()]
            print("    %-5s %4d hours | close basis min %+8.1f max %+8.1f bps"
                  "  | intraday ceiling %+8.1f bps (%.2f%%)"
                  % (coin, len(s), min(cl), max(cl), max(ou), max(ou) / 100.0))
        print()

    print("READING THIS")
    print("  The excursion number is what decides leverage. Funding accrues in")
    print("  basis points per hour; a dislocation moves in whole percent in")
    print("  minutes, and no amount of carry offsets it.")
    print("  Liquidation runs off exchange MARK price, which is smoothed and")
    print("  capped precisely to prevent wick liquidations -- so a dislocation")
    print("  visible here may not have liquidated in practice. Equally, a venue")
    print("  that halts trading denies the exit whatever the prices did.")


if __name__ == "__main__":
    main()
