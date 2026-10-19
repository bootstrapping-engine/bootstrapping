// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

// Cases found by the independent review before 0.1.0. Each one used to make
// Preview approve a plan that Apply then failed on, or report a conflict
// nobody caused, or hide a write.

// agree applies files and checks the apply did exactly what Preview said,
// that it didn't fail, that foreign keys hold, and that a second apply is
// empty.
func (f *fixture) agree(files map[string]string, take ...bs.Resolution) *bs.Plan {
	f.t.Helper()
	set := setFrom(f.t, files)
	opts := &bs.Options{Now: testClock, Take: take}
	pre, err := bs.Preview(f.ctx, f.db, set, opts)
	if err != nil {
		f.t.Fatalf("preview: %v", err)
	}
	p, err := bs.Apply(f.ctx, f.db, set, opts)
	if err != nil {
		f.t.Fatalf("preview said %q, apply failed: %v", actions(pre), err)
	}
	if actions(pre) != actions(p) {
		f.t.Fatalf("preview and apply differ:\npreview %s\n  apply %s", actions(pre), actions(p))
	}
	if !isPG(f.db) { // PostgreSQL checks foreign keys on every statement
		if n := f.one("SELECT count(*) FROM pragma_foreign_key_check"); n != "0" {
			f.t.Fatalf("%s foreign key violations", n)
		}
	}
	again, err := bs.Apply(f.ctx, f.db, set, opts)
	if err != nil || again.Applied {
		f.t.Fatalf("second apply: applied=%v %v: %s", again.Applied, err, actions(again))
	}
	return p
}

// fixtureOn is a fixture on the given database, with foreign keys on.
func fixtureOn(t *testing.T, b backend, schema string) *fixture {
	t.Helper()
	db := b.open(t)
	mustExec(t, db, b.ddl(schema))
	return &fixture{t: t, db: db, ctx: context.Background()}
}

func fkFixture(t *testing.T, schema string) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "fk.db")+"?_foreign_keys=1&_busy_timeout=30000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, schema)
	return &fixture{t: t, db: db, ctx: context.Background()}
}

const selfSchema = `
CREATE TABLE perms (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE);
CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, perm_id INTEGER REFERENCES perms (id), parent_id INTEGER REFERENCES roles (id));`

func selfSeed(roles string) map[string]string {
	return map[string]string{
		"perms.toml": "table = \"perms\"\nkey = \"code\"\n[[row]]\ncode = \"p\"\n",
		"roles.toml": "table = \"roles\"\nkey = \"code\"\non_remove = \"delete\"\n[refs]\nperm_id = \"perms.code\"\nparent_id = \"roles.code\"\n" + roles,
	}
}

func TestSelfReferenceToSkippedRow(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, selfSchema)
		// a is skipped for a missing permission, so b can't point at it either.
		p := f.agree(selfSeed("[[row]]\ncode = \"a\"\nperm_id = \"missing\"\n[[row]]\ncode = \"b\"\nparent_id = \"a\"\n"))
		expect(t, p, "insert perms/p; skip roles/a.perm_id; skip roles/b.parent_id")
	})
}

func TestSelfReferenceToDroppedRow(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, selfSchema)
		f.agree(selfSeed("[[row]]\ncode = \"a\"\n[[row]]\ncode = \"b\"\n"))
		// a now points at b, and b is dropped: b stays until nothing points at it.
		p := f.agree(selfSeed("[[row]]\ncode = \"a\"\nparent_id = \"b\"\n"))
		expect(t, p, "update roles/a.parent_id; skip roles/b")
		// Once a stops pointing at it, it goes.
		expect(t, f.agree(selfSeed("[[row]]\ncode = \"a\"\n")), "update roles/a.parent_id; delete roles/b")
	})
}

func TestReferenceByNonKeyColumn(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, `
CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, slug TEXT UNIQUE);
CREATE TABLE users (id INTEGER PRIMARY KEY, login TEXT NOT NULL UNIQUE, role_id INTEGER REFERENCES roles (id));`)
		seed := func(slug, userRole string) map[string]string {
			return map[string]string{
				"roles.toml": "table = \"roles\"\nkey = \"code\"\n[columns]\nslug = \"seed\"\n[[row]]\ncode = \"a\"\nslug = \"" + slug + "\"\n",
				"users.toml": "table = \"users\"\nkey = \"login\"\n[refs]\nrole_id = \"roles.slug\"\n[[row]]\nlogin = \"x\"\nrole_id = \"" + userRole + "\"\n",
			}
		}
		// A reference to a slug inserted in the same apply lands at once.
		expect(t, f.agree(seed("old", "old")), "insert roles/a; insert users/x")
		// The slug changes and the user's seed still names the old one: the
		// user's row keeps pointing at the same role, which now answers to the
		// new slug, and nothing breaks.
		p := f.agree(seed("new", "old"))
		expect(t, p, "update roles/a.slug; skip users/x.role_id")
		if got := f.one("SELECT r.slug FROM users u JOIN roles r ON r.id = u.role_id"); got != "new" {
			t.Fatal(got)
		}
		expect(t, f.agree(seed("new", "new")), "")
	})
}

