#!/usr/bin/env python3
"""Re-ask the dispersion question at the size we now trade.

funding_dispersion.py used the journal's viable_30d flag. Every one of those
was computed against -notional 400, where MinTopOfBookFraction 0.25 demanded
$100 at the touch. At $50 it demands $12.50 -- an eight-fold easier test. The
thinperp and thinspot refusals that accounted for 91.5% of hot observations
were measured against a bar we no longer impose, so that result understated
tradability by an unknown and probably large margin.

Nothing needs re-collecting. The journal stores spot_top_usd, perp_top_usd,
both 24h volumes and cost_bps on every observation, so viability at $50 can be
recomputed from what is already on disk.

ONE APPROXIMATION, STATED PLAINLY: cost_bps was measured at $400/leg. It is
scaled by COST_RATIO_50, the median $50/$400 ratio from seven venue pairs
measured 2026-08-24. Seven pairs is thin, and this figure should be replaced
with the direct measurement once the sampler has $50 coverage. Every number
below inherits that uncertainty.

Run from ~/vega-bot:  nice -n 10 python3 funding_at_50.py
"""

import collections
import glob
import gzip
import json
import os
import statistics
import sys

JOURNAL = "data/journal"

BASELINE_BPS_HR = 0.125
EPISODE_BPS_HR = 4 * BASELINE_BPS_HR
MAX_GAP_MS = 45 * 60 * 1000

# What the book is actually configured to do now.
NOTIONAL = 50.0
MIN_TOP_FRACTION = 0.25          # -min-depth
MIN_VOL_USD = 1_000_000.0        # -min-vol, lowered from 10M on 2026-08-24
HOLD_DAYS = 30.0                 # -hold-long

# Measured 2026-08-24 across 7 venue pairs: median cost at $50 was 0.78x the
# cost at $400. Thin evidence, and the single largest uncertainty here.
COST_RATIO_50 = 0.78


def pct(vals, q):
    if not vals:
        return float("nan")
    i = int(round(q * (len(vals) - 1)))
    return vals[max(0, min(i, len(vals) - 1))]


def files():
    return (sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl.gz")))
            + sorted(glob.glob(os.path.join(JOURNAL, "*.jsonl"))))


def opener(path):
    if path.endswith(".gz"):
        return gzip.open(path, "rt", encoding="utf-8", errors="replace")
    return open(path, "rt", encoding="utf-8", errors="replace")


