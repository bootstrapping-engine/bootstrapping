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
| `-db DB` | a SQLite file, or a PostgreSQL URL (`postgres://` or `postgresql://`); or `$BOOTSTRAPPING_DB`. The file or database must exist. Any file name works, `?` and `#` included |
| `-seeds DIR` | the seed directory, or `$BOOTSTRAPPING_SEEDS`, or `seeds` |
| `-json` | JSON instead of text; errors, flag errors included, as `{"error": "..."}` |
| `-markdown` | `plan`, `status` and `apply` print the plan as GitHub-flavored Markdown, for a pull request comment; not with `-json` |

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

### The plan on a pull request

`-markdown` prints the summary, a warning when conflicts block the apply,
and a table of changes per seed table, with every value in a code span so
it shows as itself. The first line is `<!-- bootstrapping-plan -->`, so a CI
step can find the comment it posted before and replace it. Past 60,000
characters the table stops with a count of the changes left out, keeping
the comment under GitHub's limit.

The GitHub Action in this repository does the posting for you:
[ACTION.md](ACTION.md). By hand, a plan shows what a seed change does to a
database, so CI needs one that looks like production. A fresh database with
the base branch's seeds applied works well: the plan is then exactly what
the pull request changes.

```yaml
name: seed plan
on:
  pull_request:
    paths: ["seeds/**"]
permissions:
  contents: read
  pull-requests: write
jobs:
  plan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - name: Install bst
        run: |
          curl -sSL https://github.com/bootstrapping-engine/bootstrapping/releases/latest/download/bootstrapping_linux_amd64.tar.gz | tar xz
          echo "$PWD" >> "$GITHUB_PATH"
      - name: Plan the seed change
        run: |
          sqlite3 ci.db < schema.sql                       # your migrations here
          git worktree add base "origin/$GITHUB_BASE_REF"
          bst apply -db ci.db -seeds base/seeds > /dev/null
          bst plan -markdown -db ci.db -seeds seeds > plan.md || true
      - name: Comment
        env:
          GH_TOKEN: ${{ github.token }}
        run: gh pr comment "${{ github.event.number }}" --body-file plan.md --edit-last --create-if-none
```

`plan` exits 2 when there are changes and 3 when a conflict blocks them,
hence the `|| true`; drop it to fail the check on a conflict.

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
ledger. Values carry their type, as the plan compared them: `null`, `integer`, `real`, `text` or `blob`
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

`db` is a `*sql.DB` from any SQLite, PostgreSQL or MySQL driver; the package tells
which it is talking to and doesn't import a driver. The commands use
modernc.org/sqlite (pure Go), lib/pq and go-sql-driver/mysql.

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

Apply holds one connection for the whole transaction. On SQLite it starts
it with `BEGIN IMMEDIATE`; if the connection's busy timeout is 0, it waits
up to 30 seconds for another writer and restores the setting afterwards.

## PostgreSQL

PostgreSQL 12 or later. Give the command a URL:

```sh
bst plan -db 'postgres://app:secret@db.internal:5432/app?sslmode=require' -seeds seeds
```

Any parameter lib/pq takes goes in the URL; others, such as `search_path`,
are passed to the server as settings for the connection.

- **Tables** are named without a schema and found through the connection's
  `search_path` (`&search_path=app` in the URL). The ledger tables are
  created in the first schema of the path. Each schema has its own ledger
  and its own lock.
- **References**: the table a reference points at needs a one-column
  primary key; PostgreSQL has no rowid to fall back on. Identity and serial
  primary keys are the usual case and need nothing in the seed.
- **Values** are compared as the server prints them, under fixed settings
  (UTC, ISO dates, hex bytea), whatever the role's or the database's own
  settings. Seed values are cast to the column's type by the server,
  `numeric(10,2)`, enums, domains, arrays, json and jsonb included, so a
  value written differently but stored the same isn't a change. A value
  that doesn't fit its column stops the plan with the row and column named.
  Integer, floating-point and bytea values show as numbers and blobs in
  plans and the ledger; every other type shows as text.
- **Locking**: an apply takes a session advisory lock (one per schema), then
  plans and writes in a single REPEATABLE READ transaction. A second apply
  waits for the lock and finds nothing to do. Admins can keep working: if
  one edits a row after the apply planned and before it wrote that row, the
  apply fails with a serialization error and writes nothing. Applying
  again sees the edit and merges it as usual.
- **Preview** and `plan` run in a read-only REPEATABLE READ transaction and
  take no lock.
- **Values that don't fit** stop the plan with the row and column named,
  text too long for a `varchar(n)` or `char(n)` column included: a seed
  value is never stored cut short. `numeric(p,s)` rounds as PostgreSQL
  rounds it.
- **Identity and serial columns**: when the seed sets ids in one, the apply
  moves its sequence past them, so the application's next insert gets a
  fresh number. A `GENERATED ALWAYS` column (identity or computed) can't be
  set by the seed; the plan says so.
- **Retire columns** must be able to hold the time of the apply: text, date
  or timestamp.
- **Keys** should be types whose equality is their text: text, integers,
  uuid, dates. A key of `citext`, or a plain `numeric` written both `1.5`
  and `1.50`, compares differently in the plan than in the table.
