#!/usr/bin/env python3
"""What does price do while you are holding to collect the funding payment?

THE MISSING TERM

Settlement sniping looked good on gross payment: 4.44% of settlements clear a
Bybit round trip, and when they clear the median net is 13.8 bps. Three windows
a day, several positions each, and the annualised figure comes out absurd --
which usually means a term is missing.

It is. Our journal polls every five minutes; sniping lives inside a one-minute
window around the funding timestamp. If you are short an alt unhedged while
waiting to collect, price moves. On the thin coins where funding is richest,
that move can easily exceed the 13.8 bps you are collecting.

This measures it directly. Binance publishes 1-minute klines and the funding
history gives the exact settlement timestamps, so the two can be lined up.

WHAT IS MEASURED, per rich settlement

	the payment            what actually changed hands, in bps
	price move             from one minute before settlement to one minute
	                       after, SIGNED against a short: price up hurts
	worst case in window   high-to-low across the window, the wick you could
	                       be filled on
	net                    payment minus fees minus the adverse move

Only settlements that were RICH enough to be worth sniping are examined --
above one round trip -- because that is the only population the strategy would
ever touch. Measuring the calm ones would flatter it.

WHAT THIS STILL CANNOT SEE

Whether your order fills at all. Liquidity thins exactly at settlement and
everyone attempting this is trying at the same instant. A 1-minute kline shows
what traded, not what you would have got.

And it assumes an UNHEDGED short. Hedging with spot removes the price risk and
replaces it with two more legs of fees plus the spot leg's own slippage --
worth measuring separately if this survives.

Run from ~/vega-bot:  python3 settlement_risk.py
"""

import json
import statistics
import sys
import time
import urllib.error
import urllib.request

FAPI = "https://fapi.binance.com"
DAYS = 30
# Symbols that showed the most rich settlements in the journal.
COINS = ["HOMEUSDT", "BICOUSDT", "ACEUSDT", "KAITOUSDT", "COTIUSDT",
         "ONTUSDT", "MOVEUSDT", "STORJUSDT", "ONGUSDT", "BMTUSDT"]
ROUND_TRIP_BPS = 11.0        # two perp round trips at Bybit taker
RICH_BPS = 11.0              # only settlements worth sniping


def get(url, timeout=25):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-snipe/1.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def funding(symbol, start_ms):
    out, cur = [], start_ms
    for _ in range(10):
        try:
            rows = get("%s/fapi/v1/fundingRate?symbol=%s&startTime=%d&limit=1000"
                       % (FAPI, symbol, cur))
        except Exception:
            break
        if not rows:
            break
        out += [(int(r["fundingTime"]), float(r["fundingRate"]) * 10000.0)
                for r in rows]
        last = int(rows[-1]["fundingTime"])
        if last <= cur:
            break
        cur = last + 1
        time.sleep(0.15)
        if len(rows) < 1000:
            break
    return out


def klines(symbol, start_ms, end_ms):
    """1-minute bars: {open_ms: (open, high, low, close)}"""
    out = {}
    cur = start_ms
    for _ in range(20):
        if cur >= end_ms:
            break
        try:
            rows = get("%s/fapi/v1/klines?symbol=%s&interval=1m"
                       "&startTime=%d&endTime=%d&limit=1000"
                       % (FAPI, symbol, cur, end_ms))
        except Exception:
            break
        if not rows:
            break
        for r in rows:
            out[int(r[0])] = (float(r[1]), float(r[2]), float(r[3]), float(r[4]))
        last = int(rows[-1][0])
        if last <= cur:
            break
        cur = last + 60000
        time.sleep(0.15)
        if len(rows) < 1000:
            break
    return out


