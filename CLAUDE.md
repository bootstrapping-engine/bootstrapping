# Notes for Claude

This file says where the next session starts and what comes next. What comes
next is decided with the owner before it goes here, and work starts only when
the owner says go.

## Releases

Releases are automated. To release, raise `Version` in `bootstrapping.go` and
push to `main`; never ask the owner to publish one. [RELEASING.md](RELEASING.md)
has the procedure and the checks to run afterwards.

## Where things stand

v0.1.0 is the first release (October 2026): Apache 2.0, CI on Linux, macOS
and Windows, and automated releases with smoke-tested binaries for Linux,
macOS and Windows. Bootstrapping is a seed reconciler: TOML seed files per
table, a three-way merge per value (new seed, live row, last-applied seed
from a ledger in the same database), per-column ownership (merge, seed,
local), conflicts that block the apply until settled (`on_conflict` in the
seed, `-take` on the command), `on_remove` keep/delete/retire,
`on_local_delete` respect/restore, `_renamed_from`, references by natural
key (`[refs]`, including a table referring to itself), and one
`BEGIN IMMEDIATE` transaction per apply.

The library (root package) works with any `database/sql` SQLite driver and
imports none. The commands (`cmd/bootstrapping`, `cmd/bst`, and the demo
`examples/permissions`, shipped as `bootstrapping-demo`) use
mattn/go-sqlite3, which is cgo: builds need a C compiler, and the release
workflow builds each target on its own kind of machine (MSYS2 MinGW on
Windows). modernc.org/sqlite was the first choice but is hosted on GitLab,
which the build sessions can't reach.

Read [docs/MERGE.md](docs/MERGE.md) before changing how plans are made: it
is the contract `promise_test.go` checks. [docs/SEEDS.md](docs/SEEDS.md) is
the seed format, [docs/REFERENCE.md](docs/REFERENCE.md) the command, the
API and the ledger. Ledger value encodings (`n`, `i:`, `r:`, `t:`, `b:`)
must never change meaning.

Values are compared as SQLite stores them. `TestAffinityMatchesSQLite`
checks the conversion against SQLite itself; a real going into a TEXT
column is converted by SQLite (`CAST`), since its rounding can't be
reproduced in Go. Live columns are read as `+column` so drivers don't
convert BOOLEAN, DATE and DATETIME values by type name.

## Test results

`sh scripts/record-tests.sh` runs every test at ten times the CI size (set
`BS_SCALE` to change it), the race detector, the release plan's cases, the
demo and the smoke check on static binaries, and writes them to
`test/results/`, with JSON figures from the promise, crash, process and
affinity tests. Every number in the README and on the site comes from those
files; record again after any change that could move them. It needs Go, a C
compiler and Python 3.

## Site

Bootstrapping.org is in `photozeitgeist/bootstrapping.org` (October 2026):
the Hugo Launch template copied unchanged from multivergence.com, with
content and config values only. The old bootstrapping.org site was removed
entirely, history included, at the owner's request. Pages: about, contact,
quick start, download, reference. Posts: the launch, a plain explainer of
reference data drift, how it was tested, and the name. Images are drawn
into static/images. When a recording changes a number, update the site too.

## Next

Approved with the plan on October 18 2026, to start when the owner says go:

- PostgreSQL support (v0.2)
- CSV seed files for long lists such as countries and currencies
- `plan -markdown`, so a CI step can post the seed diff on a pull request
