#!/usr/bin/env python3
"""Can we actually fill passive orders, and how fast? Measured live, risking nothing.

WHY THIS COMES BEFORE ANY ORDER CODE

Incentive farming is a volume business. Calibrated against Hyperliquid Season 1
-- $548M of tokens against roughly $550B of campaign volume -- the reward was
about 1.0 bps per dollar traded at listing price. Against a 2.5 bps taker fee
that is a LOSS. The whole thing only works at maker fees, and only at high
turnover:

	turnover 10x/month, 1 bps margin  ->   1.2%/yr
	turnover 60x/month, 1 bps margin  ->   7.4%/yr
	turnover 60x/month, 5 bps margin  ->  42.6%/yr

So everything depends on TURNOVER, and turnover depends on whether passive
orders actually fill. Our journal says 89% of them do not fill within five
minutes -- but that is five-minute resolution on a polling loop, which tells us
almost nothing about what a maker experiences second by second.

This measures it properly, and measures it WITHOUT placing a single order.

HOW IT WORKS

Every cycle it records the current best bid and ask, then watches the public
trade tape. A hypothetical passive buy resting at the bid is treated as filled
when a trade prints at or below that price; a passive sell at the ask fills
when a trade prints at or above it. Then it follows the mid for another minute
to see where price went AFTER the fill.

WHAT THIS OVERSTATES, and it matters

Queue position is not modelled. A trade printing at your price does not mean
YOUR order filled -- you are behind everyone already resting there, and on a
liquid venue that queue is deep and fast. So treat every fill rate here as a
CEILING. The real rate for a retail participant 150ms from the matching engine
is lower, possibly much lower.

If the ceiling is already too low to support the turnover the economics need,
the question is closed and no execution engine will rescue it. That is the
cheapest possible way to find out.

ADVERSE SELECTION is the second output and the one people forget. Filling is
not the same as filling profitably: if your passive buys only fill when price
is about to fall, you are being picked off, and the volume you manufacture
costs more than the fee schedule suggests.

Public endpoints only. No API key. No orders. Nothing to lose.

	python3 maker_probe.py --symbol XRPUSDT --minutes 30
"""

import argparse
import collections
import json
import statistics
import sys
import time
import urllib.error
import urllib.request

BASE = "https://api.bybit.com"
HORIZONS = [5, 15, 30, 60, 300]      # seconds
ADVERSE_AFTER = 60                    # seconds to follow price post-fill


def http(path, params):
    q = "&".join("%s=%s" % (k, v) for k, v in params.items())
    req = urllib.request.Request(BASE + path + "?" + q,
                                 headers={"User-Agent": "vega-maker-probe/1.0"})
    with urllib.request.urlopen(req, timeout=8) as r:
        return json.loads(r.read().decode())


def book(symbol, category):
    d = http("/v5/market/orderbook", {"category": category, "symbol": symbol,
                                      "limit": 1})
    r = d.get("result") or {}
    b, a = r.get("b") or [], r.get("a") or []
    if not b or not a:
        return None
    return float(b[0][0]), float(a[0][0]), float(b[0][1]), float(a[0][1])


def trades(symbol, category, limit=50):
    d = http("/v5/market/recent-trade", {"category": category,
                                         "symbol": symbol, "limit": limit})
    out = []
    for t in ((d.get("result") or {}).get("list") or []):
        try:
            out.append((int(t["time"]), float(t["price"]), float(t["size"]),
                        t.get("side")))
        except (KeyError, ValueError):
            continue
    return out


