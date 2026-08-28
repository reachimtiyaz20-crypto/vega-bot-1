#!/usr/bin/env python3
"""Put the market's forward funding forecast next to our own entry reads.

WHY

boros_predict.py established that OUR history cannot forecast forward funding:
pooled slope 0.101, R-squared 0.066, and a flat mean beats using the current
rate (RMSE 14.42 vs 13.93). Two buckets both starting at 10.95% went to 3.60%
and 13.82%. From the baseline, forward funding is a coin flip to us.

Boros is an order book where people take that risk with real money, and its
implied APR is exactly the missing quantity: what funding will AVERAGE between
now and maturity. Right now it says Hyperliquid HYPE 9.63%, BTC 8.10-8.74%,
ETH 7.70% -- while our cross-venue book enters on reads of 40%+/yr.

If Boros is right, this book's entry numbers are decay that has not happened
yet. That is worth watching per-position rather than arguing about, so this
adds a `fwd %/yr` column beside `entry` and `now`.

Applies three anchored edits and refuses unless each anchor matches EXACTLY
once. Writes vega_web.py.bak first.

    python3 patch_boros_web.py
"""

import os
import shutil
import sys

TARGET = "vega_web.py"

FUNC = '''
BOROS_MARKETS = "https://api-boros.pendle.finance/apis/v1/markets"
BOROS_CACHE = "data/boros/implied_cache.json"
BOROS_TTL = 900


def boros_forecast():
    """coin -> Boros implied APR (%/yr), nearest live Hyperliquid maturity.

    The `now` column on this table is NOT a forecast. It is the instantaneous
    rate, and we measured that it predicts the forward average about as well as
    a coin flip. This column is a forecast, priced by people with capital at
    risk, and it is the honest comparison for our entry reads.

    Cached 15 minutes. Fails silently and returns {} -- a Boros outage must
    never blank this page, and a stale number shown as live would be worse than
    no number at all.
    """
    import json as _j
    import time as _t
    import urllib.request as _u

    cp = os.path.join(ROOT, BOROS_CACHE)
    try:
        if os.path.exists(cp) and _t.time() - os.path.getmtime(cp) < BOROS_TTL:
            return _j.load(open(cp))
    except Exception:
        pass

    try:
        rows, token = [], None
        while True:
            url = BOROS_MARKETS + "?limit=200"
            if token:
                url += "&resumeToken=" + token
            rq = _u.Request(url, headers={"User-Agent": "vega-web/1.0",
                                          "Accept": "application/json"})
            with _u.urlopen(rq, timeout=8) as r:
                d = _j.loads(r.read().decode())
            rows += d.get("results") or []
            token = d.get("resumeToken")
            if not token:
                break

        now = _t.time()
        best = {}
        for m in rows:
            if ((m.get("platform") or {}).get("name") or "") != "Hyperliquid":
                continue
            im, da, md = (m.get("imData") or {}, m.get("data") or {},
                          m.get("metadata") or {})
            mat, imp = im.get("maturity"), da.get("markApr")
            sym = (md.get("underlyingSymbol") or "").upper()
            if not sym or not isinstance(mat, (int, float)) or mat <= now:
                continue
            if not isinstance(imp, (int, float)) or imp == 0:
                continue
            # Nearest maturity: our holds are days, not quarters.
            if sym not in best or mat < best[sym][0]:
                best[sym] = (mat, imp * 100.0)
        out = {k: v[1] for k, v in best.items()}
        try:
            os.makedirs(os.path.dirname(cp), exist_ok=True)
            _j.dump(out, open(cp, "w"))
        except Exception:
            pass
        return out
    except Exception:
        try:
            return _j.load(open(cp))
        except Exception:
            return {}


'''

A_ANCHOR = 'def hlbook():\n    st = load("data/hlbook/state.json")'

B_ANCHOR = ('    h.append("<table><tr><th></th><th>coin</th><th>spot</th>'
            '<th>held</th>"\n'
            '             "<th>entry %/yr</th><th>now %/yr</th><th>net bps</th>"\n'
            '             "<th>reason</th></tr>")')

B_NEW = ('    fwd = boros_forecast()\n'
         '    h.append("<table><tr><th></th><th>coin</th><th>spot</th>'
         '<th>held</th>"\n'
         '             "<th>entry %/yr</th><th>now %/yr</th>'
         '<th>fwd %/yr</th>"\n'
         '             "<th>net bps</th><th>reason</th></tr>")')

