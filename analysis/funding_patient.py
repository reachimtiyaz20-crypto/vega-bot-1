#!/usr/bin/env python3
"""Does PATIENCE turn cash-and-carry positive?

funding_realizable.py exited the moment funding dipped below the entry
threshold. Median hold 0.3 hours, 1 of 817 positions profitable. That result is
real but it tests a strategy nobody would run: it is exactly the twitchy exit
that produced 13 stop-losses averaging 1.5-hour holds in the archive, and
exactly what the minimum-hold rule was built to prevent.

This models the strategy VEGA actually implements:

  ENTER   when funding is above ENTER_BPS_HR on a coin that passes the $50 gates
  HOLD    through dips -- a position is not closed because the rate softened
  EXIT    on whichever comes first:
            - collected funding has cleared the round trip plus TARGET_MARGIN
            - the rate INVERTS and stays inverted (the trade is now backwards)
            - MAX_HOLD_H elapses
            - the coin stops being tradeable

Accrual uses the rate observed BEFORE each interval, never after.

Sweeps several entry thresholds, because a patient strategy should prefer a
persistent small rate to a violent brief one, and the old 4x-baseline entry bar
was chosen for a strategy that exits on dips.

Run from ~/vega-bot:  nice -n 10 python3 funding_patient.py
"""

import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"
BASELINE = 0.125

NOTIONAL = 50.0
MIN_TOP_FRACTION = 0.25
MIN_VOL_USD = 1_000_000.0
COST_RATIO_50 = 0.78
CAPITAL_MULTIPLE = 2.0

MAX_GAP_MS = 45 * 60 * 1000

# Exit once the position has cleared its round trip by this much. Taking the
# money at break-even leaves nothing for the errors the paper model omits.
TARGET_MARGIN_BPS = 10.0

# A position is abandoned after this long regardless. Capital sitting in a
# position earning nothing is capital not in the next one.
MAX_HOLD_H = 24.0 * 14

# The rate must be inverted for this long before the position is closed. One
# poll of noise is not a regime change; this is the same reasoning as
# NegativeSettlements counting a streak rather than a single bad reading.
INVERT_TOLERANCE_H = 4.0

# Extended upward: the sweep was still improving at 2.0 on both sides, so the
# optimum is above the range first tested. Position count falls as the bar
# rises, so the trade-off is edge per position against positions available.
ENTRY_LADDER = [0.5, 1.0, 2.0, 4.0, 8.0, 16.0, 32.0]


def pct(v, q):
    if not v:
        return float("nan")
    i = int(round(q * (len(v) - 1)))
    return v[max(0, min(i, len(v) - 1))]


