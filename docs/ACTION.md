# The GitHub Action

`bootstrapping-engine/bootstrapping` is also a GitHub Action. On a pull
request it plans the seed folder against a database, posts the plan as a
comment, updates that same comment on every later push, and fails the
check when the plan has a conflict nobody settled, which would stop an
apply. The plan also goes to the job summary.

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
      # A database that looks like production: your migrations, then the
      # base branch's seeds, so the plan is exactly what this pull request
      # changes.
      - name: Database
        run: |
          curl -sSL https://github.com/bootstrapping-engine/bootstrapping/releases/latest/download/bootstrapping_linux_amd64.tar.gz | tar xz
          sqlite3 ci.db < schema.sql
          git worktree add base "origin/$GITHUB_BASE_REF"
          ./bst apply -db ci.db -seeds base/seeds
      - uses: bootstrapping-engine/bootstrapping@v0.3.0
        with:
          database: ci.db
          seeds: seeds
```

Pointing it at a staging server works the same way, with the URL in a
secret: `database: ${{ secrets.STAGING_DATABASE_URL }}` (postgres://,
mysql:// or mariadb://). The action only plans; it never writes to the
database.

## Inputs

| Input | Default | |
|---|---|---|
| `database` | (required) | a SQLite file or a postgres://, mysql:// or mariadb:// URL, as `-db` takes it |
| `seeds` | (required) | the seed folder, relative to the workspace |
| `comment` | `true` | post the plan on the pull request; `false` for the job summary only |
| `fail-on-conflict` | `true` | fail the step on a conflict nobody settled; `false` to report only. An error always fails |
| `version` | the action's own | a release tag such as `v0.3.0`, or `latest`. Empty runs the release the action was pinned to (`@v0.3.0` runs 0.3.0) |
| `pull-request` | the event's | the pull request to comment on; a push has none, so nothing is posted |
| `github-token` | `github.token` | the token that posts the comment |
| `bootstrapping-path` | | a bootstrapping binary to run instead of downloading one |

`comment` and `fail-on-conflict` take exactly `true` or `false`; anything
else fails the step, so a typo can't let a conflict through.

## Outputs

| Output | |
|---|---|
| `result` | `in-step`, `pending`, `blocked` or `error` |
| `exit-code` | the plan's exit code: 0 in step, 2 pending, 3 blocked, 1 error |
| `plan` | the file holding the plan as Markdown |
| `comment-url`, `comment-id` | the comment, when one was posted or updated |
| `version` | the release that ran: a tag, `latest`, or `local` |

## How it works

The action downloads the release archive for the runner (Linux and macOS
on x86-64 or ARM64, Windows on x86-64), checks it against the release's
SHA256SUMS, and runs `bootstrapping plan -markdown`. Each seed folder gets
one comment, found again by a hidden marker at its top
(`<!-- bootstrapping-plan seeds=... -->`), so two folders in one workflow
get a comment each and a new push edits the comment instead of adding
another. When planning fails, the comment and the summary show the error
(never the database URL). A pull request from a fork gets a read-only
token; the comment step then only warns, and the plan is still in the job
summary.

The action is tested by
[.github/workflows/action-test.yml](../.github/workflows/action-test.yml)
on Linux, macOS and Windows with a binary built from each commit: a fresh
database, one in step, a conflict reported and then failing, a database
that isn't there and a mistyped switch. A second job posts plans to a
closed issue kept for the purpose and checks that a second run edits the
same comment. After each release the same workflow runs the action on the
published binaries, pinned to the new tag and as `latest`.
