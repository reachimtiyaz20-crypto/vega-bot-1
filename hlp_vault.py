#!/usr/bin/env python3
"""What does Hyperliquid's HLP vault ACTUALLY pay, from settled history?

WHY THIS IS THE MOST PROMISING LEAD LEFT

Everything measured this week says the same thing: the returns available to us
are small, and the large ones sit behind capabilities we do not have.

	funding carry            ~4%/yr, below the 4.45% cost of dollars
	reverse carry            +23.9% realised -- on $114 of capacity
	maker volume farming     0.72 bps spread, decided by latency
	new listings             0 of 35 hedgeable
	stablecoin deposits      8.25% published, plus a points bet

Then HLP: reportedly 15-30% APR, and it asks for nothing except capital. It is
not a carry trade. Hyperliquid's liquidity vault takes the other side of
leveraged flow and earns from market making and liquidations -- it is the
execution business we established we cannot build, offered as a deposit.

If that number is real, funding the operation beats running one.

WHY IT MIGHT NOT BE

	survivorship     '15-30% across most quarterly windows' quietly excludes
	                 the windows where it was not
	one bad day      HLP has taken real losses. The JELLY episode forced the
	                 vault into a manipulated position and cost it money. A
	                 vault that is short gamma against the whole market has a
	                 tail, and the tail is what decides whether you can hold it
	deposits         account value moves with flows as well as profit, so
	                 naive 'value went up' arithmetic overstates returns
	total loss       contract or protocol failure is not a drawdown, and no
	                 amount of yield compensates for it

So this reads the vault's own PnL history and computes returns properly:
period by period, drawdowns included, worst windows shown rather than averaged
away.

Public endpoint. No key. Nothing at risk.

Run:  python3 hlp_vault.py
"""

import json
import statistics
import sys
import time
import urllib.error
import urllib.request

API = "https://api.hyperliquid.xyz/info"
HLP = "0xdfc24b077bc1425ad1dea75bcb6f8158e10df303"


def post(payload):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(
        API, data=data,
        headers={"Content-Type": "application/json",
                 "User-Agent": "vega-hlp-probe/1.0"})
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.loads(r.read().decode())


def series(portfolio, key, field):
    """portfolio is [[periodName, {...}], ...]"""
    for name, blob in portfolio:
        if name == key:
            return blob.get(field) or []
    return []


def returns_from(av, pnl):
    """Daily returns using PnL against account value.

    Account value alone is useless -- it moves with deposits and withdrawals as
    well as with profit. PnL divided by the value that earned it is the return.
    """
    out = []
    for i in range(1, min(len(av), len(pnl))):
        t0, v0 = av[i - 1][0], float(av[i - 1][1])
        p0, p1 = float(pnl[i - 1][1]), float(pnl[i][1])
        if v0 <= 0:
            continue
        out.append((av[i][0], (p1 - p0) / v0))
    return out


def stats(rets, label, per_year):
    if len(rets) < 5:
        print("  %s: not enough data (%d points)" % (label, len(rets)))
        return
    vals = [r for _, r in rets]
    eq, peak, dd = 1.0, 1.0, 0.0
    for r in vals:
        eq *= (1.0 + r)
        peak = max(peak, eq)
        dd = max(dd, (peak - eq) / peak)
    yrs = len(vals) / per_year
    cagr = (eq ** (1.0 / yrs) - 1.0) * 100.0 if yrs > 0 and eq > 0 else float("nan")
    worst = min(vals) * 100.0
    neg = 100.0 * sum(1 for r in vals if r < 0) / len(vals)
    print("  %-10s %5d pts  total %+7.1f%%  annualised %+7.1f%%  "
          "max dd %5.1f%%  worst period %+6.2f%%  negative %4.0f%%"
          % (label, len(vals), (eq - 1) * 100, cagr, dd * 100, worst, neg))


def main():
    print("reading Hyperliquid HLP vault, public endpoint, nothing at risk\n")
    try:
        d = post({"type": "vaultDetails", "vaultAddress": HLP})
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
        sys.exit("fetch failed: %s" % e)

    if not d:
        sys.exit("empty response -- endpoint or vault address may have changed")

    print("vault: %s" % d.get("name", "?"))
    if d.get("apr") is not None:
        try:
            print("APR as reported by the venue: %.2f%%" % (float(d["apr"]) * 100))
        except (TypeError, ValueError):
            print("APR as reported by the venue: %s" % d.get("apr"))
    tvl = d.get("maxDistributable") or d.get("maxWithdrawable")
    if tvl:
        print("size indicator: %s" % tvl)
    print("followers: %d" % len(d.get("followers") or []))

    pf = d.get("portfolio") or []
    if not pf:
        sys.exit("no portfolio history returned")
    print("\nperiods available: %s" % ", ".join(n for n, _ in pf))

    print("\nRETURNS COMPUTED FROM THE VAULT'S OWN PnL HISTORY")
    print("(PnL divided by the account value that earned it -- deposits and")
    print(" withdrawals move account value and must not be counted as return)")
    for key, per_year in (("day", 24 * 365), ("week", 24 * 52),
                          ("month", 365), ("allTime", 365)):
        av = series(pf, key, "accountValueHistory")
        pn = series(pf, key, "pnlHistory")
        if not av or not pn:
            continue
        stats(returns_from(av, pn), key, per_year)

    # The all-time series is the one worth interrogating.
    av = series(pf, "allTime", "accountValueHistory")
    pn = series(pf, "allTime", "pnlHistory")
    rets = returns_from(av, pn)
    if len(rets) > 30:
        vals = [r for _, r in rets]
        srt = sorted(vals)
        print("\nDISTRIBUTION, all-time")
        print("  worst 5 periods: %s"
              % ", ".join("%+.2f%%" % (v * 100) for v in srt[:5]))
        print("  best 5 periods:  %s"
              % ", ".join("%+.2f%%" % (v * 100) for v in srt[-5:]))
        print("  median %+.3f%%   mean %+.3f%%"
              % (statistics.median(vals) * 100, statistics.mean(vals) * 100))

        # Rolling 30-period windows: does it ever have a bad quarter?
        win = 30
        rolls = []
        for i in range(len(vals) - win):
            e = 1.0
            for r in vals[i:i + win]:
                e *= (1 + r)
            rolls.append((e - 1) * 100)
        if rolls:
            print("\nROLLING %d-PERIOD WINDOWS  (%d of them)" % (win, len(rolls)))
            print("  worst %+.1f%%   median %+.1f%%   best %+.1f%%"
                  % (min(rolls), statistics.median(rolls), max(rolls)))
            print("  windows that lost money: %d of %d (%.0f%%)"
                  % (sum(1 for r in rolls if r < 0), len(rolls),
                     100.0 * sum(1 for r in rolls if r < 0) / len(rolls)))

        first_ts = av[0][0] if av else None
        if first_ts:
            print("\nhistory begins %s"
                  % time.strftime("%Y-%m-%d", time.gmtime(first_ts / 1000)))

    print("\nHOW TO JUDGE THIS")
    print("  Compare against 4.45%% for doing nothing and 8.25%% for a")
    print("  stablecoin deposit on Felix. HLP has to pay enough MORE than")
    print("  those to justify contract risk, protocol risk, and the tail.")
    print("  The rolling-window row matters more than the headline: an")
    print("  annualised figure hides whether you could have sat through it.")
    print("\n  And nothing here prices a total loss. A vault that has never")
    print("  failed and a vault that cannot fail look identical in this data.")


if __name__ == "__main__":
    main()
