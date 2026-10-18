// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

const example = "../../examples/permissions/"

func setup(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := os.ReadFile(example + "schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main("bst", args, &out, &errb)
	return code, out.String(), errb.String()
}

func sqlExec(t *testing.T, path, q string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func TestCommandStory(t *testing.T) {
	db := setup(t)
	seeds := func(v string) string { return example + "seeds/" + v }

	code, out, _ := run(t, "status", "-db", db, "-seeds", seeds("v1"))
	if code != ExitPending || strings.TrimSpace(out) != "Pending: 18 inserts." {
		t.Fatalf("status before v1: %d %q", code, out)
	}
	if code, out, _ = run(t, "apply", "-db", db, "-seeds", seeds("v1")); code != ExitOK || !strings.Contains(out, "Applied as run 1: 18 inserts.") {
		t.Fatalf("apply v1: %d %s", code, out)
	}
	if code, out, _ = run(t, "plan", "-db", db, "-seeds", seeds("v1")); code != ExitOK || strings.TrimSpace(out) != "In step with the seed." {
		t.Fatalf("plan after v1: %d %q", code, out)
	}

	sqlExec(t, db, "UPDATE roles SET name = 'Content Editor' WHERE code = 'editor'")
	code, out, _ = run(t, "plan", "-db", db, "-seeds", seeds("v2"))
	if code != ExitPending {
		t.Fatalf("plan v2: %d", code)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		`keep edit editor name local "Content Editor"; the seed still says "Editor"`,
		`update viewer name "Viewer" -> "Reader"`,
		"role_permissions insert admin, billing.view",
		"Pending: 2 inserts, 2 updates, 1 local edit kept.",
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("plan v2 lacks %q:\n%s", want, out)
		}
	}
	run(t, "apply", "-db", db, "-seeds", seeds("v2"))

	// v3 conflicts: blocked, exit 3, nothing written, with a hint.
	code, out, _ = run(t, "apply", "-db", db, "-seeds", seeds("v3"))
	if code != ExitBlocked || !strings.Contains(out, "CONFLICT") || !strings.Contains(out, "-take roles/editor/name=local") {
		t.Fatalf("apply v3: %d\n%s", code, out)
	}
	// JSON plan.
	code, out, _ = run(t, "plan", "-json", "-db", db, "-seeds", seeds("v3"))
	var p struct {
		Blocked bool `json:"blocked"`
		Counts  struct {
			Conflicts int `json:"conflicts"`
		} `json:"counts"`
		Changes []struct {
			Action  string `json:"action"`
			KeyText string `json:"key_text"`
			Column  string `json:"column"`
			Seed    struct {
				Value any    `json:"value"`
				Type  string `json:"type"`
			} `json:"seed"`
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil || code != ExitBlocked || !p.Blocked || p.Counts.Conflicts != 1 {
		t.Fatalf("json plan: %d %v\n%s", code, err, out)
	}
	found := false
	for _, c := range p.Changes {
		if c.Action == "conflict" && c.KeyText == "editor" && c.Column == "name" && c.Seed.Value == "Editorial" && c.Seed.Type == "text" {
			found = true
		}
	}
	if !found {
		t.Fatalf("json plan lacks the conflict:\n%s", out)
	}
	if code, out, _ = run(t, "apply", "-db", db, "-seeds", seeds("v3"), "-take", "roles/editor/name=local"); code != ExitOK || !strings.Contains(out, "1 settled conflict") {
		t.Fatalf("apply v3 with -take: %d\n%s", code, out)
	}

	code, out, _ = run(t, "explain", "-db", db, "-seeds", seeds("v3"), "roles", "editor")
	for _, want := range []string{"roles editor", "in the seed, in the table, in the ledger", "kept local edit", "History:", "run 3", "kept \"Content Editor\" over the seed's \"Editorial\""} {
		if code != ExitOK || !strings.Contains(out, want) {
			t.Fatalf("explain lacks %q (%d):\n%s", want, code, out)
		}
	}
	code, out, _ = run(t, "explain", "-json", "-db", db, "-seeds", seeds("v3"), "role_permissions", "editor, posts.read")
	if code != ExitOK || !strings.Contains(out, `"managed": true`) {
		t.Fatalf("explain json: %d\n%s", code, out)
	}
	code, out, _ = run(t, "history", "-db", db, "-n", "2")
	if code != ExitOK || strings.Count(out, "\n") != 2 || !strings.HasPrefix(out, "run 3 ") {
		t.Fatalf("history: %d\n%s", code, out)
	}
	code, out, _ = run(t, "history", "-json", "-db", db)
	var runs []bs.Run
	if json.Unmarshal([]byte(out), &runs); code != ExitOK || len(runs) != 3 {
		t.Fatalf("history json: %d\n%s", code, out)
	}
}

func TestCommandEnvAndInit(t *testing.T) {
	db := setup(t)
	t.Setenv("BOOTSTRAPPING_DB", db)
	t.Setenv("BOOTSTRAPPING_SEEDS", example+"seeds/v1")
	if code, out, _ := run(t, "history"); code != ExitOK || !strings.Contains(out, "No applies") {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := run(t, "init"); code != ExitOK || !strings.Contains(out, "ready") {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := run(t, "history", "-json"); code != ExitOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, _ := run(t, "apply"); code != ExitOK {
		t.Fatal(code)
	}
}

func TestCommandErrors(t *testing.T) {
	db := setup(t)
	t.Setenv("BOOTSTRAPPING_DB", "")
	cases := []struct {
		args []string
		want string
	}{
		{nil, "Usage: bst"},
		{[]string{"frobnicate"}, `unknown command "frobnicate"`},
		{[]string{"plan"}, "no database"},
		{[]string{"plan", "-db", filepath.Join(t.TempDir(), "none.db")}, "no database at"},
		{[]string{"plan", "-db", db, "-seeds", "nowhere"}, "seed directory"},
		{[]string{"apply", "-db", db, "-seeds", example + "seeds/v1", "-take", "maybe"}, `-take "maybe"`},
		{[]string{"explain", "-db", db, "-seeds", example + "seeds/v1", "roles"}, "usage: bst explain TABLE KEY"},
		{[]string{"plan", "-nope"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		code, _, errOut := run(t, c.args...)
		if code != ExitError || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: %d %q, want %q", c.args, code, errOut, c.want)
		}
	}
	code, out, _ := run(t, "plan", "-json", "-db", db, "-seeds", "nowhere")
	if code != ExitError || !strings.Contains(out, `"error"`) {
		t.Errorf("json error: %d %q", code, out)
	}
	if code, out, _ := run(t, "version"); code != ExitOK || out != "bst "+bs.Version+"\n" {
		t.Errorf("version: %q", out)
	}
	if code, out, _ := run(t, "help"); code != ExitOK || !strings.Contains(out, "Commands:") {
		t.Errorf("help: %q", out)
	}
}

func TestParseTake(t *testing.T) {
	cases := map[string]bs.Resolution{
		"local":                       {Take: bs.TakeLocal},
		"roles=seed":                  {Table: "roles", Take: bs.TakeSeed},
		"roles/editor=local":          {Table: "roles", Key: "editor", Take: bs.TakeLocal},
		"roles/editor/name=seed":      {Table: "roles", Key: "editor", Column: "name", Take: bs.TakeSeed},
		"links/a, b/c=local":          {Table: "links", Key: "a, b", Column: "c", Take: bs.TakeLocal},
		"paths/docs/guide/title=seed": {Table: "paths", Key: "docs/guide", Column: "title", Take: bs.TakeSeed},
	}
	for in, want := range cases {
		got, err := parseTake(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "fail", "roles=both"} {
		if _, err := parseTake(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOddDatabaseName(t *testing.T) {
	// A file name with the characters a SQLite URI gives a meaning to.
	path := filepath.Join(t.TempDir(), "my app?v=1#2%41.db")
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	schema, _ := os.ReadFile(example + "schema.sql")
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the schema went somewhere else: %v", err)
	}
	if code, out, errs := run(t, "apply", "-db", path, "-seeds", example+"seeds/v1"); code != ExitOK || !strings.Contains(out, "insert") {
		t.Fatalf("apply: %d %s %s", code, out, errs)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("expected only the one database file, found %d entries", len(entries))
	}
}

func TestFlagsAfterArguments(t *testing.T) {
	db := setup(t)
	if code, _, errs := run(t, "apply", "-db", db, "-seeds", example+"seeds/v1"); code != ExitOK {
		t.Fatalf("apply: %s", errs)
	}
	code, out, errs := run(t, "explain", "roles", "editor", "-db", db, "-seeds", example+"seeds/v1", "-json")
	if code != ExitOK || !json.Valid([]byte(out)) {
		t.Fatalf("explain with flags last: %d %q %s", code, out, errs)
	}
}

func TestFlagErrorsAsJSON(t *testing.T) {
	code, out, errs := run(t, "plan", "-json", "-take", "maybe")
	var got map[string]string
	if code != ExitError || json.Unmarshal([]byte(out), &got) != nil || !strings.Contains(got["error"], "maybe") || errs != "" {
		t.Fatalf("got %d %q %q", code, out, errs)
	}
}