def files():
    return (sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
            + sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl"))))


def opener(p):
    return (gzip.open(p, "rt", encoding="utf-8", errors="replace")
            if p.endswith(".gz")
            else open(p, "rt", encoding="utf-8", errors="replace"))


def tradeable(r, need_touch):
    return (r.get("spot_available") and r.get("liq_measured")
            and (r.get("perp_vol_24h_usd") or 0) >= MIN_VOL_USD
            and (r.get("spot_vol_24h_usd") or 0) >= MIN_VOL_USD
            and (r.get("spot_top_usd") or 0) >= need_touch
            and (r.get("perp_top_usd") or 0) >= need_touch)


def load_observations():
    """Stream the journal into (key, ts, bps_hr, cost50, ok) tuples.

    Held in memory once and reused across every entry threshold: re-reading
    100 MB of gzip seven times would take seven times as long for no benefit.
    """
    need_touch = NOTIONAL * MIN_TOP_FRACTION
    obs = []
    # Intern the (venue, symbol) key. 811k rows each holding their own tuple
    # costs ~60 MB for 868 distinct values, on a 1 GB box already running two
    # services. One shared object per symbol instead.
    keys = {}
    for path in files():
        with opener(path) as f:
            for line in f:
                if '"obs"' not in line:
                    continue
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if r.get("type") != "obs":
                    continue
                iv = r.get("interval_hours") or 0
                rate = r.get("funding_rate_pct")
                if not iv or rate is None:
                    continue
                kk = (r.get("venue"), r.get("symbol"))
                kk = keys.setdefault(kk, kk)
                obs.append((
                    kk,
                    r.get("ts_ms") or 0,
                    rate * 100.0 / iv,
                    (r.get("cost_bps") or 0.0) * COST_RATIO_50,
                    tradeable(r, need_touch),
                ))
    return obs


def load_borrow():
    """Cheapest annual borrow rate per currency, from the latest snapshot.

    SHORT SPOT MEANS BORROWING THE COIN, and that is the dominant cost of the
    negative side. Measured 2026-08-24: BMT borrows at 336%/yr, which over the
    4-hour median hold costs 15.4 bps against a +10.1 bps median net -- the
    trade is underwater before it starts. KAITO at 46% costs 2.1 bps and
    survives easily. Ignoring this was a 50x error on some coins.

    A currency ABSENT from the snapshot is not cheap, it is unavailable: no
    venue offered to lend it. Those positions cannot be opened at all.

    Returns {currency: bps_per_hour}.
    """
    path = os.path.join("data", "borrow", "rates.jsonl")
    try:
        with open(path) as f:
            last = [l for l in f if l.strip()][-1]
    except (OSError, IndexError):
        return {}
    snap = json.loads(last)
    out = {}
    for r in snap.get("rates") or []:
        if not r.get("ok") or not r.get("borrowable"):
            continue
        ccy = r.get("currency")
        ann = r.get("annual_pct")
        if not ccy or ann is None:
            continue
        bps_hr = ann * 100.0 / 8760.0
        if ccy not in out or bps_hr < out[ccy]:
            out[ccy] = bps_hr        # cheapest venue wins
    return out


def base_of(symbol):
    return symbol[:-4] if symbol.endswith("USDT") else symbol


def simulate(obs, enter_bps, side, borrow=None):
    """Run the patient strategy at one entry threshold, one side.

    side +1 trades POSITIVE funding (long spot / short perp, no borrow).
    side -1 trades NEGATIVE funding (short spot, which needs borrow).
    """
    open_pos = {}
    closed = []

    def shut(key, st, why):
        closed.append((st["hours"], st["collected"] - st["cost"], why,
                       key[0], key[1], st["collected"], st["cost"]))

    for key, ts, bps, cost, ok in obs:
        signed = bps * side          # what THIS side earns, signed
        st = open_pos.get(key)

        if st is not None:
            gap_h = (ts - st["last"]) / 3600000.0
            if ts - st["last"] > MAX_GAP_MS:
                shut(key, st, "record gap")
                del open_pos[key]
                st = None
            else:
                # Accrue at the PREVIOUS rate, for the elapsed time.
                st["collected"] += (st["rate"] - st["borrow"]) * gap_h
                st["hours"] += gap_h
                st["last"] = ts
                st["rate"] = signed

                if signed < 0:
                    st["inverted_h"] += gap_h
                else:
                    st["inverted_h"] = 0.0

                if st["collected"] - st["cost"] >= TARGET_MARGIN_BPS:
                    shut(key, st, "target")
                    del open_pos[key]
                    continue
                if st["inverted_h"] >= INVERT_TOLERANCE_H:
                    shut(key, st, "inverted")
                    del open_pos[key]
                    continue
                if st["hours"] >= MAX_HOLD_H:
                    shut(key, st, "max hold")
                    del open_pos[key]
                    continue
                # TRADABILITY IS AN ENTRY TEST, NOT AN EXIT.
                #
                # It was both, and that single mistake produced 16,250 of 16,716
                # positions exiting after a mean 2.7 hours, each paying a full
                # round trip. Top-of-book depth flickers around $12.50 on every
                # poll; nobody unwinds a hedged position because the touch
                # thinned for five minutes. Liquidity matters when deciding
                # whether a position CAN be opened, and again only when it must
                # actually be closed.
                continue

        if st is None and ok and signed >= enter_bps:
            bcost = 0.0
            if side < 0 and borrow is not None:
                b = borrow.get(base_of(key[1]))
                if b is None:
                    # Nobody will lend it. The short-spot leg cannot be built,
                    # so there is no position to open -- not a cheap one.
                    continue
                bcost = b
            open_pos[key] = {"last": ts, "rate": signed, "collected": 0.0,
                             "hours": 0.0, "cost": cost, "inverted_h": 0.0,
                             "borrow": bcost}

    for key, st in open_pos.items():
        shut(key, st, "still open")
    return closed


def report(closed, label):
    if not closed:
        print("  %-8s no positions" % label)
        return
    nets = sorted(c[1] for c in closed)
    hrs = sorted(c[0] for c in closed)
    wins = [n for n in nets if n > 0]
    tot = sum(nets)
    print("  %-8s %5d pos  win %5.1f%%  median net %+7.1f  total %+9.0f bps  "
          "median hold %6.1f h" % (label, len(closed), 100.0 * len(wins) / len(closed),
                                   statistics.median(nets), tot, statistics.median(hrs)))
    return tot


def main():
    if not os.path.isdir(JOURNAL):
        sys.exit("run from ~/vega-bot")

    print("loading journal...")
    obs = load_observations()
    try:
        with open("/proc/self/status") as f:
            rss = [l for l in f if l.startswith("VmRSS")][0].split()[1]
        print("%d observations held, RSS %.0f MB\n" % (len(obs), int(rss) / 1024.0))
    except Exception:
        print("%d observations held in memory\n" % len(obs))

    borrow = load_borrow()
    print("borrow rates for %d currencies (cheapest venue), median %.3f bps/hr\n"
          % (len(borrow),
             statistics.median(sorted(borrow.values())) if borrow else float("nan")))

    print("PATIENT CASH-AND-CARRY AT $%.0f/LEG" % NOTIONAL)
    print("hold through dips; exit at round trip +%.0f bps, on %.0fh of inversion, "
          "or %.0fd\n" % (TARGET_MARGIN_BPS, INVERT_TOLERANCE_H, MAX_HOLD_H / 24))

    for enter in ENTRY_LADDER:
        print("ENTER above %.3f bps/hr  (%.1fx baseline)" % (enter, enter / BASELINE))
        for side, label in ((1, "POSITIVE"), (-1, "NEGATIVE")):
            closed = simulate(obs, enter, side, borrow)
            report(closed, label)
        print()

    # WHICH COINS CARRY THE NEGATIVE SIDE.
    #
    # This is the list that decides what cmd/borrow must cover. Short spot means
    # borrowing the coin, and borrow is priced on four currencies today, none of
    # which are these. Until that is measured, every negative-side figure above
    # omits its largest cost.
    print("=" * 74)
    print("COINS CARRYING THE NEGATIVE SIDE (entry 2.0 bps/hr)")
    print("=" * 74)
    neg = simulate(obs, 2.0, -1, borrow)
    by_coin = {}
    for h, net, why, venue, sym, coll, cost in neg:
        n, tot, hrs = by_coin.get(sym, (0, 0.0, 0.0))
        by_coin[sym] = (n + 1, tot + net, hrs + h)
    ranked = sorted(by_coin.items(), key=lambda kv: -kv[1][1])
    print("  %-14s %6s %12s %10s" % ("COIN", "pos", "total bps", "hours"))
    for sym, (n, tot, hrs) in ranked[:25]:
        print("  %-14s %6d %+12.0f %10.1f" % (sym, n, tot, hrs))
    print("\n  %d distinct coins" % len(by_coin))
    winners = [sym for sym, (n, tot, hrs) in ranked if tot > 0]
    print("  %d with a positive total" % len(winners))
    print("\n  BORROW LIST (paste into vega-borrow.service -currencies):")
    bases = []
    for sym in winners[:40]:
        b = sym[:-4] if sym.endswith("USDT") else sym
        if b not in bases:
            bases.append(b)
    print("  USDT,USDC,BTC,ETH," + ",".join(bases))

    # Detail on the positive side for comparison.
    print("\n" + "=" * 74)
    print("EXIT REASONS, POSITIVE side, entry at 2.0 bps/hr")
    print("=" * 74)
    closed = simulate(obs, 2.0, 1)
    if closed:
        reasons = {}
        for h, net, why, _, _, coll, cost in closed:
            a, b, c = reasons.get(why, (0, 0.0, 0.0))
            reasons[why] = (a + 1, b + net, c + h)
        for why, (n, net, h) in sorted(reasons.items(), key=lambda x: -x[1][0]):
            print("  %-14s %5d positions  total %+9.0f bps  mean hold %6.1f h"
                  % (why, n, net, h / max(n, 1)))

        best = sorted(closed, key=lambda c: -c[1])[:10]
        print("\n  BEST POSITIONS")
        for h, net, why, venue, sym, coll, cost in best:
            print("    %-8s %-13s %7.1f h  collected %8.1f  cost %5.1f  NET %+8.1f  (%s)"
                  % (venue, sym, h, coll, cost, net, why))

        nets = [c[1] for c in closed]
        hrs = [c[0] for c in closed]
        tot, th = sum(nets), sum(hrs)
        print("\n  TOTAL %+.0f bps over %.0f position-hours" % (tot, th))
        if th > 0:
            per = tot / th
            print("  = %+.4f bps/position-hour = %+.1f%%/yr on notional, %+.1f%%/yr on capital"
                  % (per, per * 24 * 365 / 100.0,
                     per * 24 * 365 / 100.0 / CAPITAL_MULTIPLE))

    print("\nCAVEATS: cost scaled %.2f from $400 on 7 pairs; no partial fills, no"
          % COST_RATIO_50)
    print("failed orders, no basis P&L, entry at the first qualifying observation,")
    print("and capital assumed available whenever a position qualifies.")


if __name__ == "__main__":
    main()
