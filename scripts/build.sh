#!/bin/sh
# Single source of truth for which binary comes from which package.
#
# bin/cross-union is built from ./cmd/cross -- the name differs from the
# package because two cross-venue books once ran side by side against
# different data directories. An external review on 2026-08-20 correctly
# flagged the running binary as unreproducible from the archive, because
# nothing recorded that mapping. It is recorded here now.
set -e
cd "$(dirname "$0")/.."
mkdir -p bin
build() { echo "  $2 <- $1"; go build -o "bin/$2" "$1"; }

build ./cmd/monitor      monitor       # vega.service
build ./cmd/cross        cross-union   # vega-union.service
build ./cmd/listslip     listslip
build ./cmd/retire       retire
build ./cmd/borrow       borrow
build ./cmd/leverage     leverage
build ./cmd/listwatch    listwatch
build ./cmd/paper-report paper-report
build ./cmd/paperrebuild paperrebuild
build ./cmd/listreport   listreport
build ./cmd/settledscan  settledscan
build ./cmd/settledprobe settledprobe
echo "done"