- **Connection poolers**: the apply's lock and its transaction must stay on
  one server connection. Behind PgBouncer in transaction mode, connect the
  command or the library directly to the server, or use session mode.

## MySQL and MariaDB

MySQL 8.0 or later and MariaDB 10.6 or later. Give the command a URL that
names the database:

```sh
bst plan -db 'mysql://app:secret@db.internal:3306/app?tls=true' -seeds seeds
bst plan -db 'mariadb://app@localhost/app?socket=/run/mysqld/mysqld.sock' -seeds seeds
```

Other parameters go to the driver (go-sql-driver/mysql) as its own:
`tls=true` or `tls=skip-verify`, `timeout=10s`, and so on. A password with
`@` or `/` in it is written percent-encoded (`%40`, `%2F`). The library
takes any `*sql.DB` opened with that driver.

- **Session settings**: each plan or apply runs on one connection with
  sql_mode set to strict (`STRICT_ALL_TABLES` with `NO_ZERO_DATE` and the
  rest) and time_zone to `+00:00`, and puts both back as it found them
  before the connection returns to the pool. Names are quoted with
  backticks, so the connection's own sql_mode, `ANSI_QUOTES` or not,
  doesn't matter.
- **Values** are compared as the server prints them (`CAST(x AS CHAR)`, hex
  for binary types). Seed values are converted by the server: each is
  stored, in strict mode, in a temporary table whose column has the real
  column's type, character set and collation, and read back. So `7` in a
  `DECIMAL(10,2)` column is `7.00`, a `SET` value is put in its order, and a
  value that doesn't fit (text too long, a number out of range, a value
  not in the `ENUM`) stops the plan with the row and column named, rather
  than being stored cut short with a warning. Integer, floating-point and
  binary values show as numbers and blobs in plans and the ledger;
  `BIGINT UNSIGNED`, `DECIMAL` and every other type show as text.
- **Locking**: an apply takes a named lock (`GET_LOCK`, one per database),
  then plans and writes in a single REPEATABLE READ transaction, reading
  the managed tables with shared locks. A second apply waits for the lock
  and finds nothing to do. An admin who edits a managed row during an apply
  waits until it commits; the edit then lands on top and the next plan
  keeps it. If InnoDB has to break a deadlock between the two, it rolls
  back the smaller transaction, usually the admin's statement, which fails
  with an error and can be run again; nothing is written over silently.
- **Preview** and `plan` take no lock. They need to create temporary tables
  (the `CREATE TEMPORARY TABLES` privilege). On MySQL with
  `enforce_gtid_consistency` (common on managed MySQL), temporary tables
  inside a transaction need `binlog_format=ROW`, MySQL 8's default.
- **Engines**: managed and referenced tables must be on a transactional
  engine (InnoDB). A MyISAM or MEMORY table is refused before anything is
  written, since it would keep half an apply after a failure.
- **The ledger** tables are created before the apply's transaction, since
  MySQL commits on any `CREATE TABLE`, and only once a plan shows the apply
  will write. A row key may be up to 2,816 bytes.
- **Keys** are compared as exact text by Bootstrapping, while the server
  compares them under the column's collation. With a collation that
  ignores case or trailing spaces (MySQL's default `utf8mb4_0900_ai_ci`
  ignores case and accents), two seed keys that differ only that way are
  one row to the server: the plan inserts both and the insert fails on the
  unique key. Give key columns a binary collation (`utf8mb4_bin`, or
  `utf8mb4_0900_bin` on MySQL 8 to also tell trailing spaces apart), or
  keep keys distinct. Floating-point key columns aren't supported.
- **References**: the table a reference points at needs a one-column
  primary key. A row that refers to itself has that reference cleared
  before it is deleted, since InnoDB won't delete it otherwise; if that
  column is NOT NULL, the delete stops with the server's error.
- **Retire columns** must be able to hold the time of the apply: text,
  `DATE`, `DATETIME` or `TIMESTAMP` (written in UTC).

## The ledger

Four tables in the application's database (on PostgreSQL, in the first
schema of the search path), created on the first apply:

| Table | Holds |
|---|---|
| `_bootstrapping_rows` | per managed row: table, natural key, the values last applied, retired or not, and the run that wrote it |
| `_bootstrapping_runs` | per apply: when, the seed set's hash, the version, and counts |
| `_bootstrapping_changes` | per write: run, table, key, action, column, old and new value |
| `_bootstrapping_refs` | per reference column: the `[refs]` entry last applied, so a column the seed stops naming is still read by natural key |

Values are kept losslessly as `n` (NULL), `i:42`, `r:1.5`, `t:text` or
`b:base64`, and the value of a reference by several columns as `k:` and
the JSON list of its parts' encodings (`k:["t:eu","t:editor"]`). 0.2
can't read a ledger holding `k:` values, so once a seed uses a reference
by several columns, stay on 0.3 or later. The ledger is written in the same transaction as the data, so
the two never disagree.

Deleting the ledger tables makes every row look unmanaged; the next apply
adopts the rows that match and reports conflicts for the ones that don't.