func TestRepointAndDropWithForeignKeys(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, `
CREATE TABLE perms (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, name TEXT UNIQUE);
CREATE TABLE grants (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, perm_id INTEGER NOT NULL REFERENCES perms (id));`)
		seed := func(perms, grant string) map[string]string {
			return map[string]string{
				"perms.toml":  "table = \"perms\"\nkey = \"code\"\non_remove = \"delete\"\n" + perms,
				"grants.toml": "table = \"grants\"\nkey = \"code\"\n[refs]\nperm_id = \"perms.code\"\n[[row]]\ncode = \"g\"\nperm_id = \"" + grant + "\"\n",
			}
		}
		f.agree(seed("[[row]]\ncode = \"a\"\nname = \"A\"\n[[row]]\ncode = \"b\"\nname = \"B\"\n", "b"))
		// Repoint g to a and drop b in one version.
		expect(t, f.agree(seed("[[row]]\ncode = \"a\"\nname = \"A\"\n", "a")), "delete perms/b; update grants/g.perm_id")
		// Drop a and add c in one version, with g moving from a to c: c goes in,
		// g moves, then a goes.
		expect(t, f.agree(seed("[[row]]\ncode = \"c\"\nname = \"C\"\n", "c")), "insert perms/c; delete perms/a; update grants/g.perm_id")
		// A dropped row nothing points at goes first, so a new row can reuse
		// its unique name.
		expect(t, f.agree(seed("[[row]]\ncode = \"c\"\nname = \"C\"\n[[row]]\ncode = \"x\"\nname = \"X\"\n", "c")), "insert perms/x")
		expect(t, f.agree(seed("[[row]]\ncode = \"c\"\nname = \"C\"\n[[row]]\ncode = \"y\"\nname = \"X\"\n", "c")), "insert perms/y; delete perms/x")
	})
}

func TestDropRowStillListedByChild(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, linkSchema)
		links := "[[row]]\nrole_id = \"r\"\nperm_id = \"write\"\n"
		seed := func(perms string) map[string]string {
			files := linkSeed("", perms, links)
			files["roles.toml"] = "table = \"roles\"\nkey = \"code\"\n[[row]]\ncode = \"r\"\n"
			return files
		}
		f.agree(seed("[[row]]\ncode = \"read\"\n[[row]]\ncode = \"write\"\n"))
		// The seed drops the permission but still links it: it stays, skipped.
		p := f.agree(seed("[[row]]\ncode = \"read\"\n"))
		expect(t, p, "skip perms/write")
		if got := f.one("SELECT count(*) FROM perms"); got != "2" {
			t.Fatal(got)
		}
	})
}

func TestDroppedRowWhoseReferenceIsNoLongerNamed(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, linkSchema)
		f.agree(linkSeed("[[row]]\ncode = \"b\"\n[[row]]\ncode = \"a\"\nparent_id = \"b\"\n", "", ""))
		// The next seed keeps only b and no longer names parent_id at all.
		files := linkSeed("", "", "")
		files["roles.toml"] = "table = \"roles\"\nkey = \"code\"\non_remove = \"delete\"\n[[row]]\ncode = \"b\"\n"
		// b's ledger entry no longer lists parent_id, so it's recorded again.
		expect(t, f.agree(files), "delete roles/a; record roles/b")
	})
}

func TestParentRenameKeepsChildEdits(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, `
CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE);
CREATE TABLE links (role_id INTEGER NOT NULL, perm TEXT NOT NULL, note TEXT, PRIMARY KEY (role_id, perm));`)
		seed := func(roles, role, note string) map[string]string {
			return map[string]string{
				"roles.toml": "table = \"roles\"\nkey = \"code\"\n" + roles,
				"links.toml": "table = \"links\"\nkey = [\"role_id\", \"perm\"]\n[refs]\nrole_id = \"roles.code\"\n[[row]]\nrole_id = \"" + role + "\"\nperm = \"p\"\nnote = \"" + note + "\"\n",
			}
		}
		f.agree(seed("[[row]]\ncode = \"old\"\n", "old", "seed note"))
		mustExec(t, f.db, "UPDATE links SET note = 'local note'")
		p := f.agree(seed("[[row]]\ncode = \"new\"\n_renamed_from = \"old\"\n", "new", "seed note"))
		expect(t, p, "rename roles/new; keep-edit links/new, p.note")
		if got := f.one("SELECT note FROM links"); got != "local note" {
			t.Fatal(got)
		}
		// And the next seed change to the note is a real conflict, with the
		// ledger's history intact.
		set := setFrom(t, seed("[[row]]\ncode = \"new\"\n", "new", "newer seed note"))
		if p, err := bs.Apply(f.ctx, f.db, set, nil); !errors.Is(err, bs.ErrBlocked) {
			t.Fatalf("%v %s", err, actions(p))
		}
	})
}

