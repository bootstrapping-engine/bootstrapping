# Releasing Bootstrapping

Releases go out on their own. Nobody clicks Publish. This file is the record
of how that works, for whoever (person or Claude session) works on this repo
next.

## The short version

1. Refresh `test/results` with the `record` workflow (it runs every test on
   SQLite, PostgreSQL 16 and MySQL 8.4 and commits the files):
   `gh api -X POST repos/bootstrapping-engine/bootstrapping/actions/workflows/record.yml/dispatches -f ref=main`.
   Its commit starts no other workflow, so record before raising the
   version.
2. Raise `const Version = "X.Y.Z"` in `bootstrapping.go`.
3. Write what changed into the commit message.
4. Push the commit to `main`.

When the `test` workflow passes on that commit (the full suite and the smoke
check on Linux, macOS and Windows), the `release` workflow builds the
binaries, smoke-tests each on its own kind of machine, then tags the commit
`vX.Y.Z` and publishes the release "Bootstrapping vX.Y.Z" with the archives
and SHA256SUMS attached. If any build or check fails, nothing is tagged and
nothing goes public. Fix it, push again, and the next green run picks it up.

Pushing without raising the version is fine: the release workflow sees the
tag already exists and stops quietly.

## What it does, step by step

[.github/workflows/release.yml](.github/workflows/release.yml) starts when a
`test` run finishes; the push alone doesn't start it.

- **plan.** Goes on only if the `test` run passed, came from a push to
  `main` in this repo, and `main` still points at that commit (if `main`
  moved on, the run for the newer commit decides). Reads `Version` from
  `bootstrapping.go`. Stops quietly if `vX.Y.Z` is already a tag. Fails
  loudly if the version is lower than the newest tag.
  [scripts/release-plan.py](scripts/release-plan.py) has the rules, and
  [scripts/release_plan_test.py](scripts/release_plan_test.py) its cases.
- **build.** Everything is pure Go (CGO_ENABLED=0), so the binaries are
  static. Each target builds from that exact commit: Linux x86-64 and ARM64
  (statically linked, checked), macOS Apple silicon, macOS Intel
  (cross-built on Apple silicon, checked for its architecture) and
  Windows. Each set except macOS Intel runs [scripts/smoke.py](scripts/smoke.py)
  where it was built. Then it is packed, with a THIRD_PARTY_LICENSES.md
  written by [scripts/third-party-licenses.sh](scripts/third-party-licenses.sh)
  from the modules actually compiled in.
- **release.** Runs only after every build and check passed. Writes
  SHA256SUMS and the notes, then creates the tag and the release in one
  step with `gh release create --target <commit>`, on the repo's own
  `GITHUB_TOKEN` (`contents: write` on this job only). A `concurrency` group
  per tag means two runs for the same version can't publish twice.

The smoke check, on each binary set: all three binaries report the expected
version; the demo passes its own checks, including the apply it kills
halfway; Python drives the command through the demo's story, making the
admin's edits with its own sqlite3 module, and checks every exit code, the
JSON plans and the tables; and eight processes apply the same seed to one
file at once, with exactly one writing.

## Files that go out

| System | File |
|---|---|
| Linux, x86-64 (static) | bootstrapping_linux_amd64.tar.gz |
| Linux, ARM64 (static) | bootstrapping_linux_arm64.tar.gz |
| macOS, Apple silicon | bootstrapping_darwin_arm64.tar.gz |
| macOS, Intel | bootstrapping_darwin_amd64.tar.gz |
| Windows, x86-64 | bootstrapping_windows_amd64.zip |
| Checksums | SHA256SUMS |

Each archive holds `bootstrapping`, `bst` and `bootstrapping-demo`, with
LICENSE, NOTICE, THIRD_PARTY_LICENSES.md and README.md. Archive names carry
no version on purpose: links like
`https://github.com/bootstrapping-engine/bootstrapping/releases/latest/download/bootstrapping_linux_amd64.tar.gz`
keep working from one release to the next, so the README and the site never
need editing for a release.

## After a release

A Claude session can't create tags or releases itself, which is why the
workflow does it on the repository's token. After pushing a raised version,
check:

1. The `test` run and then the `release` run finished green.
2. The release exists with all six files.
3. The `latest/download` links resolve, and a downloaded archive's SHA256
   matches SHA256SUMS.
4. A downloaded Linux binary runs: `bst version` names the release, and
   `bootstrapping-demo` ends with "All checks passed."
5. The `action-test` run started by the release passed its download job:
   the GitHub Action ran the published binaries on Linux, macOS and
   Windows, pinned to the new tag and as `latest`.

A new dependency is resolved by the `tidy` workflow, since build sessions
can't reach the Go module proxy: add the import, push, start
`gh api -X POST repos/bootstrapping-engine/bootstrapping/actions/workflows/tidy.yml/dispatches -f ref=main`,
and pull its go.mod and go.sum commit.

## By hand

- A release published by hand gets the binaries attached by the same
  workflow.
- To rebuild the files of an existing tag, run the `release` workflow by
  hand with the tag (`gh workflow run release --repo bootstrapping-engine/bootstrapping -f tag=vX.Y.Z`).
