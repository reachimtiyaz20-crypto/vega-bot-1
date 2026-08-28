#!/usr/bin/env python3
"""Make the cost sampler coverage-driven and multi-size.

Expects cmd/listslip/coverage.go to be in place already.

Run from ~/vega-bot.
"""

import os
import shutil
import sys

LS = "cmd/listslip/main.go"

edits = []


def edit(name, old, new):
    edits.append((name, old, new))


# ------------------------------------------------------------------- flags
edit(
    "flags: -sizes and -coverage-target",
    '\tmaxPairs := flag.Int("max-pairs", 12, "book reads per run (venue rate limits)")\n',

    '\tmaxPairs := flag.Int("max-pairs", 12, "book reads per run (venue rate limits)")\n'
    '\tsizes := flag.String("sizes", "50,100,400,1000",\n'
    '\t\t"notionals swept per book read; the READ is the rate-limited part, the sweep is arithmetic")\n'
    '\tcoverageTarget := flag.Int("coverage-target", 40,\n'
    '\t\t"sample floor per venue pair, matching crossvenue minCostSamples; "+\n'
    '\t\t\t"pairs below it are measured regardless of spread")\n',
)

# ------------------------------------------------------- parse sizes early
edit(
    "parse -sizes right after flag.Parse",
    "\tflag.Parse()\n",

    "\tflag.Parse()\n"
    "\n"
    "\tsizeList, err := parseSizes(*sizes)\n"
    "\tif err != nil {\n"
    "\t\tfmt.Fprintln(os.Stderr, err)\n"
    "\t\tos.Exit(1)\n"
    "\t}\n"
    "\t// -notional is superseded by -sizes, which sweeps several. The flag is\n"
    "\t// kept so the existing unit file keeps parsing; removing it would break\n"
    "\t// a running service for no gain.\n"
    "\t_ = notional\n",
)

# ----------------------------------------------------------- load coverage
edit(
    "load coverage before selecting candidates",
    "\tvar cands []cand\n",

    "\t// What is already measured decides what is worth measuring now.\n"
    "\tcovPath := filepath.Join(*dataDir, \"listslip\", \"measurements.jsonl\")\n"
    "\tcoverage, cerr := loadCoverage(covPath)\n"
    "\tif cerr != nil {\n"
    "\t\tfmt.Fprintln(os.Stderr, cerr)\n"
    "\t\tos.Exit(1)\n"
    "\t}\n"
    "\treportCoverage(coverage, *coverageTarget)\n"
    "\n"
    "\tvar cands []cand\n",
)

# -------------------------------------------------------------- the filter
edit(
    "admit under-covered pairs regardless of spread",
    "\t\t\t\tif sp >= *minSpread {\n"
    "\t\t\t\t\tcands = append(cands, cand{coin, a, b, vm[a], vm[b], sp})\n"
    "\t\t\t\t}\n",

    "\t\t\t\t// The spread floor applies only to pairs that are already\n"
    "\t\t\t\t// covered. What a fill costs is a property of the order book,\n"
    "\t\t\t\t// not of the funding rate, so a pair we have never measured\n"
    "\t\t\t\t// teaches us as much at 0.5 bps/hr as at 50 -- and eleven pairs\n"
    "\t\t\t\t// sat at zero samples for seven days because the floor kept\n"
    "\t\t\t\t// them out while bitget|bybit was measured 1,298 times.\n"
    "\t\t\t\tif sp >= *minSpread || coverage[pairKey(a, b)] < *coverageTarget {\n"
    "\t\t\t\t\tcands = append(cands, cand{coin, a, b, vm[a], vm[b], sp})\n"
    "\t\t\t\t}\n",
)

