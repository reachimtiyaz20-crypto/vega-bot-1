#!/usr/bin/env python3
"""How often does funding pay above baseline, for how long, and on what.

On 2026-08-24 every candidate the paper book could trade paid exactly
0.125 bps/hr -- Binance's neutral baseline. That is one instant in a calm
market and says nothing about how often the picture is different.

This reads the journal, which carries funding_rate_pct with interval_hours per
symbol per poll since 5 August, and answers three things:

  1. the distribution of funding across all observations
  2. when a coin pays well, how long that lasts
  3. whether those episodes land on coins we could actually trade

The third question is the one that matters. High funding on a coin we cannot
trade cheaply is a picture of an opportunity, not an opportunity.

Streams line by line and holds only per-symbol episode state, so memory is
O(symbols) rather than O(observations) -- this runs on a 1 vCPU box that is
also running two pollers.

Run from ~/vega-bot:  nice -n 10 python3 funding_dispersion.py
"""

import collections
import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"

# Binance's neutral funding: 0.01% per 8h.
BASELINE_BPS_HR = 0.125

# An episode is a contiguous run at or above this. 4x baseline = 0.5 bps/hr,
# which over a 30-day hold is 360 bps against a ~30 bps round trip.
EPISODE_BPS_HR = 4 * BASELINE_BPS_HR

# A poll gap wider than this ends an episode rather than bridging it: the
# service restarted or the symbol dropped out, and assuming continuity across
# an outage would invent duration that was never observed.
MAX_GAP_MS = 45 * 60 * 1000


def pct(sorted_vals, q):
    """Percentile by nearest rank.

    int() truncation put p90 BELOW the median on a two-element sample during
    testing. Rounding keeps the ordering sane at small n, which is exactly when
    a misreported percentile is most likely to be believed.
    """
    if not sorted_vals:
        return float("nan")
    i = int(round(q * (len(sorted_vals) - 1)))
    return sorted_vals[max(0, min(i, len(sorted_vals) - 1))]


def files():
    out = sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
    out += sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl")))
    return out


def opener(path):
    if path.endswith(".gz"):
        return gzip.open(path, "rt", encoding="utf-8", errors="replace")
    return open(path, "rt", encoding="utf-8", errors="replace")


