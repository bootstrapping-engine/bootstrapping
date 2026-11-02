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
#   BS_POSTGRES=postgres://user@host/db?sslmode=disable \
#   BS_MYSQL=mysql://root:secret@127.0.0.1:3306/ sh scripts/record-tests.sh
#
# Needs Go and Python 3 (and a C compiler for the race detector). With
# BS_POSTGRES naming a PostgreSQL server, the tests that run on every
# database run on it too, each in a schema of its own, and their figures
# are written as *-postgres.json; with BS_MYSQL naming a MySQL or MariaDB
# server, the same in a database of its own each, written as *-mysql.json
# (BS_MYSQL_VERSION, if set, is noted in the header). Without them, only
# SQLite is recorded. The record workflow runs this on GitHub with both.
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

cmd="BS_SCALE=$scale go test -count=1 -v -timeout 120m ./..."
if [ -n "${BS_POSTGRES:-}" ]; then
	pg=$(psql "$BS_POSTGRES" -Atc 'SHOW server_version' 2>/dev/null || echo 'version unknown')
	cmd="$cmd  (with BS_POSTGRES set: PostgreSQL $pg)"
else
	echo "BS_POSTGRES isn't set: PostgreSQL isn't recorded" >&2
fi
if [ -n "${BS_MYSQL:-}" ]; then
	cmd="$cmd  (with BS_MYSQL set: ${BS_MYSQL_VERSION:-MySQL, version unknown})"
else
	echo "BS_MYSQL isn't set: MySQL isn't recorded" >&2
fi
{ header "Every test, verbose, at $scale times the CI size; the tests that measure something also write the JSON files here" "$cmd"
  BS_SCALE=$scale BS_RESULTS="$(pwd)/$out" go test -count=1 -v -timeout 120m ./... 2>&1; } > "$out/tests.txt"

cmd="CGO_ENABLED=1 go test -race -count=1 ./..."
{ header "Every test with the race detector, at CI size, on SQLite" "$cmd"; env -u BS_POSTGRES -u BS_MYSQL CGO_ENABLED=1 go test -race -count=1 ./... 2>&1; } > "$out/race.txt"

cmd="python3 -B -m unittest discover -s scripts -p *_test.py -v"
{ header "The release plan's cases" "$cmd"; $cmd 2>&1; } > "$out/release-plan.txt"

tags=sqlite_omit_load_extension,osusergo,netgo
mkdir "$work/bin"
for c in bootstrapping bst; do
	CGO_ENABLED=0 go build -trimpath -tags $tags -ldflags '-s -w' -o "$work/bin/$c" ./cmd/$c
done
CGO_ENABLED=0 go build -trimpath -tags $tags -ldflags '-s -w' -o "$work/bin/bootstrapping-demo" ./examples/permissions

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