class Quote:
    """A hypothetical passive order, WITH its place in the queue.

    The first version of this filled whenever any trade printed at the quote
    price, which produced a 100% fill rate at a median of zero seconds. That is
    not a measurement -- on a liquid book, sellers hit the bid continuously. It
    measured "does the market trade here", not "would I have filled".

    Joining a resting price means queueing behind everything already there. You
    fill only once enough volume has traded through to clear the size ahead of
    you. That queue is the whole difference between a market maker and someone
    watching one.
    """

    __slots__ = ("side", "price", "born", "queue_ahead", "cleared",
                 "filled_at", "mid_at_fill")

    def __init__(self, side, price, born, queue_ahead):
        self.side = side
        self.price = price
        self.born = born
        self.queue_ahead = max(queue_ahead, 0.0)
        self.cleared = 0.0
        self.filled_at = None
        self.mid_at_fill = None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--symbol", default="XRPUSDT")
    ap.add_argument("--category", default="linear",
                    help="linear (perp) or spot")
    ap.add_argument("--minutes", type=float, default=30)
    ap.add_argument("--interval", type=float, default=1.0,
                    help="seconds between polls")
    ap.add_argument("--quote-every", type=float, default=10.0,
                    help="seconds between new hypothetical quotes")
    args = ap.parse_args()

    print("probing %s (%s) for %.0f minutes, polling every %.1fs"
          % (args.symbol, args.category, args.minutes, args.interval))
    print("no API key, no orders placed, nothing at risk\n")

    end = time.time() + args.minutes * 60
    live = []          # unfilled quotes
    done = []          # filled quotes awaiting adverse-selection follow-up
    settled = []       # fully measured
    expired = []
    seen_trades = set()
    last_quote = 0.0
    spreads = []
    polls = 0
    errors = 0

    while time.time() < end:
        now = time.time()
        try:
            bk = book(args.symbol, args.category)
            tr = trades(args.symbol, args.category)
            polls += 1
        except (urllib.error.URLError, urllib.error.HTTPError,
                TimeoutError, ValueError) as e:
            errors += 1
            if errors > 20:
                sys.exit("too many errors: %s" % e)
            time.sleep(args.interval)
            continue

        if not bk:
            time.sleep(args.interval)
            continue
        bid, ask, bsz, asz = bk
        mid = (bid + ask) / 2.0
        spreads.append((ask - bid) / mid * 10000.0)

        # New hypothetical quotes at the touch, both sides.
        if now - last_quote >= args.quote_every:
            # Joining the touch means queueing behind the size already resting.
            live.append(Quote("buy", bid, now, bsz))
            live.append(Quote("sell", ask, now, asz))
            last_quote = now

        # Fill detection from the public tape.
        fresh = [t for t in tr if (t[0], t[1], t[2]) not in seen_trades]
        for t in fresh:
            seen_trades.add((t[0], t[1], t[2]))
        if len(seen_trades) > 20000:
            seen_trades.clear()

        for q in list(live):
            hit = False
            for _, px, sz, _ in fresh:
                if q.side == "buy":
                    if px < q.price:
                        # Price traded THROUGH our level: everything resting
                        # there, including us, is gone.
                        hit = True
                        break
                    if px == q.price:
                        # Trading AT our level consumes the queue in order.
                        q.cleared += sz
                        if q.cleared >= q.queue_ahead:
                            hit = True
                            break
                else:
                    if px > q.price:
                        hit = True
                        break
                    if px == q.price:
                        q.cleared += sz
                        if q.cleared >= q.queue_ahead:
                            hit = True
                            break
            if hit:
                q.filled_at = now
                q.mid_at_fill = mid
                done.append(q)
                live.remove(q)
            elif now - q.born > max(HORIZONS):
                expired.append(q)
                live.remove(q)

        # Adverse selection: where did mid go after we filled?
        for q in list(done):
            if now - q.filled_at >= ADVERSE_AFTER:
                # Positive = price moved AGAINST the fill.
                if q.side == "buy":
                    adv = (q.mid_at_fill - mid) / mid * 10000.0
                else:
                    adv = (mid - q.mid_at_fill) / mid * 10000.0
                settled.append((q, adv))
                done.remove(q)

        time.sleep(args.interval)

    total = len(settled) + len(done) + len(expired)
    if not total:
        sys.exit("no quotes measured -- try a longer run")

    filled = len(settled) + len(done)
    print("=" * 62)
    print("%d polls, %d errors, %d hypothetical quotes" % (polls, errors, total))
    print("median spread: %.2f bps" % statistics.median(spreads))
    print("\nFILL RATE (CEILING -- queue position not modelled)")
    print("  filled at all: %d of %d  (%.1f%%)"
          % (filled, total, 100.0 * filled / total))

    times = [q.filled_at - q.born for q, _ in settled]
    times += [q.filled_at - q.born for q in done]
    for h in HORIZONS:
        n = sum(1 for t in times if t <= h)
        print("  within %4ds: %5.1f%%" % (h, 100.0 * n / total))
    if times:
        print("  median time to fill: %.1fs" % statistics.median(times))

    if settled:
        advs = [a for _, a in settled]
        print("\nADVERSE SELECTION (%ds after fill, positive = moved against us)"
              % ADVERSE_AFTER)
        print("  mean %+.2f bps   median %+.2f bps   p90 %+.2f bps"
              % (statistics.mean(advs), statistics.median(advs),
                 sorted(advs)[int(0.9 * (len(advs) - 1))]))
        half = statistics.median(spreads) / 2.0
        net = half - statistics.median(advs)
        print("  half-spread captured %.2f bps, less adverse move = %+.2f bps net"
              % (half, net))
        if net <= 0:
            print("  NEGATIVE: passive fills are being picked off. Manufacturing")
            print("  volume this way costs money on top of any fee.")

    hrs = args.minutes / 60.0
    fills_hr = filled / hrs if hrs else 0
    print("\nTURNOVER IMPLICATION")
    print("  %.1f fills per hour per quoted pair" % fills_hr)
    print("  at one unit of capital per quote, that is roughly %.0fx per month"
          % (fills_hr * 24 * 30))
    print("  the economics need 60x/month to reach 40%/yr at 5 bps margin,")
    print("  and this figure is a CEILING before queue position.")


if __name__ == "__main__":
    main()
