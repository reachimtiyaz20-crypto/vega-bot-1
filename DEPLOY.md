# VEGA — deploy from scratch

Funding-rate arbitrage MEASUREMENT rig. Paper only. It holds no credentials and
contains no code path that can place an order on a real account.

Tested on Ubuntu 24.04, 1 vCPU / 1 GB RAM / 24 GB disk.

---

## 0. What you need

- Ubuntu 22.04+ with systemd
- Go 1.21 or newer
- `jq`, `curl`, `tar`
- Outbound HTTPS to api.binance.com, api.bybit.com, api.hyperliquid.xyz
- ~1 GB disk for 90 days of journal

No exchange account. No API keys. Nothing to fund.

---

## 1. Install prerequisites

    sudo apt update
    sudo apt install -y jq curl tar
    sudo snap install go --classic
    go version          # want 1.21+

If `go` is not on PATH after snap:

    export PATH=$PATH:/snap/bin
    echo 'export PATH=$PATH:/snap/bin' >> ~/.bashrc

---

## 2. Restore the code

From the backup archive:

    cd /root
    tar xzf vega-backup-YYYY-MM-DD-HHMM.tar.gz
    mv vega-backup-*/vega-bot /root/vega-bot
    ls /root/vega-bot        # cmd  pkg  go.mod  go.sum  CHANGELOG.md  DEPLOY.md

The archive also contains `system/` with the unit files, the CLI script and the
crontab. Keep it until step 6 is done.

---

## 3. Build

    cd /root/vega-bot
    go mod download
    gofmt -w ./pkg ./cmd
    go vet ./...
    go test ./...            # must be green before anything else
    mkdir -p bin
    for c in monitor scan live report paper-report dispersion breakeven; do
      go build -o bin/$c ./cmd/$c && echo "built $c"
    done

`go mod download` fetches one external dependency, `github.com/xuri/excelize/v2`,
used only for the Excel reports. Everything else is standard library.

---

## 4. Install the CLI

    sudo cp /root/vega-backup-*/system/vega-cli.sh /usr/local/bin/vega
    sudo chmod +x /usr/local/bin/vega
    vega help

If the paths inside differ from `/root/vega-bot`, edit `ROOT=` at the top.

---

## 5. systemd

    sudo cp /root/vega-backup-*/system/vega.service          /etc/systemd/system/
    sudo cp /root/vega-backup-*/system/vega-report.service   /etc/systemd/system/
    sudo cp /root/vega-backup-*/system/vega-report.timer     /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable --now vega
    sudo systemctl enable --now vega-report.timer
    systemctl status vega --no-pager

The monitor's full configuration lives in the unit's `ExecStart` line, ON PURPOSE
— three months from now that line is the only record of what produced the data:

    ExecStart=/root/vega-bot/bin/monitor \
      -data /root/vega-bot/data \
      -poll 5m -sweep 30m -http 127.0.0.1:8081 \
      -notional 400 -hold-short 7 -hold-long 30 \
      -min-vol 10000000 -min-depth 0.25 -max-slip 8 -fallback-slip 2

---

## 6. Cross-venue cron

    mkdir -p /root/vega-bot/data/dispersion
    ( crontab -l 2>/dev/null; \
      echo "7 * * * * /root/vega-bot/bin/dispersion -top 10 -json /root/vega-bot/data/dispersion/log.jsonl >> /root/vega-bot/data/dispersion/cron.log 2>&1" \
    ) | crontab -
    crontab -l

---

## 7. Verify

    vega brief

Expect: monitor RUNNING, timer armed, cron armed. Positions appear within one
poll (5 minutes). Then:

    vega scan
    vega dispersion
    ./bin/paper-report -data /root/vega-bot/data

---

## 8. The dashboard

Bound to 127.0.0.1 deliberately — the page has no authentication and a public
VPS is under continuous SSH brute force. Reach it with a tunnel FROM YOUR LAPTOP:

    ssh -N -L 8081:localhost:8081 root@YOUR_VPS_IP

Then open http://localhost:8081

---

## Command reference

### Running
    vega start | stop | restart | status

