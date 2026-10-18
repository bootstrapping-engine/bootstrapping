# Reference

## The command

```text
bootstrapping <command> [flags]      (or: bst <command> [flags])
```

Flags can go before or after a command's other arguments; `--` ends them.

| Command | Does | Exit codes |
|---|---|---|
| `plan [-v]` | lists what an apply would do; `-v` also lists rows that only need recording | 0 in step, 2 pending, 3 blocked, 1 error |
| `apply [-take ...]` | does it, in one transaction | 0 done or nothing to do, 3 blocked (nothing written), 1 error |
| `status` | one summary line | as `plan` |
| `explain TABLE KEY` | one row, value by value, with its history | 0, 1 error |
| `history [-n N]` | the applies the ledger recorded, newest first | 0, 1 error |
| `init` | creates the ledger tables (apply does this on its own) | 0, 1 error |
| `version` | prints the version | 0 |

Flags for every command:

| Flag | Meaning |
|---|---|
| `-db FILE` | the SQLite database, or `$BOOTSTRAPPING_DB`; it must exist. Any file name works, `?` and `#` included |
| `-seeds DIR` | the seed directory, or `$BOOTSTRAPPING_SEEDS`, or `seeds` |
| `-json` | JSON instead of text; errors, flag errors included, as `{"error": "..."}` |

`-take` settles conflicts for one apply, and can be repeated. The first that
matches a conflict wins:

```text
-take local                        every conflict keeps the local value
-take seed                         every conflict takes the seed's value
-take roles=local                  every conflict in roles
-take roles/editor=seed            every conflict in that row
-take roles/editor/name=local      that value
-take 'links/editor, posts.publish=seed'   a composite key, as the plan prints it
```

KEY in `explain` and `-take` is the natural key as the plan prints it, with
a comma between the parts of a composite key. In `-take`, a key that has a
`/` in it needs the column as well (`-take 'pages/docs/intro/title=seed'`
reads as table `pages`, key `docs/intro`, column `title`); without one, the
last part would be taken for a column.

### Plan lines

| Label | Meaning |
|---|---|
| `insert` | a seed row that isn't in the table yet |
| `update` | a value changed in the seed and not locally, or a seed-owned value |
| `rename` | a row's key changed (`_renamed_from`) |
| `delete` | dropped from the seed with `on_remove = "delete"` |
| `retire` / `unretire` | dropped from the seed with retire, or back again |
| `keep edit` | edited locally, unchanged in the seed: stays |
| `keep delete` | deleted locally: stays deleted |
| `CONFLICT` | needs a decision; shows `(settled: ...)` once one is given |
| `release` | dropped from the seed with `on_remove = "keep"`: no longer managed |
| `skip` | refers to a row that doesn't exist; tried again next time |
| `record` | already matches the seed; the ledger records it (`-v`) |

### JSON plan

```json
{
  "pending": true, "blocked": false, "applied": false,
  "seed_hash": "...", "summary": "Pending: 2 inserts, 2 updates.",
  "counts": {"inserts": 2, "updates": 2, "conflicts": 0, "resolved": 0, "...": 0},
  "changes": [
    {"table": "roles", "key": ["viewer"], "key_text": "viewer", "action": "update",
     "column": "name", "base": {"value": "Viewer", "type": "text"},
     "seed": {"value": "Reader", "type": "text"}, "live": {"value": "Viewer", "type": "text"}}
  ]
}
```

After an apply, `"applied": true` and `"run"` gives the run's number in the
ledger. Values carry their SQLite type: `null`, `integer`, `real`, `text` or `blob`
(blobs as base64).

## The Go package

```go
import "github.com/bootstrapping-engine/bootstrapping"
```

| Function | Does |
|---|---|
| `LoadDir(dir) (*Set, error)` | reads a seed directory |
| `LoadFS(fsys, dir) (*Set, error)` | reads one from an `fs.FS`, such as an `embed.FS` |
| `ParseTable(name, data) (*Table, error)` | reads one seed file |
| `Preview(ctx, db, set, opts) (*Plan, error)` | plans without writing, in a read transaction |
| `Apply(ctx, db, set, opts) (*Plan, error)` | plans and writes in one transaction; `ErrBlocked` with the plan if a conflict stops it |
| `Explain(ctx, db, set, table, key, opts) (*RowReport, error)` | one row: fields, pending changes, history |
| `History(ctx, db, n) ([]Run, error)` | the last n applies, newest first |
| `Init(ctx, db) error` | creates the ledger tables |

`db` is a `*sql.DB` from any SQLite driver. The package doesn't import one.

```go
type Options struct {
	Take     []Resolution        // settles conflicts; first match wins
	Now      func() time.Time    // the clock for retire times and runs
	Progress func(Change)        // called after each statement Apply writes, inside the transaction
}

type Resolution struct {
	Table, Key, Column string    // empty matches anything
	Take               Take      // TakeSeed or TakeLocal
}
```

A `Plan` has `Changes`, and `Counts()`, `Blocked()`, `Pending()` and
`Writes()`. Each `Change` has the `Table`, the `Key`, an `Action`, the
`Column` for value-level changes, the `Base`, `Seed` and `Live` values, and
for conflicts the `Conflict` kind and how it was `Resolved`.

Apply holds one connection for the whole transaction and starts it with
`BEGIN IMMEDIATE`. If the connection's busy timeout is 0, it waits up to 30
seconds for another writer and restores the setting afterwards.

## The ledger

Four tables in the application's database, created on the first apply:

| Table | Holds |
|---|---|
| `_bootstrapping_rows` | per managed row: table, natural key, the values last applied, retired or not, and the run that wrote it |
| `_bootstrapping_runs` | per apply: when, the seed set's hash, the version, and counts |
| `_bootstrapping_changes` | per write: run, table, key, action, column, old and new value |
| `_bootstrapping_refs` | per reference column: the `[refs]` entry last applied, so a column the seed stops naming is still read by natural key |

Values are kept losslessly as `n` (NULL), `i:42`, `r:1.5`, `t:text` or
`b:base64`. The ledger is written in the same transaction as the data, so
the two never disagree.

Deleting the ledger tables makes every row look unmanaged; the next apply
adopts the rows that match and reports conflicts for the ones that don't.