# ---------------------------------------------------------------- the sort
edit(
    "rank by coverage first, spread second",
    "\tsort.Slice(cands, func(i, j int) bool { return cands[i].spread > cands[j].spread })\n",

    "\t// Least-covered venue pair first; spread only breaks ties. Ranking by\n"
    "\t// spread alone is what let one pair take 40% of every sample while the\n"
    "\t// book refused 927 candidates a day for want of data on the others.\n"
    "\tsort.Slice(cands, func(i, j int) bool {\n"
    "\t\tci := coverage[pairKey(cands[i].a, cands[i].b)]\n"
    "\t\tcj := coverage[pairKey(cands[j].a, cands[j].b)]\n"
    "\t\tif ci != cj {\n"
    "\t\t\treturn ci < cj\n"
    "\t\t}\n"
    "\t\treturn cands[i].spread > cands[j].spread\n"
    "\t})\n"
    "\n"
    "\t// One read per venue pair per run. Without this the least-covered pair\n"
    "\t// wins every slot with a different coin each time, which fills one pair\n"
    "\t// and starves the rest -- the original bug wearing a new hat.\n"
    "\t{\n"
    "\t\tseen := map[string]bool{}\n"
    "\t\tspread := cands[:0]\n"
    "\t\tfor _, c := range cands {\n"
    "\t\t\tk := pairKey(c.a, c.b)\n"
    "\t\t\tif seen[k] {\n"
    "\t\t\t\tcontinue\n"
    "\t\t\t}\n"
    "\t\t\tseen[k] = true\n"
    "\t\t\tspread = append(spread, c)\n"
    "\t\t}\n"
    "\t\tcands = spread\n"
    "\t}\n",
)

# ------------------------------------------------------------ the sweep
edit(
    "sweep every read at all sizes",
    "\t\tslipA, okA := bkA.RoundTripSlippageBps(*notional)\n"
    "\t\tslipB, okB := bkB.RoundTripSlippageBps(*notional)\n"
    "\t\tif !okA || !okB {\n"
    "\t\t\t// Book too thin to fill the size at all. That IS the measurement --\n"
    "\t\t\t// record it as unfillable rather than dropping it, because a\n"
    "\t\t\t// backtest that assumes 40 bps silently assumes fillability.\n"
    "\t\t\tfmt.Printf(\"%-12s %-9s %-9s %8.3f   TOO THIN TO FILL $%.0f\\n\",\n"
    "\t\t\t\tc.coin, c.a, c.b, c.spread, *notional)\n"
    "\t\t\tcontinue\n"
    "\t\t}\n"
    "\t\tdA, _, _ := bkA.DepthWithinBps(*depthBand)\n",

    "\t\tdA, _, _ := bkA.DepthWithinBps(*depthBand)\n",
)

