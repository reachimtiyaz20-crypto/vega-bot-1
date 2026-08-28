#!/usr/bin/env python3
"""Ask about every coin we could actually trade, not the 67 I guessed.

WE WERE ASKING ABOUT A TENTH OF OUR OWN UNIVERSE.

vega-borrow's -currencies list was hand-picked: 42 coins, later 67, chosen by
me from a reading of which coins looked interesting. Meanwhile the scanner sees
449 distinct base assets per poll on binance and bybit, 362 of which have a
spot pair and clear the $250k volume floor on both legs.

That mattered more than it sounds. Measured 2026-08-25: 19 observations cleared
the reverse-carry entry bar overnight and ZERO were on a borrowable coin. The
edge was there; we simply had not asked whether those coins could be borrowed.
A hand-picked list cannot find the opportunity it was not written to include.

The list is generated here rather than typed into the unit file so it tracks
the universe as coins list and delist. Rerun it weekly.

FILTERS, and why each one:

  spot pair          no spot leg, no cash-and-carry and no short spot either
  $250k both legs    matches the reverse book's -min-vol; asking a venue to
                     lend a coin we would refuse on liquidity is wasted calls
  ASCII only         one symbol came back as 币安人生, which would be mangled
                     by URL encoding somewhere downstream and fail in a way
                     that looks like "not lendable" rather than "bad request"

Run from ~/vega-bot as root, then:
    systemctl daemon-reload && systemctl restart vega-borrow
"""

import glob
import json
import os
import re
import shutil
import sys
import time

UNIT = "/etc/systemd/system/vega-borrow.service"
JOURNALS = ["data/journal/*.jsonl", "data/reverse/journal/*.jsonl"]
HOURS = 6
MIN_VOL = 250_000

# Sanity rails. A list of 3 means the journal is empty and we are about to
# blind the borrow scanner; a list of 3000 means the filter broke and we are
# about to hammer two exchanges. Either way, refuse rather than apply.
MIN_COINS = 50
MAX_COINS = 900

# Always keep these regardless of what the last few hours looked like: the
# quote assets, and the majors whose borrow rate is a useful baseline even
# when nothing is trading on them.
ALWAYS = {"USDT", "USDC", "BTC", "ETH"}

SAFE = re.compile(r"^[A-Z0-9]{1,20}$")


def base(sym):
    for q in ("USDT", "USDC", "USD"):
        if sym.endswith(q) and len(sym) > len(q):
            return sym[: -len(q)]
    return sym


def collect():
    cut = (time.time() - HOURS * 3600) * 1000
    out = set()
    paths = []
    for pat in JOURNALS:
        paths += sorted(glob.glob(pat))
    if not paths:
        raise SystemExit("no journal files found -- run from ~/vega-bot")
    for p in paths:
        with open(p, errors="replace") as f:
            for line in f:
                if '"obs"' not in line:
                    continue
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if r.get("type") != "obs" or (r.get("ts_ms") or 0) < cut:
                    continue
                if not r.get("spot_available"):
                    continue
                if (r.get("spot_vol_24h_usd") or 0) < MIN_VOL:
                    continue
                if (r.get("perp_vol_24h_usd") or 0) < MIN_VOL:
                    continue
                c = base(r.get("symbol") or "")
                if SAFE.match(c):
                    out.add(c)
    return out


def main():
    if not os.path.exists("data"):
        raise SystemExit("run this from ~/vega-bot")
    if not os.path.exists(UNIT):
        raise SystemExit("%s not found" % UNIT)

    coins = collect() | ALWAYS
    n = len(coins)
    if n < MIN_COINS or n > MAX_COINS:
        print("REFUSING -- generated %d coins, outside the sane range %d-%d"
              % (n, MIN_COINS, MAX_COINS))
        return 1

    listing = ",".join(sorted(coins))

    src = open(UNIT, encoding="utf-8").read()
    m = re.search(r"-currencies (\S+)", src)
    if not m:
        print("REFUSING -- no -currencies argument found in %s" % UNIT)
        return 1

    old = set(m.group(1).split(","))
    added = sorted(coins - old)
    dropped = sorted(old - coins)

    print("was %d coins, now %d" % (len(old), n))
    print("  added   %d" % len(added))
    print("  dropped %d%s" % (len(dropped), (": " + ",".join(dropped)) if dropped else ""))
    print("\nestimated runtime: %.1f minutes at the measured 0.3s/coin" % (n * 0.3 / 60))

    if listing == m.group(1):
        print("\nlist unchanged -- nothing to do")
        return 0

    shutil.copy2(UNIT, UNIT + ".prewiden")
    src = src[: m.start(1)] + listing + src[m.end(1):]
    open(UNIT, "w", encoding="utf-8").write(src)
    print("\nwrote %s (backup: %s.prewiden)" % (UNIT, UNIT))
    print("now run: systemctl daemon-reload && systemctl restart vega-borrow")
    return 0


if __name__ == "__main__":
    sys.exit(main())
