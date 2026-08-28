#!/usr/bin/env python3
"""Is current funding a biased predictor of FORWARD AVERAGE funding?

WHY THIS IS THE ONLY QUESTION THAT MATTERS FOR BOROS

boros4.py found implied sitting 1-5% below current floating on the big
Hyperliquid markets. It is tempting to read that as "the market is offering us
2.21% to take the floating side". It is not. Long YU pays the fixed implied and
receives floating, so it profits only if floating AVERAGES above implied over
the whole life of the contract. The premium is the market's FORECAST of forward
funding, not a discount to a known quantity.

So the trade exists only if the forecast is biased. That is testable, and we
already hold the data to test it:

    Binance      5 years of 8h funding, six majors
    Hyperliquid  hourly, paged back as far as the API will serve

THE TEST

For each sample point t:
    current  = funding right now, annualised
    forward  = the average funding actually realised over the next H days,
               annualised
    H        = 28 / 63 / 119 days, matching the live Boros maturities

Then three things:

 1. BIAS       mean(forward - current). If systematically negative, funding
               decays from wherever it sits, and paying a discount to current
               is CORRECT pricing rather than an opportunity.

 2. REGRESSION forward = a + b*current. b < 1 is mean reversion; b near 0 means
               current tells you almost nothing about forward.

 3. SKILL      RMSE of three forecasts -- "forward = current", the regression,
               and the flat unconditional mean. If the unconditional mean beats
               current, current funding carries essentially no information and
               the entire premium is noise we would be paying 10 bps to trade.

Then it applies the fitted relationship to the LIVE Boros markets: given each
market's current floating, what does history say forward will average, and does
that differ from the implied by more than the round trip?

CAVEAT STATED UP FRONT: overlapping forward windows are heavily autocorrelated,
so the effective sample is far smaller than the row count. Sampling is every 24h
to reduce it, but any t-statistic here would still be overstated. Read the
direction and magnitude, not the significance.

Writes cache to data/boros/. Run from ~/vega-bot:  python3 boros_predict.py
"""

import bisect
import json
import math
import os
import statistics
import time
import urllib.error
import urllib.parse
import urllib.request

CACHE = "data/boros"
FAPI = "https://fapi.binance.com"
HL = "https://api.hyperliquid.xyz/info"
BOROS = "https://api-boros.pendle.finance/apis"

BINANCE = ["BTCUSDT", "ETHUSDT", "BNBUSDT", "XRPUSDT", "SOLUSDT"]
HLCOINS = ["BTC", "ETH", "HYPE"]
HORIZONS = [28, 63, 119]
YEARS = 5
ROUND_TRIP_BPS = 10.0          # 5.0 bps taker each way, from the contract config