### Results
    vega brief          one screen: health, P&L, positions, live dislocations
    vega pnl            one-line net result
    vega positions      open and closed, as a table
    vega report [date]  build the Excel + print the copy command
    vega reports        list every workbook on the server

### Looking
    vega scan           cash-and-carry scanner (add -adaptive for depth sizing)
    vega dispersion     cross-venue perp-perp, fees pre-verified
    vega dash           how to open the web dashboard
    vega logs [n]       last n log lines
    vega tail           follow the log
    vega disk           journal size and free space

### Maintenance
    vega update         rebuild all binaries (runs tests first, refuses if red)
    vega test           run the test suite

### Live trading (only once API keys exist)
    vega pause          kill switch ON: blocks OPENING, still allows closing
    vega resume         kill switch OFF

---

## Configuration

### monitor (`ExecStart` in vega.service)

| Flag | Current | Meaning |
|---|---|---|
| `-notional` | 400 | USD per leg. Capital deployed is ~2x this |
| `-hold-long` | 30 | Days the entry gate is judged against |
| `-min-vol` | 10000000 | 24h quote volume floor, BOTH legs |
| `-min-depth` | 0.25 | Fraction of notional required at the touch |
| `-max-slip` | 8 | Reject if MEASURED round-trip slippage exceeds this |
| `-poll` | 5m | How often venues are queried |
| `-sweep` | 30m | How often EVERY hedgeable symbol is journaled |

Journal grows ~12 MB/day raw, ~16:1 under gzip after daily rotation.

### paper book (`pkg/funding.DefaultPaperConfig`)

| Field | Value | Meaning |
|---|---|---|
| `MaxConcurrent` | 5 | Concurrent positions |
| `PlannedHoldDays` | 30 | Hard cap on any hold |
| `MinHoldDays` | 2 | Anti-churn floor |
| `StopLossBps` | -60 | Closes regardless of everything else |
| `NegativeIntervalsBeforeExit` | 6 | Two days at 8h intervals |
| `ReenterCooldown` | 24h | Stops open/close churn on one symbol |

**The exit rule, in one line:** hold while underwater; exit on sustained negative
funding only once profitable; stop loss and the 30-day cap override both.

---

## What is NOT in the backup, and why

**No API keys.** Credentials live in `/etc/vega/credentials`, outside the repo
and outside every archive, chmod 600 root-owned. The predecessor project's keys
reached a public Drive folder because they sat in a `.env` beside the code.

**No `bin/`.** Binaries are rebuilt in step 3. Shipping them invites running a
stale binary against changed source — a trap this project hit once already.

---

## Known state as of 2026-08-11

Four constants are `false` and gate all live trading:

    execution.BinanceShapesVerified
    execution.BybitShapesVerified
    execution.BinanceOrderShapesVerified
    execution.BybitOrderShapesVerified

Every JSON struct in the order and account files was written from published
documentation, not from a response anyone has inspected. `cmd/live` refuses
mainnet while these are false. Flip them only after placing a testnet order and
reading the raw response with `DumpRaw`.

Fees verified 2026-08-05 / 2026-08-11:

| Venue | Spot taker | Perp taker | Source |
|---|---|---|---|
| Binance | 10 bps | 5.0 bps | binance.com/en/fee/schedule |
| Bybit | 10 bps | 5.5 bps | bybit.com/en/help-center/article/Trading-Fee-Structure |
| Hyperliquid | 7 bps | 4.5 bps | hyperliquid.gitbook.io/hyperliquid-docs/trading/fees |

Fee tiers move. Re-verify every 30 days or the entire cost model is wrong.

---

## Troubleshooting

**No positions after 10 minutes** — `vega logs 40`. Look for `passing 0`, which
means nothing cleared the gate. Confirm with `vega scan`.

**Dashboard blank** — the tunnel runs on your LAPTOP, not the VPS.

**Scan and book disagree** — they use different flags. `vega scan` defaults to
30-day hold; the book uses `-hold-long` from the unit file. This exact mismatch
hid a real bug for a week.

**Journal filling the disk** — raise `-sweep`. Old days gzip automatically.
