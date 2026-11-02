// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

func loadSeed(files map[string]string) (*bs.Set, error) {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return bs.LoadFS(m, ".")
}

func (f *fixture) previewErr(files map[string]string) (*bs.Plan, error) {
	return bs.Preview(context.Background(), f.db, setFrom(f.t, files), nil)
}

// References that name their row by several columns: roles are keyed by
// region and code, and a grant names its role by both.

const compositeSchema = `
CREATE TABLE roles (id INTEGER PRIMARY KEY, region TEXT NOT NULL, code TEXT NOT NULL, name TEXT, UNIQUE (region, code));
CREATE TABLE grants (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, role_id INTEGER REFERENCES roles (id), note TEXT);`

func compositeSeed(roles, grants string) map[string]string {
	return map[string]string{
		"roles.toml":  "table = \"roles\"\nkey = [\"region\", \"code\"]\non_remove = \"delete\"\n" + roles,
		"grants.toml": "table = \"grants\"\nkey = \"code\"\non_remove = \"delete\"\n[refs]\nrole_id = \"roles.(region, code)\"\n" + grants,
	}
}

func TestCompositeReferences(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, compositeSchema)
		roles := "[[row]]\nregion = \"eu\"\ncode = \"editor\"\n[[row]]\nregion = \"us\"\ncode = \"editor\"\n"
		grants := "[[row]]\ncode = \"g1\"\nrole_id = [\"eu\", \"editor\"]\n[[row]]\ncode = \"g2\"\nrole_id = [\"us\", \"editor\"]\n" +
			"[[row]]\ncode = \"g3\"\nrole_id = [\"fr\", \"editor\"]\n"
		// The grant naming a role nobody has is skipped, as with one column.
		expect(t, f.agree(compositeSeed(roles, grants)), "insert roles/eu, editor; insert roles/us, editor; insert grants/g1; insert grants/g2; skip grants/g3.role_id")
		for g, region := range map[string]string{"g1": "eu", "g2": "us"} {
			if got := f.one("SELECT r.region FROM grants g JOIN roles r ON r.id = g.role_id WHERE g.code = ?", g); got != region {
				t.Fatalf("%s points at the %s role", g, got)
			}
		}
		// The ledger remembers the pair.
		if got := f.one(`SELECT vals FROM _bootstrapping_rows WHERE tbl = 'grants' AND vals LIKE '%k:%'`); !strings.Contains(got, `k:[`) {
			t.Fatalf("ledger: %s", got)
		}
		// An edit to the pair a grant names is seen and kept.
		mustExec(t, f.db, "UPDATE grants SET role_id = (SELECT id FROM roles WHERE region = 'us') WHERE code = 'g1'")
		p := f.preview(compositeSeed(roles, grants))
		if got := actions(p); !strings.Contains(got, "keep-edit grants/g1.role_id") {
			t.Fatalf("an edit wasn't seen: %s", got)
		}
		for _, c := range p.Changes {
			if c.Column == "role_id" && c.Live != nil && c.Live.String() != `("us", "editor")` {
				t.Fatalf("the live pair prints as %s", c.Live)
			}
		}
		mustExec(t, f.db, "UPDATE grants SET role_id = (SELECT id FROM roles WHERE region = 'eu') WHERE code = 'g1'")

		// Renaming a role keeps its id; the grant naming it by the new pair
		// is the same row as before.
		id := f.one("SELECT id FROM roles WHERE region = 'eu'")
		roles2 := "[[row]]\nregion = \"eu\"\ncode = \"writer\"\n_renamed_from = [\"eu\", \"editor\"]\n[[row]]\nregion = \"us\"\ncode = \"editor\"\n"
		grants2 := strings.Replace(grants, `["eu", "editor"]`, `["eu", "writer"]`, 1)
		expect(t, f.agree(compositeSeed(roles2, grants2)), "rename roles/eu, writer; skip grants/g3.role_id; record grants/g1")
		if got := f.one("SELECT id FROM roles WHERE code = 'writer'"); got != id {
			t.Fatalf("the renamed role's id changed: %s, was %s", got, id)
		}

		// A role a grant still names isn't deleted.
		p = f.preview(compositeSeed("[[row]]\nregion = \"us\"\ncode = \"editor\"\n", grants2))
		if got := actions(p); strings.Contains(got, "delete roles/eu, writer") {
			t.Fatalf("a role still named was deleted: %s", got)
		}
		// Dropped together, the grant goes first.
		grants3 := "[[row]]\ncode = \"g2\"\nrole_id = [\"us\", \"editor\"]\n"
		expect(t, f.agree(compositeSeed("[[row]]\nregion = \"us\"\ncode = \"editor\"\n", grants3)), "delete roles/eu, writer; delete grants/g1")
	})
}

