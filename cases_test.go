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

// One case per rule that the story and the random histories don't pin down
// on their own.

type fixture struct {
	t   *testing.T
	db  *sql.DB
	ctx context.Context
}

func newFixture(t *testing.T, schema string) *fixture {
	t.Helper()
	db := openPath(t, filepath.Join(t.TempDir(), "f.db"))
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, schema)
	return &fixture{t: t, db: db, ctx: context.Background()}
}

func (f *fixture) apply(files map[string]string, take ...bs.Resolution) *bs.Plan {
	f.t.Helper()
	p, err := bs.Apply(f.ctx, f.db, setFrom(f.t, files), &bs.Options{Now: testClock, Take: take})
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	return p
}

func (f *fixture) preview(files map[string]string) *bs.Plan {
	f.t.Helper()
	p, err := bs.Preview(f.ctx, f.db, setFrom(f.t, files), &bs.Options{Now: testClock})
	if err != nil {
		f.t.Fatalf("preview: %v", err)
	}
	return p
}

func (f *fixture) one(q string, args ...any) string {
	f.t.Helper()
	return query1(f.t, f.db, q, args...)
}

func actions(p *bs.Plan) string {
	var parts []string
	for _, c := range p.Changes {
		s := string(c.Action) + " " + c.Table + "/" + c.KeyText()
		if c.Column != "" {
			s += "." + c.Column
		}
		if c.Resolved != "" {
			s += "=" + string(c.Resolved)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

func expect(t *testing.T, p *bs.Plan, want string) {
	t.Helper()
	if got := actions(p); got != want {
		t.Fatalf("plan:\n got %s\nwant %s", got, want)
	}
}

const rolesSchema = `CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, name TEXT, sys INTEGER, note TEXT, retired_at TEXT)`

func rolesSeed(rows string, extra string) map[string]string {
	return map[string]string{"roles.toml": "table = \"roles\"\nkey = \"code\"\n" + extra + "\n[columns]\nsys = \"seed\"\nnote = \"local\"\n" + rows}
}

func TestAdoptExistingRows(t *testing.T) {
	f := newFixture(t, rolesSchema)
	mustExec(t, f.db, "INSERT INTO roles (code, name, sys, note) VALUES ('a', 'A', 1, 'n'), ('b', 'Bee', 0, 'mine'), ('x', 'X', 0, NULL)")
	seed := rolesSeed(`
[[row]]
code = "a"
name = "A"
sys = 1
note = "n"
[[row]]
code = "b"
name = "B"
sys = 1
note = "theirs"
`, "")
	p := f.preview(seed)
	// a matches and is recorded; b's name differs (a conflict, since nothing
	// says which is right), its seed-owned sys is corrected, and its local
	// note stays. x isn't in the seed and isn't touched.
	expect(t, p, "conflict roles/b.name; update roles/b.sys; record roles/a")
	if !p.Blocked() {
		t.Fatal("not blocked")
	}
	f.apply(seed, bs.Resolution{Take: bs.TakeLocal})
	if got := f.one("SELECT name || sys || note FROM roles WHERE code = 'b'"); got != "Bee1mine" {
		t.Fatalf("b: %s", got)
	}
	expect(t, f.preview(seed), "keep-edit roles/b.name")
}

func TestNewlyManagedColumn(t *testing.T) {
	f := newFixture(t, rolesSchema)
	f.apply(rolesSeed("[[row]]\ncode = \"a\"\n", ""))
	// The seed starts setting name: the ledger never knew it, so the old
	// value counts as NULL and the table, still NULL, takes the seed's.
	seed := rolesSeed("[[row]]\ncode = \"a\"\nname = \"A\"\n", "")
	expect(t, f.preview(seed), "update roles/a.name")
	f.apply(seed)
	if got := f.one("SELECT name FROM roles WHERE code = 'a'"); got != "A" {
		t.Fatal(got)
	}
}

func TestRestoreAndUnretire(t *testing.T) {
	f := newFixture(t, rolesSchema)
	seed := rolesSeed("[[row]]\ncode = \"a\"\n[[row]]\ncode = \"b\"\n", "on_local_delete = \"restore\"\non_remove = { retire = \"retired_at\" }")
	f.apply(seed)
	mustExec(t, f.db, "DELETE FROM roles WHERE code = 'a'")
	expect(t, f.preview(seed), "insert roles/a")
	f.apply(seed)

	dropped := rolesSeed("[[row]]\ncode = \"a\"\n", "on_local_delete = \"restore\"\non_remove = { retire = \"retired_at\" }")
	expect(t, f.apply(dropped), "retire roles/b")
	if got := f.one("SELECT retired_at FROM roles WHERE code = 'b'"); got != "2026-11-02T08:00:00Z" {
		t.Fatal(got)
	}
	expect(t, f.preview(dropped), "")
	expect(t, f.apply(seed), "unretire roles/b")
	if got := f.one("SELECT retired_at FROM roles WHERE code = 'b'"); got != "NULL" {
		t.Fatal(got)
	}
}

func TestRemovedButEdited(t *testing.T) {
	for _, take := range []bs.Take{bs.TakeSeed, bs.TakeLocal} {
		f := newFixture(t, rolesSchema)
		f.apply(rolesSeed("[[row]]\ncode = \"a\"\nname = \"A\"\n[[row]]\ncode = \"b\"\nname = \"B\"\n", "on_remove = \"delete\""))
		mustExec(t, f.db, "UPDATE roles SET name = 'Edited' WHERE code = 'b'")
		mustExec(t, f.db, "UPDATE roles SET note = 'only a local column' WHERE code = 'a'")
		seed := rolesSeed("", "on_remove = \"delete\"")
		p, err := bs.Apply(f.ctx, f.db, setFrom(t, seed), nil)
		if !errors.Is(err, bs.ErrBlocked) {
			t.Fatal(err)
		}
		// a was only edited in a local column, so it goes; b's name was edited.
		expect(t, p, "delete roles/a; conflict roles/b")
		f.apply(seed, bs.Resolution{Table: "roles", Key: "b", Take: take})
		want := map[bs.Take]string{bs.TakeSeed: "0", bs.TakeLocal: "1"}[take]
		if got := f.one("SELECT count(*) FROM roles"); got != want {
			t.Fatalf("take %s: %s rows left", take, got)
		}
		expect(t, f.preview(seed), "")
	}
}

func TestOnConflictInSeedFile(t *testing.T) {
	f := newFixture(t, rolesSchema)
	f.apply(rolesSeed("[[row]]\ncode = \"a\"\nname = \"A\"\n", ""))
	mustExec(t, f.db, "UPDATE roles SET name = 'Local' WHERE code = 'a'")
	seed := map[string]string{"roles.toml": "table = \"roles\"\nkey = \"code\"\n[columns]\nname = { own = \"merge\", on_conflict = \"seed\" }\n[[row]]\ncode = \"a\"\nname = \"Seed\"\n"}
	expect(t, f.apply(seed), "conflict roles/a.name=seed")
	if got := f.one("SELECT name FROM roles"); got != "Seed" {
		t.Fatal(got)
	}
	// A Take given at apply time beats the seed file.
	mustExec(t, f.db, "UPDATE roles SET name = 'Local again'")
	seed["roles.toml"] = strings.Replace(seed["roles.toml"], `"Seed"`, `"Seed 2"`, 1)
	expect(t, f.apply(seed, bs.Resolution{Column: "name", Take: bs.TakeLocal}), "conflict roles/a.name=local")
	if got := f.one("SELECT name FROM roles"); got != "Local again" {
		t.Fatal(got)
	}
}

const linkSchema = `
CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, parent_id INTEGER REFERENCES roles (id));
CREATE TABLE perms (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE);
CREATE TABLE links (role_id INTEGER NOT NULL, perm_id INTEGER NOT NULL, PRIMARY KEY (role_id, perm_id)) WITHOUT ROWID;`

func linkSeed(roles, perms, links string) map[string]string {
	return map[string]string{
		"roles.toml": "table = \"roles\"\nkey = \"code\"\n[refs]\nparent_id = \"roles.code\"\n" + roles,
		"perms.toml": "table = \"perms\"\nkey = \"code\"\non_remove = \"delete\"\n" + perms,
		"links.toml": "table = \"links\"\nkey = [\"role_id\", \"perm_id\"]\non_remove = \"delete\"\n[refs]\nrole_id = \"roles.code\"\nperm_id = \"perms.code\"\n" + links,
	}
}

func TestReferences(t *testing.T) {
	f := newFixture(t, linkSchema)
	// A child before its parent in the file, pointing at its own table.
	roles := "[[row]]\ncode = \"junior\"\nparent_id = \"senior\"\n[[row]]\ncode = \"senior\"\n"
	perms := "[[row]]\ncode = \"read\"\n[[row]]\ncode = \"write\"\n"
	links := "[[row]]\nrole_id = \"junior\"\nperm_id = \"read\"\n[[row]]\nrole_id = \"senior\"\nperm_id = \"write\"\n[[row]]\nrole_id = \"ghost\"\nperm_id = \"read\"\n"
	p := f.apply(linkSeed(roles, perms, links))
	// The link to a role nobody has is skipped, not guessed at.
	expect(t, p, "insert roles/junior; insert roles/senior; insert perms/read; insert perms/write; insert links/junior, read; insert links/senior, write; skip links/ghost, read.role_id")
	if got := f.one("SELECT p.code FROM roles r JOIN roles p ON p.id = r.parent_id WHERE r.code = 'junior'"); got != "senior" {
		t.Fatalf("parent: %s", got)
	}
	expect(t, f.preview(linkSeed(roles, perms, links)), "skip links/ghost, read.role_id")
	// Once the role exists, the link goes in.
	mustExec(t, f.db, "INSERT INTO roles (code) VALUES ('ghost')")
	expect(t, f.apply(linkSeed(roles, perms, links)), "insert links/ghost, read")

	// Renaming a role keeps its id, so its links follow it; the seed's links
	// under the new name are recognised as the same rows.
	id := f.one("SELECT id FROM roles WHERE code = 'senior'")
	roles2 := "[[row]]\ncode = \"junior\"\nparent_id = \"lead\"\n[[row]]\ncode = \"lead\"\n_renamed_from = \"senior\"\n"
	links2 := strings.ReplaceAll(links, `"senior"`, `"lead"`)
	expect(t, f.apply(linkSeed(roles2, perms, links2)), "rename roles/lead; record roles/junior; record links/lead, write")
	if got := f.one("SELECT id FROM roles WHERE code = 'lead'"); got != id {
		t.Fatalf("the renamed row's id changed: %s, was %s", got, id)
	}
	if got := f.one("SELECT count(*) FROM links"); got != "3" {
		t.Fatalf("links: %s", got)
	}
	expect(t, f.preview(linkSeed(roles2, perms, links2)), "")

	// Dropping a permission deletes its links first, then the permission.
	links3 := "[[row]]\nrole_id = \"junior\"\nperm_id = \"read\"\n[[row]]\nrole_id = \"ghost\"\nperm_id = \"read\"\n"
	expect(t, f.apply(linkSeed(roles2, "[[row]]\ncode = \"read\"\n", links3)), "delete perms/write; delete links/lead, write")
	if got := f.one("SELECT count(*) FROM links") + f.one("SELECT count(*) FROM perms"); got != "21" {
		t.Fatalf("after the drop: %s", got)
	}
}

func TestRenameOfDeletedRow(t *testing.T) {
	f := newFixture(t, rolesSchema)
	f.apply(rolesSeed("[[row]]\ncode = \"old\"\n", ""))
	mustExec(t, f.db, "DELETE FROM roles")
	seed := rolesSeed("[[row]]\ncode = \"new\"\n_renamed_from = \"old\"\n", "")
	// The admin deleted it under its old name; it stays deleted under the new one.
	expect(t, f.apply(seed), "keep-delete roles/new")
	expect(t, f.preview(seed), "keep-delete roles/new")
	if got := f.one("SELECT count(*) FROM roles"); got != "0" {
		t.Fatal(got)
	}
}

func TestTypedColumnsStayInSync(t *testing.T) {
	f := newFixture(t, `CREATE TABLE plans (code TEXT PRIMARY KEY, active BOOLEAN, starts DATE, at DATETIME,
		price REAL, ratio NUMERIC, blob BLOB, label TEXT, n INTEGER)`)
	seed := map[string]string{"plans.toml": `
table = "plans"
key = "code"
[[row]]
code = "pro"
active = true
starts = 2026-10-18
at = 2026-10-18T09:30:00Z
price = 12
ratio = "0.25"
label = 1.5
n = "42"
[[row]]
code = "free"
active = false
starts = "2026-01-01"
price = 0.1
label = 0.1
`}
	f.apply(seed)
	// BOOLEAN, DATE and DATETIME columns are read back as stored, so a second
	// look finds nothing to do.
	expect(t, f.preview(seed), "")
	if got := f.one("SELECT typeof(n) || typeof(ratio) || typeof(price) || typeof(label) FROM plans WHERE code = 'pro'"); got != "integerrealrealtext" {
		t.Fatal(got)
	}
}

func TestSchemaErrors(t *testing.T) {
	cases := []struct{ schema, seed, want string }{
		{rolesSchema, "table = \"nope\"\nkey = \"code\"\n", `table "nope" doesn't exist`},
		{rolesSchema, "table = \"roles\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\ncolour = 1\n", `no column "colour"`},
		{rolesSchema, "table = \"roles\"\nkey = \"code\"\non_remove = { retire = \"gone\" }\n", `no column "gone"`},
		{`CREATE TABLE t (code TEXT, must TEXT NOT NULL)`, "table = \"t\"\nkey = \"code\"\n", `NOT NULL with no default`},
		{`CREATE TABLE t (code INTEGER)`, "table = \"t\"\nkey = \"code\"\n[[row]]\ncode = 1\n[[row]]\ncode = \"1\"\n", `repeats the key 1`},
		{`CREATE TABLE t (code TEXT, x TEXT)`, "table = \"t\"\nkey = [\"code\", \"x\"]\n[[row]]\ncode = \"a\"\nx = \"\"\n[[row]]\ncode = \"b\"\n", `no value for key column "x"`},
		{`CREATE TABLE t (code TEXT, r INTEGER); CREATE TABLE o (a INTEGER, b INTEGER, c TEXT, PRIMARY KEY (a, b)) WITHOUT ROWID`,
			"table = \"t\"\nkey = \"code\"\n[refs]\nr = \"o.c\"\n[[row]]\ncode = \"a\"\nr = \"x\"\n", `no single primary key`},
	}
	for _, c := range cases {
		f := newFixture(t, c.schema)
		set, err := func() (*bs.Set, error) {
			return bs.LoadFS(mapFS(map[string]string{"s.toml": c.seed}), ".")
		}()
		if err == nil {
			_, err = bs.Preview(f.ctx, f.db, set, nil)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.seed, err, c.want)
		}
	}
	// Two live rows with one key.
	f := newFixture(t, `CREATE TABLE t (code TEXT)`)
	mustExec(t, f.db, "INSERT INTO t VALUES ('a'), ('a')")
	if _, err := bs.Preview(f.ctx, f.db, setFrom(t, map[string]string{"t.toml": "table = \"t\"\nkey = \"code\"\n"}), nil); err == nil || !strings.Contains(err.Error(), "more than one row") {
		t.Errorf("duplicate live keys: %v", err)
	}
}

func TestLedgerAndExplain(t *testing.T) {
	f := newFixture(t, rolesSchema)
	seed := rolesSeed("[[row]]\ncode = \"a\"\nname = \"A\"\nsys = 1\n", "")
	// Preview never creates the ledger.
	f.preview(seed)
	if got := f.one("SELECT count(*) FROM sqlite_master WHERE name LIKE '_bootstrapping%'"); got != "0" {
		t.Fatalf("preview created %s ledger tables", got)
	}
	if runs, err := bs.History(f.ctx, f.db, 0); err != nil || len(runs) != 0 {
		t.Fatal(runs, err)
	}
	if err := bs.Init(f.ctx, f.db); err != nil {
		t.Fatal(err)
	}
	if err := bs.Init(f.ctx, f.db); err != nil {
		t.Fatal("Init twice:", err)
	}
	f.apply(seed)
	mustExec(t, f.db, "UPDATE roles SET name = 'Mine'")
	seed2 := rolesSeed("[[row]]\ncode = \"a\"\nname = \"Theirs\"\nsys = 0\n", "")
	f.apply(seed2, bs.Resolution{Take: bs.TakeLocal})
	rep, err := bs.Explain(f.ctx, f.db, setFrom(t, seed2), "roles", " a ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.InSeed || !rep.InTable || !rep.Managed || len(rep.Fields) != 3 || len(rep.History) != 3 {
		t.Fatalf("%+v", rep)
	}
	verdicts := []string{}
	for _, fs := range rep.Fields {
		verdicts = append(verdicts, fs.Column+": "+fs.Verdict)
	}
	if got := strings.Join(verdicts, "; "); got != "name: kept local edit; note: in sync; sys: in sync" {
		t.Fatal(got)
	}
	if h := rep.History; h[0].Action != "insert" || h[1].Action != "update" || h[1].Column != "sys" || h[2].Action != "conflict" {
		t.Fatalf("%+v", h)
	}
	if rep, err := bs.Explain(f.ctx, f.db, setFrom(t, seed2), "roles", "zzz", nil); err != nil || rep.InSeed || rep.Managed {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := bs.Explain(f.ctx, f.db, setFrom(t, seed2), "nope", "a", nil); err == nil {
		t.Fatal("explained a table no seed covers")
	}
	runs, _ := bs.History(f.ctx, f.db, 1)
	if len(runs) != 1 || runs[0].ID != 2 || runs[0].Updates != 1 || runs[0].Resolved != 1 || runs[0].Version != bs.Version {
		t.Fatalf("%+v", runs)
	}
}

func TestProgressAndRollback(t *testing.T) {
	f := newFixture(t, rolesSchema+"; CREATE TRIGGER no_z BEFORE INSERT ON roles WHEN NEW.code = 'z' BEGIN SELECT RAISE(ABORT, 'no z'); END")
	seed := rolesSeed("[[row]]\ncode = \"a\"\n[[row]]\ncode = \"z\"\n", "")
	n := 0
	_, err := bs.Apply(f.ctx, f.db, setFrom(t, seed), &bs.Options{Progress: func(bs.Change) { n++ }})
	if err == nil || !strings.Contains(err.Error(), "no z") {
		t.Fatal(err)
	}
	// The first insert happened inside the transaction, and was rolled back.
	if n != 1 || f.one("SELECT count(*) FROM roles") != "0" || f.one("SELECT count(*) FROM sqlite_master WHERE name LIKE '_bootstrapping%'") != "0" {
		t.Fatalf("progress %d, rows %s", n, f.one("SELECT count(*) FROM roles"))
	}
}
