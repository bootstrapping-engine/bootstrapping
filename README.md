# Bootstrapping

Bootstrapping keeps the reference data an application ships with (roles,
permissions, plan tiers, workflow states, country lists) in step with its
SQLite database, without undoing what admins changed.

Loading that data once is easy. Updating it later is where it hurts. Your
app ships v2 of its roles, but a customer's admin renamed one of them last
month, and the usual upgrade script either overwrites the rename or never
delivers the new description. Bootstrapping remembers what it applied last
time, in a small ledger inside the same database, and merges every value
three ways: the new seed, the live row and the last-applied seed.

| Seed since last apply | Live row since last apply | Result |
|---|---|---|
| unchanged | unchanged | nothing to do |
| changed | unchanged | the seed's value lands |
| unchanged | changed | the admin's edit stays |
| changed | changed to the same value | nothing to do |
| changed | changed, differently | a **conflict**: nothing is written until someone decides |

Rows get the same treatment. A row the admin deleted stays deleted. A row
the seed adds appears. A row dropped from the seed is left alone, deleted or
retired, as you choose, and a delete never takes a row someone edited
without asking first.

Open source under the Apache License 2.0. A Go library and a command,
`bootstrapping` (or `bst` for short). Site: [bootstrapping.org](https://bootstrapping.org).

## Install

Ready-built binaries for Linux, macOS and Windows are on the
[releases page](https://github.com/bootstrapping-engine/bootstrapping/releases/latest).
Each archive holds `bootstrapping`, `bst` and `bootstrapping-demo`.

```sh
tar -xzf bootstrapping_linux_amd64.tar.gz
sudo mv bootstrapping bst bootstrapping-demo /usr/local/bin/
bst version
```

The macOS binaries aren't signed by Apple; allow them once in System Settings
under Privacy & Security, or run `xattr -d com.apple.quarantine bootstrapping bst bootstrapping-demo`.

From Go (the command needs a C compiler, since its SQLite driver is cgo):

```sh
go install github.com/bootstrapping-engine/bootstrapping/cmd/bootstrapping@latest
```

## The demo

```sh
bootstrapping-demo
```

It runs an application's roles and permissions through three seed versions.
In between, an admin renames the Editor role and takes a permission away.
The third version renames Editor too, which is a conflict; the demo settles
it, kills the apply with SIGKILL after its first statement, shows the
database untouched, runs it again, and ends with "All checks passed." This
is its plan for the second version:

```text
permissions
  insert       billing.view
roles
  update       editor                description  "Writes and publishes posts" -> "Writes, edits and publishes posts"
  keep edit    editor                name         local "Content Editor"; the seed still says "Editor"
  update       viewer                name         "Viewer" -> "Reader"
role_permissions
  insert       admin, billing.view
  keep delete  editor, posts.delete               deleted locally; it stays deleted

Pending: 2 inserts, 2 updates, 1 local edit kept, 1 local delete kept.
```

The seeds, the schema and the program are in [examples/permissions](examples/permissions).

## Seed files

One TOML file per table, in a directory:

```toml
# seeds/roles.toml
table = "roles"
key = "code"                          # the natural key that identifies a row
on_remove = { retire = "retired_at" } # or "keep" (the default) or "delete"

[columns]
system = "seed"                       # the seed always wins
notes = "local"                       # set on insert, then the admin's
                                      # every other column is "merge"

[[row]]
code = "editor"
name = "Editor"
description = "Writes and publishes posts"
system = true
```

Tables that link to each other use natural keys, and Bootstrapping turns
them into ids:

```toml
# seeds/role_permissions.toml
table = "role_permissions"
key = ["role_id", "permission_id"]
on_remove = "delete"

[refs]
role_id = "roles.code"
permission_id = "permissions.code"

[[row]]
role_id = "editor"
permission_id = "posts.publish"
```

A renamed row keeps its id and its links with `_renamed_from = "author"`.
[docs/SEEDS.md](docs/SEEDS.md) has every setting.

Bootstrapping fills tables; it doesn't create them. Your migrations do that,
and Bootstrapping runs after them.

## The command

```text
bst plan    -db app.db -seeds seeds    what an apply would do
bst apply   -db app.db -seeds seeds    do it, in one transaction
bst status  -db app.db -seeds seeds    one line
bst explain -db app.db -seeds seeds roles editor
bst history -db app.db
```

`-db` and `-seeds` can come from `BOOTSTRAPPING_DB` and
`BOOTSTRAPPING_SEEDS`. Every command takes `-json`.

Exit codes for `plan` and `status`: 0 in step, 2 an apply would write
something, 3 a conflict blocks it, 1 an error. `apply` exits 0 when done
and 3 when blocked, having written nothing.

### Conflicts

A conflict stops the apply until someone decides. Decide once, in the seed
file, for a column or a whole table:

```toml
[columns]
name = { own = "merge", on_conflict = "local" }   # "fail" (default), "seed" or "local"
```

or for one apply, from the command line:

```sh
bst apply -db app.db -seeds seeds -take roles/editor/name=local
bst apply -db app.db -seeds seeds -take seed       # every conflict
```

Once settled, a conflict doesn't come back: the ledger records the seed's
value as applied, so the admin's value now counts as a local edit.

`explain` shows one row value by value, with its history. This is the
demo's Editor role at the end, after its conflict was settled:

```text
$ bst explain -db app.db -seeds seeds roles editor
roles editor
  in the seed, in the table, in the ledger

  column       own    last applied                         seed                                 table                                verdict
  description  merge  "Writes, edits and publishes posts"  "Writes, edits and publishes posts"  "Writes, edits and publishes posts"  in sync
  name         merge  "Editorial"                          "Editorial"                          "Content Editor"                     kept local edit
  system       seed   1                                    1                                    1                                    in sync

An apply would:
  keep edit name: local "Content Editor"; the seed still says "Editorial"

History:
  run 1  2026-11-02T08:00:00Z  insert  (description "Writes and publishes posts", name "Editor", system 1)
  run 2  2026-11-02T08:00:00Z  update description: "Writes and publishes posts" -> "Writes, edits and publishes posts"
  run 3  2026-11-02T08:00:00Z  conflict name: kept "Content Editor" over the seed's "Editorial"
```

## The Go library

The library works with any `database/sql` driver for SQLite and doesn't
import one. Ship the seeds inside your binary and apply them at startup:

```go
import "github.com/bootstrapping-engine/bootstrapping"

//go:embed seeds
var seeds embed.FS

set, err := bootstrapping.LoadFS(seeds, "seeds")
plan, err := bootstrapping.Apply(ctx, db, set, nil)
if errors.Is(err, bootstrapping.ErrBlocked) {
	// plan.Changes lists the conflicts; nothing was written
}
```

`Preview` plans without writing, `Explain` reports one row, `History` lists
the applies. [docs/REFERENCE.md](docs/REFERENCE.md) has the whole API.

## How it works

Each apply opens one connection and takes SQLite's write lock with
`BEGIN IMMEDIATE`, plans again inside the transaction, writes the data and
the ledger, and commits. A crash leaves the database as it was or as the
apply leaves it. Two applies at once take turns: the second waits for the
lock and finds nothing to do.

Values are compared the way SQLite stores them. A seed's `1` in a TEXT
column is the text `"1"`, as SQLite would have stored it, so it doesn't
look like a change. [docs/MERGE.md](docs/MERGE.md) is the full set of rules,
row by row and value by value.

## How it was tested

Every number here comes from [test/results](test/results), recorded by
`sh scripts/record-tests.sh` at ten times the size CI runs on every push.

- **The promise, on random histories.** 1,200 histories of 12 seed versions
  each, with an admin editing, deleting and adding rows between versions.
  After each of the 14,400 applies, 492,228 values in all were checked
  against the merge rules, worked out from the test's own record of what
  was applied. 47,330 local edits kept, 25,870 seed changes landed, 21,473
  conflicts settled either way, 3,618 applies blocked with nothing written,
  and no value overwritten without a conflict.
- **Killed halfway.** 400 applies of a 1,500-row seed, killed with SIGKILL
  after a random statement or at a random moment. Every one left the
  database exactly as it was (248) or exactly as a finished apply leaves it
  (152), passed SQLite's integrity check, and reached the clean result when
  applied again.
- **Many processes, one file.** 60 rounds of eight processes applying the
  same seed at once. Each round, exactly one wrote and seven found nothing
  to do, and the file matched a single apply.
- **Types, against SQLite itself.** 4,051 values in 16 declared column
  types, 129,632 checks that the engine's conversion matches what SQLite
  stores.

The suite also tests that it can fail: making the merge overwrite local
edits, or let conflicts through, or delete edited rows, each turns it red.

## Limits in version 0.1

- SQLite only. PostgreSQL is the plan for the next version.
- A reference names a row by one column; composite references aren't
  supported.
- Seeds are for reference data: thousands of rows, not millions. Every
  managed table is read in full on each plan.

## License

Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE) and
[THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