func TestChainedRenameIsRefused(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, rolesSchema)
		f.agree(rolesSeed("[[row]]\ncode = \"a\"\n[[row]]\ncode = \"b\"\n", ""))
		set := setFrom(t, rolesSeed("[[row]]\ncode = \"b\"\n_renamed_from = \"a\"\n[[row]]\ncode = \"c\"\n_renamed_from = \"b\"\n", ""))
		if _, err := bs.Preview(f.ctx, f.db, set, nil); err == nil || !strings.Contains(err.Error(), "rename one step per version") {
			t.Fatalf("%v", err)
		}
	})
}

func TestLedgerOnlyWritesAreShown(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, rolesSchema)
		f.agree(rolesSeed("[[row]]\ncode = \"a\"\n[[row]]\ncode = \"b\"\n", ""))
		mustExec(t, f.db, "DELETE FROM roles WHERE code = 'b'")
		// b is gone from the table and now from the seed: the only write is the
		// ledger forgetting it, and the plan says so.
		seed := rolesSeed("[[row]]\ncode = \"a\"\n", "")
		p := f.preview(seed)
		if !p.Pending() {
			t.Fatal("not pending")
		}
		expect(t, p, "record roles/b")
		f.agree(seed)
	})
}

func TestRetireColumnMustAllowNull(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, `CREATE TABLE r (code TEXT PRIMARY KEY, retired_at TEXT NOT NULL)`)
		set := setFrom(t, map[string]string{"r.toml": "table = \"r\"\nkey = \"code\"\non_remove = { retire = \"retired_at\" }\n[[row]]\ncode = \"a\"\n"})
		if _, err := bs.Preview(f.ctx, f.db, set, nil); err == nil || !strings.Contains(err.Error(), "retire column") {
			t.Fatal(err)
		}
	})
}

func TestNullKeysAreLeftAlone(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, `CREATE TABLE t (code TEXT, name TEXT)`)
		mustExec(t, f.db, "INSERT INTO t VALUES (NULL, 'x'), (NULL, 'y')")
		expect(t, f.agree(map[string]string{"t.toml": "table = \"t\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\n"}), "insert t/a")
		if got := f.one("SELECT count(*) FROM t"); got != "3" {
			t.Fatal(got)
		}
	})
}

// Cases found by the random histories with references (refs_promise_test.go).

func TestKeptRowKeepsItsParent(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, selfSchema)
		f.agree(selfSeed("[[row]]\ncode = \"b\"\n[[row]]\ncode = \"a\"\nparent_id = \"b\"\n[[row]]\ncode = \"c\"\nparent_id = \"a\"\n"))
		// c still points at a, so a stays; a points at b, so b stays too.
		p := f.agree(selfSeed("[[row]]\ncode = \"c\"\nparent_id = \"a\"\n"))
		expect(t, p, "skip roles/a; skip roles/b")
	})
}

func TestDropParentAndChildTogether(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, selfSchema)
		f.agree(selfSeed("[[row]]\ncode = \"a\"\n[[row]]\ncode = \"z\"\nparent_id = \"a\"\n[[row]]\ncode = \"k\"\n"))
		p := f.agree(selfSeed("[[row]]\ncode = \"k\"\n"))
		expect(t, p, "delete roles/a; delete roles/z")
	})
}

func TestDropRowsThatPointAtEachOther(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, selfSchema)
		f.agree(selfSeed("[[row]]\ncode = \"a\"\nparent_id = \"z\"\n[[row]]\ncode = \"z\"\nparent_id = \"a\"\n[[row]]\ncode = \"k\"\n[[row]]\ncode = \"s\"\nparent_id = \"s\"\n"))
		p := f.agree(selfSeed("[[row]]\ncode = \"k\"\n"))
		expect(t, p, "delete roles/a; delete roles/s; delete roles/z")
	})
}

func TestLateDeleteWaitsForItsParent(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, selfSchema)
		f.agree(selfSeed("[[row]]\ncode = \"y\"\n[[row]]\ncode = \"x\"\nparent_id = \"y\"\n[[row]]\ncode = \"c\"\nparent_id = \"x\"\n"))
		// c lets go of x in this apply, so x is deleted at the end; y, which x
		// points at, has to wait for it.
		p := f.agree(selfSeed("[[row]]\ncode = \"c\"\n"))
		expect(t, p, "update roles/c.parent_id; delete roles/x; delete roles/y")
	})
}

func TestSkippedSelfReferenceLeavesLedgerAlone(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, selfSchema)
		f.agree(selfSeed("[[row]]\ncode = \"b\"\n[[row]]\ncode = \"a\"\nparent_id = \"b\"\n"))
		// The new parent x is skipped for a missing permission, so a's update
		// is skipped too and the ledger keeps what it had; a second apply has
		// nothing to do.
		p := f.agree(selfSeed("[[row]]\ncode = \"b\"\n[[row]]\ncode = \"x\"\nperm_id = \"missing\"\n[[row]]\ncode = \"a\"\nparent_id = \"x\"\n"))
		expect(t, p, "skip roles/x.perm_id; skip roles/a.parent_id")
	})
}
