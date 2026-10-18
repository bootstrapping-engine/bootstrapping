# Seed files

A seed set is a directory of `.toml` files, one per table. Files are read in
name order, then sorted so a table comes after the tables it references.
Other files in the directory are ignored.

```toml
table = "roles"                        # required: the table to fill
key = "code"                           # required: a column, or a list of columns
on_remove = { retire = "retired_at" }  # "keep" (default), "delete", or { retire = "column" }
on_conflict = "fail"                   # "fail" (default), "seed" or "local"
on_local_delete = "respect"            # "respect" (default) or "restore"

[columns]
system = "seed"
notes = "local"
name = { own = "merge", on_conflict = "local" }

[refs]
parent_id = "roles.code"

[[row]]
code = "editor"
name = "Editor"
system = true
parent_id = "admin"
```

Anything else at the top level is an error, so a typo doesn't pass silently.

## table and key

`table` names a table that already exists. Bootstrapping doesn't create or
alter tables.

`key` is the natural key: the column or columns that identify a row across
versions and across databases, such as a code or a slug. Every row must set
every key column, and no two rows may share a key. The key isn't the
table's primary key; an `id INTEGER PRIMARY KEY` the database fills is
fine and usual.

## Rows

Each `[[row]]` sets column values. Values can be strings, integers, floats,
booleans (stored as 1 and 0) and TOML dates and times. A local date
(`2026-10-18`) is stored as `"2026-10-18"`, a local time as `"09:30:00"`, a
date-time with an offset in UTC. Lists and tables can't be stored; write
them as a string if you need to.

A column a row leaves out is NULL for that row. The managed columns are the
key, every column any row sets, and every column under `[columns]`.

Values are converted the way SQLite would store them in that column, using
its type affinity: `1` in a TEXT column is the text `"1"`; `"42"` in an
INTEGER column is the integer 42.

A table with a NOT NULL column that has no default must have it managed by
the seed; otherwise inserts would fail, so the plan stops early and says so.

### Renames

To change a row's key without losing its id, its links or its admin's
edits, give the old key:

```toml
[[row]]
code = "editor"
_renamed_from = "author"     # a list for a composite key
```

If the ledger knows `author` and the table has no `editor` yet, the apply
renames the row in place. Once applied, the hint does nothing and can stay
or go.

## columns: who owns a value

| Ownership | Meaning |
|---|---|
| `merge` | The default. A three-way merge: an admin's edit stays, a seed change to an untouched value lands, both changed differently is a conflict. |
| `seed` | The seed always wins. Use it for values the application relies on, like a system flag. |
| `local` | The seed sets it when the row is inserted and never again. Use it for values that are the admin's from day one. |

A column can carry its own conflict rule: `name = { own = "merge", on_conflict = "local" }`.

## refs: links by natural key

```toml
[refs]
role_id = "roles.code"
```

The seed writes `role_id = "editor"`; the table stores the id of the
`roles` row whose `code` is `editor`. The referenced table needs a single
primary key column or a rowid, and the column named must tell its rows
apart. References into another seed table see that table as the apply will
leave it, so a link to a role added in the same apply works, and a table
can refer to itself (a role's parent role).

A row whose reference names a row that doesn't exist is skipped, not
guessed at, and tried again on the next apply.

## on_remove: rows dropped from the seed

| Setting | What happens |
|---|---|
| `"keep"` | The default. The row stays as it is and is no longer managed. |
| `"delete"` | The row is deleted, unless an admin edited one of its merge columns since the last apply: then it's a conflict. |
| `{ retire = "retired_at" }` | The named column is set to the time of the apply. If the row comes back into the seed, the column is set back to NULL. The retire column can't be a managed column. |

## on_conflict

What happens when a value changed in the seed and locally, differently, or
a row was already in the table with a different value before Bootstrapping
first saw it, or a dropped row was edited:

| Setting | What happens |
|---|---|
| `"fail"` | The default. The apply writes nothing and lists the conflicts. |
| `"seed"` | The seed's value is written. |
| `"local"` | The live value stays. |

Set it for the table, or per column under `[columns]`. A `-take` given to
`apply` (or a `Resolution` in Go) overrides both.

## on_local_delete

A row the seed still has, which an admin deleted after it was applied:

| Setting | What happens |
|---|---|
| `"respect"` | The default. It stays deleted. |
| `"restore"` | It is inserted again. |