def main():
    now = int(time.time() * 1000)
    start = now - DAYS * 86400 * 1000
    print("measuring price risk in the settlement window, %d days, %d symbols\n"
          % (DAYS, len(COINS)))

    all_pay, all_move, all_worst, all_net = [], [], [], []
    per_coin = {}

    for c in COINS:
        fr = funding(c, start)
        rich = [(t, r, abs(r)) for t, r in fr if abs(r) >= RICH_BPS]
        if len(rich) < 5:
            print("  %-12s %d rich settlements -- skipped" % (c, len(rich)))
            continue
        kl = klines(c, start, now)
        if len(kl) < 500:
            print("  %-12s klines unavailable" % c)
            continue

        pays, moves, worsts, nets = [], [], [], []
        for t, signed, pay in rich:
            m = (t // 60000) * 60000          # the settlement minute
            before = kl.get(m - 60000)
            during = kl.get(m)
            after = kl.get(m + 60000)
            if not (before and during and after):
                continue
            entry = before[3]                  # close of the prior minute
            exit_ = after[3]                   # close of the following minute
            if entry <= 0:
                continue

            # WHICH SIDE COLLECTS
            #
            # funding positive -> longs pay shorts -> collect by being SHORT
            # funding negative -> shorts pay longs -> collect by being LONG
            #
            # The first version of this assumed SHORT regardless of sign, on a
            # sample of coins whose funding is overwhelmingly negative. It
            # therefore measured the price move for a position nobody would
            # hold, and reported a headwind as a tailwind: +39.5 median net
            # where the correct side gives -17.7.
            side = -1 if signed > 0 else +1    # -1 short, +1 long

            raw = (exit_ - entry) / entry * 10000.0
            # Signed so POSITIVE always means the move HURT the position held.
            move = -raw * side

            hi, lo = max(during[1], after[1]), min(during[2], after[2])
            if side < 0:
                worst = (hi - entry) / entry * 10000.0     # short: spike up
            else:
                worst = (entry - lo) / entry * 10000.0     # long: spike down

            net = pay - ROUND_TRIP_BPS - move
            pays.append(pay)
            moves.append(move)
            worsts.append(worst)
            nets.append(net)

        if len(nets) < 5:
            print("  %-12s too few aligned windows" % c)
            continue
        per_coin[c] = (pays, moves, worsts, nets)
        all_pay += pays
        all_move += moves
        all_worst += worsts
        all_net += nets
        print("  %-12s %4d rich settlements  median pay %6.1f  "
              "median move %+6.1f  median net %+6.1f"
              % (c, len(nets), statistics.median(pays),
                 statistics.median(moves), statistics.median(nets)))

    if not all_net:
        sys.exit("\nno aligned windows -- klines or funding unavailable")

    def pct(v, q):
        s = sorted(v)
        return s[min(int(q * (len(s) - 1)), len(s) - 1)]

    print("\n" + "=" * 66)
    print("ACROSS %d RICH SETTLEMENTS" % len(all_net))
    print("\nTHE PAYMENT (bps, what you are collecting)")
    print("  median %6.1f   p90 %6.1f   max %6.1f"
          % (statistics.median(all_pay), pct(all_pay, 0.9), max(all_pay)))

    print("\nPRICE MOVE AGAINST THE SIDE ACTUALLY HELD (bps, positive = hurts)")
    print("  short when funding is positive, long when it is negative")
    print("  median %+6.1f   mean %+6.1f" % (statistics.median(all_move),
                                             statistics.mean(all_move)))
    print("  p10 %+6.1f   p90 %+6.1f   worst %+6.1f"
          % (pct(all_move, 0.1), pct(all_move, 0.9), max(all_move)))
    sd = statistics.pstdev(all_move) if len(all_move) > 1 else 0
    print("  standard deviation %.1f bps" % sd)

    print("\nWORST WICK IN THE WINDOW (bps against a short)")
    print("  median %+6.1f   p90 %+6.1f   max %+6.1f"
          % (statistics.median(all_worst), pct(all_worst, 0.9), max(all_worst)))

    print("\nNET PER SNIPE (payment - %.0f bps fees - price move)" % ROUND_TRIP_BPS)
    print("  median %+6.1f   mean %+6.1f" % (statistics.median(all_net),
                                             statistics.mean(all_net)))
    print("  p10 %+6.1f   p90 %+6.1f" % (pct(all_net, 0.1), pct(all_net, 0.9)))
    winners = sum(1 for x in all_net if x > 0)
    print("  profitable: %d of %d (%.0f%%)"
          % (winners, len(all_net), 100.0 * winners / len(all_net)))

    print("\nWHAT DECIDES IT")
    print("  If the median net is comfortably positive and the price move is")
    print("  small relative to the payment, the idea survives and the question")
    print("  becomes execution -- can you fill at settlement at all.")
    print("\n  If the standard deviation of the price move is comparable to or")
    print("  larger than the payment, you are not collecting funding, you are")
    print("  taking a coin flip and paying fees for the privilege. That would")
    print("  explain why the rate is not arbitraged away despite looking free.")
    print("\n  Mean matters more than median here: the strategy is repeated")
    print("  many times, so it lives on the average, and a fat adverse tail")
    print("  can make a healthy median worthless.")


if __name__ == "__main__":
    main()
