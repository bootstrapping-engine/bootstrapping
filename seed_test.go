// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"strings"
	"testing"
	"testing/fstest"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

func TestParseTable(t *testing.T) {
	tb, err := bs.ParseTable("roles.toml", []byte(`
table = "roles"
key = "code"
on_remove = { retire = "retired_at" }
on_conflict = "local"
on_local_delete = "restore"

[columns]
system = "seed"
name = { own = "merge", on_conflict = "seed" }
notes = "local"

[[row]]
code = "admin"
name = "Administrator"
system = true

[[row]]
code = "editor"
_renamed_from = "author"
description = "Writes"
`))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Name != "roles" || len(tb.Key) != 1 || tb.OnRemove != bs.RemoveRetire || tb.RetireColumn != "retired_at" ||
		tb.OnConflict != bs.TakeLocal || tb.OnLocalDelete != "restore" || len(tb.Rows) != 2 {
		t.Fatalf("%+v", tb)
	}
	var names []string
	for _, c := range tb.Columns {
		names = append(names, c.Name+":"+string(c.Own))
	}
	if got := strings.Join(names, " "); got != "code:seed description:merge name:merge notes:local system:seed" {
		t.Fatalf("columns: %s", got)
	}
	if c := tb.Column("name"); c.OnConflict != bs.TakeSeed {
		t.Fatalf("name's on_conflict: %q", c.OnConflict)
	}
	if r := tb.Rows[1]; len(r.RenamedFrom) != 1 || r.RenamedFrom[0] != "author" {
		t.Fatalf("renamed_from: %v", r.RenamedFrom)
	}
}

func TestParseTableErrors(t *testing.T) {
	cases := []struct{ body, want string }{
		{`key = "a"`, `missing table`},
		{`table = "t"`, `missing key`},
		{`table = "t"` + "\nkey = []", `key is empty`},
		{`table = "t"` + "\nkey = [\"a\", \"a\"]", `key names column "a" twice`},
		{`table = "t"` + "\nkey = 3", `key must be`},
		{`table = "t"` + "\nkey = \"a\"\ncolour = 1", `unknown setting "colour"`},
		{`table = "t"` + "\nkey = \"a\"\non_remove = \"retire\"", `needs the column to set`},
		{`table = "t"` + "\nkey = \"a\"\non_remove = \"drop\"", `on_remove must be`},
		{`table = "t"` + "\nkey = \"a\"\non_remove = { retire = \"x\", also = 1 }", `on_remove must be`},
		{`table = "t"` + "\nkey = \"a\"\non_conflict = \"maybe\"", `on_conflict must be`},
		{`table = "t"` + "\nkey = \"a\"\non_local_delete = \"forget\"", `on_local_delete must be`},
		{`table = "t"` + "\nkey = \"a\"\n[columns]\nb = \"mine\"", `ownership must be`},
		{`table = "t"` + "\nkey = \"a\"\n[columns]\na = \"seed\"", `part of the key`},
		{`table = "t"` + "\nkey = \"a\"\n[columns]\nb = { owner = \"seed\" }", `unknown setting "owner"`},
		{`table = "t"` + "\nkey = \"a\"\n[columns]\nb = { on_conflict = \"x\" }", `on_conflict must be`},
		{`table = "t"` + "\nkey = \"a\"\n[refs]\nb = \"roles\"", `must be "table.column"`},
		{`table = "t"` + "\nkey = \"a\"\n[refs]\nb = \"roles.code\"", `names a column no row sets`},
		{`table = "t"` + "\nkey = \"a\"\n[[row]]\nb = 1", `no value for key column "a"`},
		{`table = "t"` + "\nkey = \"a\"\n[[row]]\na = 1\n_why = 2", `reserved`},
		{`table = "t"` + "\nkey = \"a\"\n[[row]]\na = 1\nb = [1, 2]", `lists and tables`},
		{`table = "t"` + "\nkey = \"a\"\n[[row]]\na = 1\n_renamed_from = [1, 2]", `needs 1 value`},
		{`table = "t"` + "\nkey = \"a\"\non_remove = { retire = \"r\" }\n[[row]]\na = 1\nr = 2", `retire column`},
		{`table = "t"` + "\nkey = \"a\"\non_remove = { retire = \"r\" }\n[columns]\nr = \"seed\"", `can't also be a managed column`},
		{`table = "t"` + "\nkey = \"a\"\nrow = 3", `[[row]] tables`},
		{`table = `, `roles.toml:`},
	}
	for _, c := range cases {
		_, err := bs.ParseTable("roles.toml", []byte(c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "roles.toml:") {
			t.Errorf("%q: got %v, want an error with %q", c.body, err, c.want)
		}
	}
}

func TestSetOrder(t *testing.T) {
	m := fstest.MapFS{
		"a_links.toml": {Data: []byte("table = \"links\"\nkey = [\"r\", \"p\"]\n[refs]\nr = \"roles.code\"\np = \"perms.code\"\n[[row]]\nr = \"x\"\np = \"y\"\n")},
		"b_roles.toml": {Data: []byte("table = \"roles\"\nkey = \"code\"\n[refs]\nparent = \"roles.code\"\n[[row]]\ncode = \"x\"\nparent = \"x\"\n")},
		"c_perms.toml": {Data: []byte("table = \"perms\"\nkey = \"code\"\n")},
		"notes.txt":    {Data: []byte("ignored")},
	}
	s, err := bs.LoadFS(m, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tb := range s.Tables {
		names = append(names, tb.Name)
	}
	if got := strings.Join(names, " "); got != "roles perms links" {
		t.Fatalf("order: %s", got)
	}
	if len(s.Hash) != 64 {
		t.Fatalf("hash: %q", s.Hash)
	}
	// A loop between tables is an error; so is a table seeded twice; so is an
	// empty directory.
	m["d_perms.toml"] = &fstest.MapFile{Data: []byte("table = \"perms\"\nkey = \"code\"\n[refs]\nr = \"links.r\"\n[[row]]\ncode = 1\nr = 2\n")}
	if _, err := bs.LoadFS(m, "."); err == nil || !strings.Contains(err.Error(), "already seeded by c_perms.toml") {
		t.Fatalf("seeded twice: %v", err)
	}
	delete(m, "c_perms.toml")
	if _, err := bs.LoadFS(m, "."); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("loop: %v", err)
	}
	if _, err := bs.LoadFS(fstest.MapFS{"x.txt": {}}, "."); err == nil || !strings.Contains(err.Error(), "no .toml files") {
		t.Fatalf("empty: %v", err)
	}
	if _, err := bs.LoadDir("does/not/exist"); err == nil {
		t.Fatal("a missing directory loaded")
	}
}