edit(
    "emit one record per size",
    "\t\tfees := 2 * (tA + tB)\n"
    "\t\tcost := slipA + slipB + fees\n"
    "\t\trepay := 0.0\n"
    "\t\tif c.spread > 0 {\n"
    "\t\t\trepay = cost / c.spread\n"
    "\t\t}\n"
    "\t\tm := measurement{\n"
    "\t\t\tTS: time.Now().UTC().Format(time.RFC3339), Coin: c.coin,\n"
    "\t\t\tVenueA: c.a, VenueB: c.b, SymbolA: c.ra.symbol, SymbolB: c.rb.symbol,\n"
    "\t\t\tSpreadBpsHr: c.spread, IntervalA: c.ra.interval, IntervalB: c.rb.interval,\n"
    "\t\t\tSlipABps: slipA, SlipBBps: slipB, SlipTotal: slipA + slipB,\n"
    "\t\t\tFeesBps: fees, CostBps: cost, HoursRepay: repay,\n"
    "\t\t\tDepthAUSD: dA, DepthBUSD: dB, NotionalUSD: *notional,\n"
    "\t\t\tFeesKnown: knownA && knownB,\n"
    "\t\t}\n"
    "\t\tenc.Encode(m)\n"
    "\t\tflag := \"\"\n"
    "\t\tif !m.FeesKnown {\n"
    "\t\t\tflag = \"  (fees unverified)\"\n"
    "\t\t}\n"
    "\t\tfmt.Printf(\"%-12s %-9s %-9s %8.3f %8.2f %8.2f %8.1f %9.1f %10.1f%s\\n\",\n"
    "\t\t\tc.coin, c.a, c.b, c.spread, slipA, slipB, fees, cost, repay, flag)\n",

    "\t\tfees := 2 * (tA + tB)\n"
    "\n"
    "\t\t// ONE BOOK READ, MANY SIZES. Fetching the book is the expensive part\n"
    "\t\t// and the only part a rate limit sees; sweeping it again at another\n"
    "\t\t// notional is arithmetic on bytes already in memory. Four sizes cost\n"
    "\t\t// nothing extra and produce the capacity curve as a by-product.\n"
    "\t\t//\n"
    "\t\t// This matters more than it sounds: every sample taken before today\n"
    "\t\t// was at $400/leg, and the books now trade $50. A cost table measured\n"
    "\t\t// at one size cannot price a position at another.\n"
    "\t\tfor _, size := range sizeList {\n"
    "\t\t\tslipA, okA := bkA.RoundTripSlippageBps(size)\n"
    "\t\t\tslipB, okB := bkB.RoundTripSlippageBps(size)\n"
    "\t\t\tif !okA || !okB {\n"
    "\t\t\t\t// Too thin AT THIS SIZE, which is itself a measurement: it says\n"
    "\t\t\t\t// where the book stops being able to fill. A table holding only\n"
    "\t\t\t\t// fillable prices hides exactly that.\n"
    "\t\t\t\tfmt.Printf(\"%-12s %-9s %-9s %8.3f   TOO THIN AT $%.0f\\n\",\n"
    "\t\t\t\t\tc.coin, c.a, c.b, c.spread, size)\n"
    "\t\t\t\tcontinue\n"
    "\t\t\t}\n"
    "\t\t\tcost := slipA + slipB + fees\n"
    "\t\t\trepay := 0.0\n"
    "\t\t\tif c.spread > 0 {\n"
    "\t\t\t\trepay = cost / c.spread\n"
    "\t\t\t}\n"
    "\t\t\tm := measurement{\n"
    "\t\t\t\tTS: time.Now().UTC().Format(time.RFC3339), Coin: c.coin,\n"
    "\t\t\t\tVenueA: c.a, VenueB: c.b, SymbolA: c.ra.symbol, SymbolB: c.rb.symbol,\n"
    "\t\t\t\tSpreadBpsHr: c.spread, IntervalA: c.ra.interval, IntervalB: c.rb.interval,\n"
    "\t\t\t\tSlipABps: slipA, SlipBBps: slipB, SlipTotal: slipA + slipB,\n"
    "\t\t\t\tFeesBps: fees, CostBps: cost, HoursRepay: repay,\n"
    "\t\t\t\tDepthAUSD: dA, DepthBUSD: dB, NotionalUSD: size,\n"
    "\t\t\t\tFeesKnown: knownA && knownB,\n"
    "\t\t\t}\n"
    "\t\t\tenc.Encode(m)\n"
    "\t\t\tflag := \"\"\n"
    "\t\t\tif !m.FeesKnown {\n"
    "\t\t\t\tflag = \"  (fees unverified)\"\n"
    "\t\t\t}\n"
    "\t\t\tfmt.Printf(\"%-12s %-9s %-9s %8.3f %8.2f %8.2f %8.1f %9.1f %10.1f  $%-6.0f%s\\n\",\n"
    "\t\t\t\tc.coin, c.a, c.b, c.spread, slipA, slipB, fees, cost, repay, size, flag)\n"
    "\t\t}\n",
)

# ------------------------------------------------------------- the header
edit(
    "header: report sizes rather than one notional",
    '\tfmt.Printf("%s  %d coins, %d pairs above %.1f bps/hr, measuring %d at $%.0f/leg\\n\\n",\n'
    '\t\ttime.Now().UTC().Format(time.RFC3339), len(rates), len(cands), *minSpread, len(cands), *notional)\n',

    '\tfmt.Printf("%s  %d coins, %d venue pairs selected, sweeping each at $%s/leg\\n\\n",\n'
    '\t\ttime.Now().UTC().Format(time.RFC3339), len(rates), len(cands), *sizes)\n',
)


def main():
    if not os.path.exists(LS):
        raise SystemExit("run this from ~/vega-bot (%s not found)" % LS)
    if not os.path.exists("cmd/listslip/coverage.go"):
        raise SystemExit("cmd/listslip/coverage.go is missing -- copy it first")

    src = open(LS, encoding="utf-8").read()
    original = src

    problems = []
    for name, old, _ in edits:
        n = src.count(old)
        if n != 1:
            problems.append("  %-52s found %d times, want 1" % (name, n))
    if problems:
        print("REFUSING TO PATCH -- anchors did not match:")
        print("\n".join(problems))
        print("\nNothing was written. The file is untouched.")
        return 1

    for name, old, new in edits:
        src = src.replace(old, new, 1)
        print("  ok  %s" % name)

    shutil.copy2(LS, LS + ".precoverage")
    with open(LS, "w", encoding="utf-8") as f:
        f.write(src)
    print("\nwrote %s  (backup: %s.precoverage, %d -> %d bytes)"
          % (LS, LS, len(original), len(src)))
    print("\nNOTE: -notional is now unused by the sweep. Left in place so the\n"
          "existing unit file keeps parsing; -sizes supersedes it.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