C_ANCHOR = '''        entry = p.get("entry_f_pct_yr") or 0
        now_f = p.get("last_f") or 0
        decay = "r" if now_f < entry else "g"
        h.append("<tr><td class='d'>%s</td><td>%s</td><td class='d'>%s</td>"
                 "<td class='d'>%.1fh</td><td class='d'>%.1f</td>"
                 "<td class='%s'>%.1f</td><td>%s</td><td class='d'>%s</td></tr>"
                 % ("OPEN" if is_open else "CLOSED",
                    html.escape(str(p.get("coin", "?"))),
                    html.escape(str(p.get("spot_venue", "?"))),
                    held, entry, decay, now_f, num(net),
                    html.escape(str(p.get("close_reason") or "")[:44])))'''

C_NEW = '''        entry = p.get("entry_f_pct_yr") or 0
        now_f = p.get("last_f") or 0
        decay = "r" if now_f < entry else "g"
        # The market's forecast. Red when it sits BELOW the current rate,
        # i.e. when Boros is pricing decay this position has not taken yet.
        f = fwd.get(str(p.get("coin", "")).upper())
        if f is None:
            fcell = "<td class='d'>-</td>"
        else:
            fcell = "<td class='%s'>%.1f</td>" % ("r" if f < now_f else "g", f)
        h.append("<tr><td class='d'>%s</td><td>%s</td><td class='d'>%s</td>"
                 "<td class='d'>%.1fh</td><td class='d'>%.1f</td>"
                 "<td class='%s'>%.1f</td>%s<td>%s</td>"
                 "<td class='d'>%s</td></tr>"
                 % ("OPEN" if is_open else "CLOSED",
                    html.escape(str(p.get("coin", "?"))),
                    html.escape(str(p.get("spot_venue", "?"))),
                    held, entry, decay, now_f, fcell, num(net),
                    html.escape(str(p.get("close_reason") or "")[:44])))'''

D_ANCHOR = '''    if len(c) < 10:
        h.append("<p class='note'>%d closes. The reverse book read +23.9%% at "
                 "four closes and +4.6%% at seven &mdash; judge this at thirty, "
                 "not before.</p>" % len(c))'''

D_NEW = '''    h.append("<p class='note'><b>fwd</b> is the Boros implied APR for the "
             "nearest live Hyperliquid maturity &mdash; a funding-rate futures "
             "order book, so it is real money's forecast of what funding will "
             "AVERAGE to maturity, not another spot reading. Our own history "
             "cannot forecast this at all (slope 0.101, R&sup2; 0.066, and a "
             "flat mean beats using the current rate), so where fwd sits well "
             "below now, the market is pricing decay this book has not taken "
             "yet. Cached 15 minutes; a dash means Boros lists no live market "
             "for that coin, which is true of every alt.</p>")
    if len(c) < 10:
        h.append("<p class='note'>%d closes. The reverse book read +23.9%% at "
                 "four closes and +4.6%% at seven &mdash; judge this at thirty, "
                 "not before.</p>" % len(c))'''


def main():
    if not os.path.exists(TARGET):
        sys.exit("run from the directory holding %s" % TARGET)
    src = open(TARGET).read()

    if "boros_forecast" in src:
        sys.exit("already patched -- boros_forecast is present. Nothing done.")

    edits = [("insert boros_forecast()", A_ANCHOR, FUNC.lstrip("\n") + A_ANCHOR),
             ("table header", B_ANCHOR, B_NEW),
             ("row rendering", C_ANCHOR, C_NEW),
             ("explanatory note", D_ANCHOR, D_NEW)]

    for name, anchor, _ in edits:
        n = src.count(anchor)
        if n != 1:
            sys.exit("REFUSING: anchor '%s' matched %d times, expected exactly 1.\n"
                     "The file differs from what this patch was written against."
                     % (name, n))
    print("all %d anchors matched exactly once" % len(edits))

    for name, anchor, new in edits:
        src = src.replace(anchor, new, 1)
        print("  applied: %s" % name)

    shutil.copy2(TARGET, TARGET + ".bak")
    open(TARGET, "w").write(src)
    import ast
    ast.parse(src)
    print("\nwrote %s (backup at %s.bak), syntax OK" % (TARGET, TARGET))
    print("restart with:  systemctl restart vega-web")


if __name__ == "__main__":
    main()