def main():
    if not os.path.isdir(JOURNAL):
        sys.exit("run from ~/vega-bot")

    need_touch = NOTIONAL * MIN_TOP_FRACTION

    total = hot = 0
    hot_pass = 0
    hot_fail = collections.Counter()
    hot_pass_rates = []
    net_bps_pass = []

    state = {}
    episodes = []

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

                total += 1
                bps_hr = rate * 100.0 / iv
                mag = abs(bps_hr)
                if mag < EPISODE_BPS_HR:
                    key = (r.get("venue"), r.get("symbol"))
                    st = state.pop(key, None)
                    if st:
                        episodes.append(((st["last"] - st["start"]) / 3600000.0,
                                         st["peak"], key[0], key[1], st["ok"],
                                         st["net"]))
                    continue

                hot += 1

                # Re-apply the CURRENT gates at the CURRENT size.
                ok = True
                if not r.get("spot_available"):
                    hot_fail["no spot pair"] += 1
                    ok = False
                elif not r.get("liq_measured"):
                    hot_fail["book unreadable"] += 1
                    ok = False
                elif (r.get("perp_vol_24h_usd") or 0) < MIN_VOL_USD:
                    hot_fail["perp volume < $1M"] += 1
                    ok = False
                elif (r.get("spot_vol_24h_usd") or 0) < MIN_VOL_USD:
                    hot_fail["spot volume < $1M"] += 1
                    ok = False
                elif (r.get("spot_top_usd") or 0) < need_touch:
                    hot_fail["spot touch < $12.50"] += 1
                    ok = False
                elif (r.get("perp_top_usd") or 0) < need_touch:
                    hot_fail["perp touch < $12.50"] += 1
                    ok = False

                net = 0.0
                if ok:
                    cost50 = (r.get("cost_bps") or 0.0) * COST_RATIO_50
                    # Funding is signed: a negative rate pays the SHORT side, so
                    # magnitude is what a correctly-oriented position earns.
                    net = mag * 24.0 * HOLD_DAYS - cost50
                    if net <= 0:
                        hot_fail["net negative after cost"] += 1
                        ok = False

                if ok:
                    hot_pass += 1
                    hot_pass_rates.append(mag)
                    net_bps_pass.append(net)

                key = (r.get("venue"), r.get("symbol"))
                ts = r.get("ts_ms") or 0
                st = state.get(key)
                if st is None or ts - st["last"] > MAX_GAP_MS:
                    if st:
                        episodes.append(((st["last"] - st["start"]) / 3600000.0,
                                         st["peak"], key[0], key[1], st["ok"],
                                         st["net"]))
                    state[key] = {"start": ts, "last": ts, "peak": mag,
                                  "ok": ok, "net": net}
                else:
                    st["last"] = ts
                    st["peak"] = max(st["peak"], mag)
                    st["ok"] = st["ok"] or ok
                    st["net"] = max(st["net"], net)

    for key, st in state.items():
        episodes.append(((st["last"] - st["start"]) / 3600000.0, st["peak"],
                         key[0], key[1], st["ok"], st["net"]))

    print("RE-EVALUATED AT $%.0f/LEG  (touch >= $%.2f, vol >= $%.0fM)"
          % (NOTIONAL, need_touch, MIN_VOL_USD / 1e6))
    print("%d observations, %d at or above %.2f bps/hr\n"
          % (total, hot, EPISODE_BPS_HR))

    print("OF %d HOT OBSERVATIONS" % hot)
    print("  tradeable at $50:  %d  (%.1f%%)" % (hot_pass, 100.0 * hot_pass / max(hot, 1)))
    print("  refused:")
    for g, n in hot_fail.most_common():
        print("    %-26s %7d  %5.1f%%" % (g, n, 100.0 * n / max(hot, 1)))

    if hot_pass_rates:
        s = sorted(hot_pass_rates)
        print("\n  their funding: median %.3f bps/hr (%.0f%%/yr), p90 %.3f, max %.3f"
              % (statistics.median(s), statistics.median(s) * 24 * 365 / 100.0,
                 pct(s, 0.9), s[-1]))
        n = sorted(net_bps_pass)
        print("  net over a 30d hold: median %.0f bps, p90 %.0f bps"
              % (statistics.median(n), pct(n, 0.9)))

    good = [e for e in episodes if e[4]]
    print("\nEPISODES: %d total, %d tradeable at $50 (%.1f%%)"
          % (len(episodes), len(good), 100.0 * len(good) / max(len(episodes), 1)))
    if good:
        d = sorted(e[0] for e in good)
        print("  duration: median %.1f h, p90 %.1f h, max %.1f h"
              % (statistics.median(d), pct(d, 0.9), d[-1]))
        print("\n  LONGEST TRADEABLE EPISODES AT $50")
        for dur, peak, venue, sym, _, net in sorted(good, key=lambda e: -e[0])[:15]:
            print("    %-9s %-14s %7.1f h  peak %7.2f bps/hr (%5.0f%%/yr)  net30 %+7.0f bps"
                  % (venue, sym, dur, peak, peak * 24 * 365 / 100.0, net))

    print("\nCAVEAT: cost scaled by %.2f from $400 measurements on 7 pairs."
          % COST_RATIO_50)
    print("Replace with direct $50 measurements once the sampler has coverage.")


if __name__ == "__main__":
    main()
