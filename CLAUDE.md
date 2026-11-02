# Notes for Claude

This file says where the next session starts and what comes next. What comes
next is decided with the owner before it goes here, and work starts only when
the owner says go.

## Releases

Releases are automated. To release, raise `Version` in `bootstrapping.go` and
push to `main`; never ask the owner to publish one. [RELEASING.md](RELEASING.md)
has the procedure and the checks to run afterwards.

## Where things stand

v0.1.0 was the first release (October 2026). v0.2.0 (October 2026) added
PostgreSQL 12+ support, CSV seed files (`rows = "file.csv"`) and
`plan -markdown` for pull request comments. v0.2.1 (October 2026) made the plan stop on a row that
leaves out a managed NOT NULL column, found while writing the case studies:
four example programs in `examples/` (plans, countries, workflow,
templates) on `examples/internal/story`, run by `go test`, each with a
case study post on the site. Apache 2.0, CI on Linux, macOS
and Windows plus a PostgreSQL job (13 and 18), and automated releases with
smoke-tested binaries for Linux, macOS and Windows. Bootstrapping is a seed
reconciler: TOML or CSV seed files per table, a three-way merge per value
(new seed, live row, last-applied seed from a ledger in the same
database), per-column ownership (merge, seed, local), conflicts that block
the apply until settled (`on_conflict` in the seed, `-take` on the
command), `on_remove` keep/delete/retire, `on_local_delete`
respect/restore, `_renamed_from`, references by natural key (`[refs]`,
including a table referring to itself), and one transaction per apply.

The library (root package) works with any `database/sql` driver for SQLite
or PostgreSQL and imports none; `dialect.go` tells them apart and holds
what differs (placeholders, schema reading, value comparison, locking).
On SQLite an apply is one `BEGIN IMMEDIATE` transaction; on PostgreSQL a
session advisory lock (two keys: Bootstrapping, current schema) and one
REPEATABLE READ transaction, values compared as the text the server prints
under fixed session settings, seed values cast by the server. The commands
(`cmd/bootstrapping`, `cmd/bst`, and the demo `examples/permissions`,
shipped as `bootstrapping-demo`) use mattn/go-sqlite3, which is cgo, and
lib/pq (pure Go; pgx couldn't be fetched, its module graph needs
gopkg.in). Builds need a C compiler, and the release workflow builds each
target on its own kind of machine (MSYS2 MinGW on Windows).
modernc.org/sqlite was the first choice but is hosted on GitLab, which the
build sessions can't reach.

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
`test/results/`, with JSON figures from the promise, references, crash,
process and affinity tests. With `BS_POSTGRES` set to a server URL, the
tests that run on every database (`eachBackend`) run on PostgreSQL too,
each in its own schema, and write `*-postgres.json`; record with it set.
PostgreSQL 16 is installed in the build image: initdb into /var/tmp as the
postgres user and start it on a socket in /tmp. Every number in the README
and on the site comes from those files; record again after any change that
could move them. It needs Go, a C compiler and Python 3.

## Site

Bootstrapping.org is in `photozeitgeist/bootstrapping.org` (October 2026):
the Hugo Launch template copied unchanged from multivergence.com, with
content and config values only. The old bootstrapping.org site was removed
entirely, history included, at the owner's request. Pages: about, contact,
quick start, download, reference. Posts: the launch, a plain explainer of
reference data drift, how it was tested, the name, the 0.2 release, and
four case studies tagged "case studies", one per example program. Images are drawn
into static/images. When a recording changes a number, update the site too.

## In progress: Bootstrapping 0.3 (softdev-plan item 29, approved by YK 2026-11-02)

What and why: most Rails, Laravel and Django apps with reference data run on
MySQL or MariaDB, which Bootstrapping can't touch; `plan -markdown` exists but
nothing posts it on a pull request; and a referenced row can only be named by
one column, while join tables are often keyed on two (region plus code).

Design decisions:
- Phase A, MySQL 8.0+ and MariaDB 10.6+ as a third dialect. Detected after
  the PostgreSQL and SQLite probes. Each plan or apply runs on one session with
  fixed settings, restored afterwards: sql_mode
  `ANSI_QUOTES,STRICT_ALL_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_AUTO_VALUE_ON_ZERO,NO_BACKSLASH_ESCAPES`
  (so the package's double-quoted names work) and time_zone `+00:00`.
  An apply takes `GET_LOCK` on a name built from the database, then one
  REPEATABLE READ transaction. Values are compared as the text the server
  prints (`CAST(col AS CHAR)`, HEX for binary types), like PostgreSQL;
  integer, float and binary columns become Integer, Real and Blob Values
  (BIGINT UNSIGNED stays text). Seed values are converted by the server: they
  are inserted, in strict mode, into a temporary table with the column's own
  type, character set and collation, and read back, so a value that doesn't
  fit is reported by name and nothing is ever stored shortened. The ledger
  gets MySQL DDL (no TEXT keys: table names VARCHAR(64), row keys
  VARBINARY(2816), binary collations) and is created before the transaction,
  since DDL commits implicitly there. The ledger's `key` column is quoted
  everywhere (a reserved word in MySQL). Upserts use ON DUPLICATE KEY UPDATE.
  A referenced table needs a one-column primary key. Collations that ignore
  case or trailing spaces are documented: keys must match exactly.
  The library still imports no driver; the commands and tests use
  go-sql-driver/mysql. CI runs the every-backend tests on MySQL 8.4 and
  MariaDB 11 service containers (`BS_MYSQL`).
- Phase B1, the Action: `action.yml` (composite) downloads the release
  binary for the runner, runs `bootstrapping plan -markdown` against the
  database and seeds it's given, posts or updates one pull request comment
  (found by a hidden marker), and fails the step when the plan has
  conflicts. Tested by a workflow on a real pull-request-like run with a
  SQLite file.
- Phase B2, composite references: `[refs] role_id = "roles.(region, code)"`
  names the referenced row by several columns; the seed gives an array
  (`role_id = ["eu", "editor"]`, in CSV the cell is the values joined by
  `|`). Stored ids are unchanged. The ledger remembers the tuple with a new
  encoding (`k:` + JSON list of value encodings); existing encodings keep
  their meaning. Everything a single-column reference does (self references,
  renames followed, rows protected from deletion) works the same.
- Upkeep: pure-Go SQLite (modernc.org/sqlite v1.60.1) for the commands and
  tests, so builds need no C compiler; go.mod to 1.26 and the Go 1.24 CI job
  dropped; CLAUDE.md "after 0.2.1".
- Version 0.3.0.

Steps:
1. [x] Upkeep: modernc SQLite, go 1.26, CI without the 1.24 job and cgo (84fd219, CI green on 3 OS).
2. [ ] Phase A: MySQL/MariaDB dialect, CI jobs, tests.
3. [x] Phase B2: composite references, tests (composite_test.go: TOML and CSV, skip, edits, renames, protected rows, self references, mistakes).
4. [ ] Phase B1: the Action and its test workflow.
5. [ ] Docs (README, SEEDS, REFERENCE, MERGE where it applies).
6. [ ] Independent review (at most two rounds), fixes, fresh test/results.
7. [ ] 0.3.0 released by the workflow, downloads checked.
8. [ ] bootstrapping.org post with its own SVG, pages; softdevstudio.com
   note; softdev-plan log row.
