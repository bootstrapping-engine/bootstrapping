#!/bin/sh
# Copyright Bootstrapping.org 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs every test, larger than CI runs them (BS_SCALE), the race detector,
# the release plan's cases, the demo and the smoke check on static
# release-style binaries, and writes the output to test/results/ with a note
# of the machine they ran on. The tests that measure something also write a
# JSON file of their figures there. The numbers in the README and on
# bootstrapping.org come from these files.
#
#   sh scripts/record-tests.sh
#
# Needs Go, a C compiler (the SQLite driver is cgo) and Python 3.
set -eu
cd "$(dirname "$0")/.."
out=test/results
# Whether the checkout was clean before anything was recorded. The old
# results are part of the checkout, so this comes before they're cleared.
state="at $(git rev-parse --short HEAD 2>/dev/null || echo 'no commit')$(test -z "$(git status --porcelain 2>/dev/null)" || echo ' plus uncommitted changes')"
mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.txt
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
scale=${BS_SCALE:-10}

header() {
	echo "# $1"
	echo "# recorded $(date -u '+%Y-%m-%d %H:%M UTC') $state, Bootstrapping $(sed -n 's/^const Version = "\(.*\)"$/\1/p' bootstrapping.go), $(go version | cut -d' ' -f3-)"
	cpu=$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //' || true)
	test -n "$cpu" || cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo 'unknown CPU')
	echo "# $(uname -sr), $cpu, $(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo '?') CPUs"
	echo "# command: $2"
	echo
}

go vet ./...

cmd="BS_SCALE=$scale go test -count=1 -v -timeout 60m ./..."
{ header "Every test, verbose, at $scale times the CI size; the promise, crash, process and affinity tests also write the JSON files here" "$cmd"
  BS_SCALE=$scale BS_RESULTS="$(pwd)/$out" go test -count=1 -v -timeout 60m ./... 2>&1; } > "$out/tests.txt"

cmd="go test -race -count=1 ./..."
{ header "Every test with the race detector, at CI size" "$cmd"; $cmd 2>&1; } > "$out/race.txt"

cmd="python3 -B -m unittest discover -s scripts -p *_test.py -v"
{ header "The release plan's cases" "$cmd"; $cmd 2>&1; } > "$out/release-plan.txt"

tags=sqlite_omit_load_extension,osusergo,netgo
mkdir "$work/bin"
for c in bootstrapping bst; do
	CGO_ENABLED=1 go build -trimpath -tags $tags -ldflags '-s -w -extldflags "-static"' -o "$work/bin/$c" ./cmd/$c 2>/dev/null
done
CGO_ENABLED=1 go build -trimpath -tags $tags -ldflags '-s -w -extldflags "-static"' -o "$work/bin/bootstrapping-demo" ./examples/permissions 2>/dev/null

cmd="bootstrapping-demo"
{ header "The demo: roles and permissions through three seed versions, a conflict and an apply killed halfway" "$cmd"
  "$work/bin/bootstrapping-demo" 2>&1; } > "$out/demo.txt"

cmd="python3 scripts/smoke.py BIN"
{ header "The release smoke check on static binaries built as the release workflow builds them" "$cmd"
  ls -l "$work/bin" | sed 1d | awk '{print "# " $5 " bytes  " $9}'
  echo
  python3 -B scripts/smoke.py "$work/bin" 2>&1; } > "$out/smoke.txt"

grep -E '^(ok|FAIL|--- FAIL)' "$out/tests.txt" "$out/race.txt" || true
tail -n 1 "$out/release-plan.txt" "$out/smoke.txt" "$out/demo.txt" 2>/dev/null || true
echo "results written to $out/"
