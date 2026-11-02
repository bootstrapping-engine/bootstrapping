# Third-party licenses

Bootstrapping is under the Apache License 2.0 (see [LICENSE](LICENSE) and
[NOTICE](NOTICE)). The Go package uses BurntSushi/toml and imports no
database driver. The bootstrapping, bst and bootstrapping-demo commands and
their ready-built binaries also contain the Go modules below. Every release
archive carries a THIRD_PARTY_LICENSES.md with the full license text of
each module actually compiled in, written by
[scripts/third-party-licenses.sh](scripts/third-party-licenses.sh) from the
module sources at build time. Run that script to see the same texts for any
commit.

| Module | Version | License |
|---|---|---|
| github.com/BurntSushi/toml | v1.6.0 | MIT |
| github.com/lib/pq | v1.12.3 | MIT |
| github.com/go-sql-driver/mysql | v1.10.1 | Mozilla Public License 2.0 |
| filippo.io/edwards25519 | v1.2.0 | BSD 3-Clause |
| modernc.org/sqlite | v1.60.1 | BSD 3-Clause; the SQLite code it is translated from is in the public domain |
| modernc.org/libc | v1.77.1 | BSD 3-Clause, plus the third-party notices in its source |
| modernc.org/mathutil | v1.7.1 | BSD 3-Clause |
| modernc.org/memory | v1.12.1 | BSD 3-Clause |
| golang.org/x/sys | v0.48.0 | BSD 3-Clause |
| github.com/google/uuid | v1.6.0 | BSD 3-Clause |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748 | BSD 3-Clause |
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/mattn/go-isatty | v0.0.24 | MIT |
| github.com/ncruces/go-strftime | v1.0.0 | MIT |
| The Go runtime and standard library | 1.26 | BSD 3-Clause |

The list follows go.mod. A module that go.mod names but the commands don't
compile in is left out of the archive's file.

go-sql-driver/mysql is under the Mozilla Public License 2.0, a file-level
copyleft licence. Bootstrapping uses it unchanged; its source code is
available at https://github.com/go-sql-driver/mysql (version v1.10.1).

The binaries are pure Go (built with CGO_ENABLED=0), so no C library is
copied into them. The macOS binaries call the operating system's own
libSystem, and the Windows binaries its system DLLs; neither is copied into
the archives. Bootstrapping 0.2 used the cgo driver github.com/mattn/go-sqlite3
and linked the mingw-w64 runtime on Windows; neither is in 0.3 or later.