def jget(url, timeout=30):
    req = urllib.request.Request(url, headers={"User-Agent": "vega-boros/5.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def jpost(url, body, timeout=30):
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data,
                                 headers={"Content-Type": "application/json",
                                          "User-Agent": "vega-boros/5.0"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def cached(name, fn):
    os.makedirs(CACHE, exist_ok=True)
    p = os.path.join(CACHE, name + ".json")
    if os.path.exists(p) and time.time() - os.path.getmtime(p) < 86400:
        try:
            return json.load(open(p))
        except Exception:
            pass
    d = fn()
    if d:
        json.dump(d, open(p, "w"))
    return d


def binance_funding(sym):
    def go():
        out, cur = [], int((time.time() - YEARS * 365 * 86400) * 1000)
        for _ in range(400):
            try:
                rows = jget("%s/fapi/v1/fundingRate?symbol=%s&startTime=%d&limit=1000"
                            % (FAPI, sym, cur))
            except Exception as e:
                print("    %s fetch stopped: %s" % (sym, e))
                break
            if not rows:
                break
            out += [[int(r["fundingTime"]), float(r["fundingRate"])] for r in rows]
            last = int(rows[-1]["fundingTime"])
            if last <= cur:
                break
            cur = last + 1
            if len(rows) < 1000:
                break
            time.sleep(0.12)
        return out
    return cached("binance_" + sym, go)


def hl_funding(coin):
    def go():
        out, cur = [], int((time.time() - YEARS * 365 * 86400) * 1000)
        seen = set()
        for _ in range(600):
            try:
                rows = jpost(HL, {"type": "fundingHistory", "coin": coin,
                                  "startTime": cur})
            except Exception as e:
                print("    %s fetch stopped: %s" % (coin, e))
                break
            if not rows:
                break
            new = 0
            for r in rows:
                t = int(r["time"])
                if t in seen:
                    continue
                seen.add(t)
                out.append([t, float(r["fundingRate"])])
                new += 1
            last = max(int(r["time"]) for r in rows)
            if new == 0 or last <= cur:
                break
            cur = last + 1
            time.sleep(0.12)
        out.sort()
        return out
    return cached("hl_" + coin, go)


def annualise(rows):
    """Return (ts[], annualised_rate[]) using the observed settlement interval."""
    if len(rows) < 50:
        return [], [], 0.0
    gaps = [rows[i + 1][0] - rows[i][0] for i in range(min(400, len(rows) - 1))]
    gaps = [g for g in gaps if g > 0]
    ivl_ms = statistics.median(gaps)
    per_year = 365.0 * 24.0 * 3600_000.0 / ivl_ms
    ts = [r[0] for r in rows]
    ann = [r[1] * per_year * 100.0 for r in rows]      # percent per year
    return ts, ann, ivl_ms / 3600_000.0


def pairs(ts, ann, horizon_days, step_h=24):
    """(current, forward_mean) sampled every step_h hours."""
    hms = horizon_days * 86400_000
    out = []
    if not ts:
        return out
    i, n = 0, len(ts)
    step_ms = step_h * 3600_000
    nxt = ts[0]
    while i < n:
        if ts[i] < nxt:
            i += 1
            continue
        nxt = ts[i] + step_ms
        end = ts[i] + hms
        if end > ts[-1]:
            break
        j = bisect.bisect_left(ts, end)
        win = ann[i:j]
        if len(win) >= 3:
            out.append((ann[i], sum(win) / len(win)))
        i += 1
    return out


def ols(xs, ys):
    n = len(xs)
    if n < 20:
        return None
    mx, my = sum(xs) / n, sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    if sxx <= 0:
        return None
    b = sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / sxx
    a = my - b * mx
    ss_t = sum((y - my) ** 2 for y in ys)
    ss_r = sum((y - (a + b * x)) ** 2 for x, y in zip(xs, ys))
    r2 = 1 - ss_r / ss_t if ss_t > 0 else 0.0
    return a, b, r2, my


def rmse(vals):
    return math.sqrt(sum(v * v for v in vals) / len(vals)) if vals else float("nan")


def main():
    if not os.path.isdir("data"):
        raise SystemExit("run from ~/vega-bot")

    series = {}
    print("FETCHING (cached 24h in data/boros/)")
    for s in BINANCE:
        r = binance_funding(s)
        ts, ann, ivl = annualise(r)
        if ts:
            series[("Binance", s)] = (ts, ann)
            print("  Binance %-9s %6d settlements, %.0fh interval, from %s"
                  % (s, len(ts), ivl, time.strftime("%Y-%m-%d",
                                                    time.gmtime(ts[0] / 1000))))
    for c in HLCOINS:
        r = hl_funding(c)
        ts, ann, ivl = annualise(r)
        if ts:
            series[("Hyperliquid", c)] = (ts, ann)
            print("  Hyperliq %-9s %6d settlements, %.0fh interval, from %s"
                  % (c, len(ts), ivl, time.strftime("%Y-%m-%d",
                                                    time.gmtime(ts[0] / 1000))))

    fits = {}
    for H in HORIZONS:
        print("\n" + "=" * 76)
        print("HORIZON %d DAYS" % H)
        print("=" * 76)
        print("%-22s %6s %9s %9s %9s %7s %7s"
              % ("series", "n", "cur mean", "fwd mean", "bias", "b", "R2"))
        allx, ally = [], []
        for k, (ts, ann) in sorted(series.items()):
            pr = pairs(ts, ann, H)
            if len(pr) < 40:
                print("%-22s %6d  (too few windows)" % ("/".join(k), len(pr)))
                continue
            xs = [p[0] for p in pr]
            ys = [p[1] for p in pr]
            allx += xs
            ally += ys
            bias = statistics.mean(y - x for x, y in pr)
            f = ols(xs, ys)
            print("%-22s %6d %8.2f%% %8.2f%% %+8.2f%% %7s %7s"
                  % ("/".join(k), len(pr), statistics.mean(xs),
                     statistics.mean(ys), bias,
                     ("%.3f" % f[1]) if f else "-",
                     ("%.3f" % f[2]) if f else "-"))
            if f:
                fits[(k, H)] = f

        if len(allx) > 50:
            f = ols(allx, ally)
            fits[("POOLED", H)] = f
            bias = statistics.mean(y - x for x, y in zip(allx, ally))
            print("%-22s %6d %8.2f%% %8.2f%% %+8.2f%% %7.3f %7.3f"
                  % ("POOLED", len(allx), statistics.mean(allx),
                     statistics.mean(ally), bias, f[1], f[2]))

            e_cur = [y - x for x, y in zip(allx, ally)]
            e_reg = [y - (f[0] + f[1] * x) for x, y in zip(allx, ally)]
            e_avg = [y - f[3] for y in ally]
            print("\n  FORECAST SKILL (RMSE of forward APR, percentage points)")
            print("    forward = current            %6.2f" % rmse(e_cur))
            print("    forward = %.2f + %.3f*current %5.2f"
                  % (f[0], f[1], rmse(e_reg)))
            print("    forward = flat mean %.2f%%     %6.2f" % (f[3], rmse(e_avg)))
            if rmse(e_avg) <= rmse(e_cur):
                print("    -> the FLAT MEAN beats current. Current funding carries")
                print("       no usable information about the forward average at")
                print("       this horizon, so any premium against it is noise.")
            else:
                print("    -> current funding beats the flat mean, so there is")
                print("       real information in the level.")

            print("\n  WHERE DOES FUNDING GO FROM EACH STARTING LEVEL")
            bk = sorted(zip(allx, ally))
            nb = 8
            sz = len(bk) // nb
            print("    %12s %12s %12s" % ("current", "-> forward", "change"))
            for i in range(nb):
                seg = bk[i * sz:(i + 1) * sz] if i < nb - 1 else bk[i * sz:]
                if not seg:
                    continue
                cx = statistics.mean(s[0] for s in seg)
                cy = statistics.mean(s[1] for s in seg)
                print("    %11.2f%% %11.2f%% %+11.2f%%" % (cx, cy, cy - cx))

    # ---- apply to the live Boros book ----
    print("\n" + "=" * 76)
    print("APPLIED TO LIVE BOROS MARKETS")
    print("=" * 76)
    try:
        rows, token = [], None
        while True:
            p = {"limit": 200}
            if token:
                p["resumeToken"] = token
            d = jget(BOROS + "/v1/markets?" + urllib.parse.urlencode(p))
            rows += d.get("results") or []
            token = d.get("resumeToken")
            if not token:
                break
    except Exception as e:
        print("  could not load Boros markets: %s" % e)
        return

    now = time.time()
    print("%-30s %5s %8s %8s %9s %9s %8s"
          % ("market", "days", "implied", "float", "fair fwd", "edge", "net/yr"))
    for m in rows:
        im, da = m.get("imData") or {}, m.get("data") or {}
        mat = im.get("maturity")
        if not isinstance(mat, (int, float)) or mat <= now:
            continue
        imp, flo = da.get("markApr"), da.get("floatingApr")
        if not isinstance(imp, (int, float)) or not isinstance(flo, (int, float)):
            continue
        days = (mat - now) / 86400.0
        H = min(HORIZONS, key=lambda h: abs(h - days))
        plat = (m.get("platform") or {}).get("name")
        sym = (m.get("metadata") or {}).get("fundingRateSymbol") or ""
        key = None
        for k in series:
            if k[0] == plat and (k[1] == sym or k[1].lower() == sym.split("-")[-1]):
                key = k
                break
        f = fits.get((key, H)) or fits.get(("POOLED", H))
        if not f:
            continue
        cur = flo * 100.0
        fair = f[0] + f[1] * cur
        imp_p = imp * 100.0
        edge = fair - imp_p                     # long YU expected PnL, %/yr
        gross = abs(edge) * days / 365.0 * 100.0        # bps over the hold
        net = gross - ROUND_TRIP_BPS
        ann = net / days * 365.0 * 3.2 / 100.0 if days else 0   # % on capital, 3.2x
        tag = "LONG" if edge > 0 else "SHORT"
        print("%-30s %5.0f %7.2f%% %7.2f%% %8.2f%% %+8.2f%% %7.1f%% %s%s"
              % (str(im.get("name"))[:30], days, imp_p, cur, fair, edge,
                 ann, tag, "" if key else " (pooled)"))

    print("\n  'fair fwd' is what history says funding will AVERAGE from this")
    print("  starting level. 'edge' is fair minus implied: positive favours long")
    print("  YU, negative favours short YU. 'net/yr' is after the 10 bps round")
    print("  trip, levered 3.2x, and IGNORES open interest -- boros4 showed the")
    print("  biggest numbers sit on markets holding a few hundred dollars.")
    print("\n  If R2 above is near zero, the 'fair' column is close to a constant")
    print("  and these edges are an artefact of the fit, not a tradeable signal.")


if __name__ == "__main__":
    main()