def main():
    if not os.path.isdir(JOURNAL):
        sys.exit("run from ~/vega-bot (%s not found)" % JOURNAL)

    paths = files()
    if not paths:
        sys.exit("no journal files")
    print("reading %d journal files" % len(paths))

    total = 0
    skipped = 0
    first_ts = last_ts = None
    symbols = set()

    # |bps/hr| histogram
    bands = collections.Counter()
    # observations at or above the episode threshold, split by tradability
    hot_viable = 0
    hot_refused = collections.Counter()
    hot_rates = []

    # per (venue,symbol) episode state
    state = {}
    episodes = []          # (duration_hours, peak_bps_hr, venue, symbol, ever_viable)

    def band_of(x):
        if x < 0.0625:
            return "below half baseline"
        if x < 0.1875:
            return "AT baseline (0.125)"
        if x < 0.5:
            return "1.5x - 4x"
        if x < 1.0:
            return "4x - 8x"
        if x < 2.0:
            return "8x - 16x"
        if x < 5.0:
            return "16x - 40x"
        return "40x+"

    for path in paths:
        with opener(path) as f:
            for line in f:
                if '"obs"' not in line:
                    continue
                try:
                    r = json.loads(line)
                except Exception:
                    skipped += 1
                    continue
                if r.get("type") != "obs":
                    continue

                iv = r.get("interval_hours") or 0
                rate = r.get("funding_rate_pct")
                if not iv or rate is None:
                    skipped += 1
                    continue

                # rate_pct is per interval. 0.01% per 8h -> 0.125 bps/hr.
                bps_hr = rate * 100.0 / iv
                mag = abs(bps_hr)

                total += 1
                ts = r.get("ts_ms") or 0
                if first_ts is None or ts < first_ts:
                    first_ts = ts
                if last_ts is None or ts > last_ts:
                    last_ts = ts

                key = (r.get("venue", "?"), r.get("symbol", "?"))
                symbols.add(key)
                bands[band_of(mag)] += 1

                viable = bool(r.get("viable_30d"))
                if mag >= EPISODE_BPS_HR:
                    hot_rates.append(mag)
                    if viable:
                        hot_viable += 1
                    else:
                        hot_refused[r.get("gate_30d") or "(none)"] += 1

                st = state.get(key)
                if mag >= EPISODE_BPS_HR:
                    if st is None or ts - st["last"] > MAX_GAP_MS:
                        # New episode. A gap wider than MAX_GAP_MS is a hole in
                        # the record, not continuity.
                        if st is not None:
                            episodes.append((
                                (st["last"] - st["start"]) / 3600000.0,
                                st["peak"], key[0], key[1], st["viable"]))
                        state[key] = {"start": ts, "last": ts, "peak": mag,
                                      "viable": viable}
                    else:
                        st["last"] = ts
                        st["peak"] = max(st["peak"], mag)
                        st["viable"] = st["viable"] or viable
                elif st is not None:
                    episodes.append((
                        (st["last"] - st["start"]) / 3600000.0,
                        st["peak"], key[0], key[1], st["viable"]))
                    del state[key]

    # Close whatever is still open at the end of the record.
    for key, st in state.items():
        episodes.append(((st["last"] - st["start"]) / 3600000.0,
                         st["peak"], key[0], key[1], st["viable"]))

    days = (last_ts - first_ts) / 86400000.0 if first_ts and last_ts else 0
    print("\n%d observations, %d venue-symbols, %.1f days, %d unparseable"
          % (total, len(symbols), days, skipped))

    print("\nFUNDING MAGNITUDE, all observations")
    order = ["below half baseline", "AT baseline (0.125)", "1.5x - 4x",
             "4x - 8x", "8x - 16x", "16x - 40x", "40x+"]
    for b in order:
        n = bands[b]
        if n:
            print("  %-22s %8d  %5.1f%%" % (b, n, 100.0 * n / max(total, 1)))

    hot = len(hot_rates)
    print("\nAT OR ABOVE %.2f bps/hr (4x baseline): %d observations, %.1f%%"
          % (EPISODE_BPS_HR, hot, 100.0 * hot / max(total, 1)))
    if hot:
        print("  median %.3f bps/hr, p90 %.3f, max %.3f"
              % (statistics.median(hot_rates),
                 pct(sorted(hot_rates), 0.9), max(hot_rates)))
        print("  tradeable (viable_30d):   %d  (%.1f%%)"
              % (hot_viable, 100.0 * hot_viable / hot))
        print("  refused, by gate:")
        for g, n in hot_refused.most_common(8):
            print("    %-24s %6d  %5.1f%%" % (g, n, 100.0 * n / hot))

    print("\nEPISODES above %.2f bps/hr: %d" % (EPISODE_BPS_HR, len(episodes)))
    if episodes:
        durs = sorted(e[0] for e in episodes)
        print("  duration hours: median %.1f, p90 %.1f, max %.1f"
              % (statistics.median(durs), pct(durs, 0.9), durs[-1]))
        trade = [e for e in episodes if e[4]]
        print("  episodes on a coin that was EVER viable: %d (%.1f%%)"
              % (len(trade), 100.0 * len(trade) / len(episodes)))
        if trade:
            td = sorted(e[0] for e in trade)
            print("  their duration: median %.1f h, p90 %.1f h, max %.1f h"
                  % (statistics.median(td), pct(td, 0.9), td[-1]))
            print("\n  LONGEST TRADEABLE EPISODES")
            for d, peak, venue, sym, _ in sorted(trade, reverse=True)[:12]:
                print("    %-9s %-14s %7.1f h  peak %6.2f bps/hr  (%.0f%% annualised)"
                      % (venue, sym, d, peak, peak * 24 * 365 / 100.0))

    print("\nNOTE: viable_30d before 2026-08-24 was computed with cost_bps")
    print("measured at $400/leg. The book now trades $50, where cost is about")
    print("0.78x that -- so historical viability is understated, not overstated.")


if __name__ == "__main__":
    main()