func TestCompositeReferenceInCSV(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, compositeSchema)
		seed := compositeSeed("[[row]]\nregion = \"eu\"\ncode = \"editor\"\n", "")
		seed["grants.toml"] = "table = \"grants\"\nkey = \"code\"\nrows = \"grants.csv\"\n[refs]\nrole_id = \"roles.(region, code)\"\n"
		seed["grants.csv"] = "code,role_id,note\ng1,eu|editor,first\ng2,,none\n"
		expect(t, f.agree(seed), "insert roles/eu, editor; insert grants/g1; insert grants/g2")
		if got := f.one("SELECT count(*) FROM grants WHERE role_id IS NULL"); got != "1" {
			t.Fatalf("an empty cell is no role: %s", got)
		}
		// The same in TOML plans nothing.
		toml := compositeSeed("[[row]]\nregion = \"eu\"\ncode = \"editor\"\n",
			"[[row]]\ncode = \"g1\"\nrole_id = [\"eu\", \"editor\"]\nnote = \"first\"\n[[row]]\ncode = \"g2\"\nnote = \"none\"\n")
		expect(t, f.agree(toml), "")
	})
}

func TestCompositeReferenceMistakes(t *testing.T) {
	f := newFixture(t, compositeSchema)
	for _, c := range []struct{ grants, want string }{
		{"[[row]]\ncode = \"g\"\nrole_id = \"editor\"\n", "names its row by 2 columns; give a list of 2 values"},
		{"[[row]]\ncode = \"g\"\nrole_id = [\"eu\"]\n", "got 1 values"},
		{"[[row]]\ncode = \"g\"\nrole_id = [\"eu\", \"editor\", \"x\"]\n", "got 3 values"},
		{"[[row]]\ncode = \"g\"\nrole_id = [\"eu\", [\"x\"]]\n", "lists and tables can't be stored"},
	} {
		if _, err := loadSeed(compositeSeed("", c.grants)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.grants, err, c.want)
		}
	}
	for _, spec := range []string{"roles.(region)", "roles.(region, )", "roles.(region, region)", "roles.(a.b, c)", "roles.(region, code"} {
		seed := map[string]string{"grants.toml": "table = \"grants\"\nkey = \"code\"\n[refs]\nrole_id = \"" + spec + "\"\n"}
		if _, err := loadSeed(seed); err == nil || !strings.Contains(err.Error(), `"table.(a, b)"`) {
			t.Errorf("%s: got %v", spec, err)
		}
	}
	seed := compositeSeed("", "[[row]]\ncode = \"g\"\nrole_id = [\"eu\", \"editor\"]\n")
	seed["grants.toml"] = strings.Replace(seed["grants.toml"], "(region, code)", "(region, nope)", 1)
	if _, err := f.previewErr(seed); err == nil || !strings.Contains(err.Error(), `has no column "nope"`) {
		t.Fatalf("got %v", err)
	}
}

// A table that refers to itself by several columns: categories named by
// region and code, each with a parent in the same table.
func TestCompositeSelfReference(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, `CREATE TABLE cats (id INTEGER PRIMARY KEY, region TEXT NOT NULL, code TEXT NOT NULL, parent_id INTEGER REFERENCES cats (id), UNIQUE (region, code))`)
		seed := func(rows string) map[string]string {
			return map[string]string{"cats.toml": "table = \"cats\"\nkey = [\"region\", \"code\"]\non_remove = \"delete\"\n[refs]\nparent_id = \"cats.(region, code)\"\n" + rows}
		}
		// A child before its parent in the file.
		rows := "[[row]]\nregion = \"eu\"\ncode = \"shoes\"\nparent_id = [\"eu\", \"all\"]\n[[row]]\nregion = \"eu\"\ncode = \"all\"\n" +
			"[[row]]\nregion = \"us\"\ncode = \"shoes\"\nparent_id = [\"us\", \"none\"]\n"
		expect(t, f.agree(seed(rows)), "insert cats/eu, shoes; insert cats/eu, all; skip cats/us, shoes.parent_id")
		if got := f.one("SELECT p.code FROM cats c JOIN cats p ON p.id = c.parent_id WHERE c.region = 'eu' AND c.code = 'shoes'"); got != "all" {
			t.Fatalf("parent: %s", got)
		}
		// Renaming the parent: the child follows it.
		rows2 := "[[row]]\nregion = \"eu\"\ncode = \"shoes\"\nparent_id = [\"eu\", \"root\"]\n[[row]]\nregion = \"eu\"\ncode = \"root\"\n_renamed_from = [\"eu\", \"all\"]\n"
		expect(t, f.agree(seed(rows2)), "rename cats/eu, root; record cats/eu, shoes")
	})
}
