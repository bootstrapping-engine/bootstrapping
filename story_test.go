// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bs "github.com/bootstrapping-engine/bootstrapping"
	_ "github.com/mattn/go-sqlite3"
)

// The demo's story, step by step, with every expectation spelled out: what
// each plan says and what the tables hold afterwards.

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=10000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func dbExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(rebind(db, q), args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func loadSchema(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dbExec(t, db, string(b))
}

func loadSeeds(t *testing.T, dir string) *bs.Set {
	t.Helper()
	s, err := bs.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var fixed = &bs.Options{Now: func() time.Time { return time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) }}

func query1(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(rebind(db, q), args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if !s.Valid {
		return "NULL"
	}
	return s.String
}

type want struct {
	table, key string
	action     bs.Action
	column     string
}

func checkPlan(t *testing.T, p *bs.Plan, wants []want) {
	t.Helper()
	var got []want
	for _, c := range p.Changes {
		if c.Action == bs.ActRecord {
			continue
		}
		got = append(got, want{c.Table, c.KeyText(), c.Action, c.Column})
	}
	if len(got) != len(wants) {
		t.Fatalf("plan has %d changes, want %d:\n got %v\nwant %v", len(got), len(wants), got, wants)
	}
	for i := range got {
		if got[i] != wants[i] {
			t.Fatalf("change %d: got %v, want %v\nall: %v", i, got[i], wants[i], got)
		}
	}
}

func TestStory(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		ctx := context.Background()
		db := b.open(t)
		schema, err := os.ReadFile("examples/permissions/schema.sql")
		if err != nil {
			t.Fatal(err)
		}
		dbExec(t, db, b.ddl(string(schema)))
		story(t, ctx, db)
	})
}

func story(t *testing.T, ctx context.Context, db *sql.DB) {
	v1 := loadSeeds(t, "examples/permissions/seeds/v1")
	v2 := loadSeeds(t, "examples/permissions/seeds/v2")
	v3 := loadSeeds(t, "examples/permissions/seeds/v3")

	// v1 on an empty database: everything inserts.
	p, err := bs.Apply(ctx, db, v1, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Counts(); c.Inserts != 18 || p.Writes() != 18 {
		t.Fatalf("v1: %+v", c)
	}
	if n := query1(t, db, "SELECT count(*) FROM role_permissions"); n != "10" {
		t.Fatalf("links: %s", n)
	}
	// Applying the same seed again changes nothing and records no run.
	if p, err = bs.Apply(ctx, db, v1, fixed); err != nil || p.Pending() || p.Applied {
		t.Fatalf("second apply: pending=%v applied=%v err=%v %v", p.Pending(), p.Applied, err, p.Changes)
	}

	// An admin renames a role and takes a permission away.
	dbExec(t, db, "UPDATE roles SET name = 'Content Editor' WHERE code = 'editor'")
	dbExec(t, db, `DELETE FROM role_permissions WHERE role_id = (SELECT id FROM roles WHERE code = 'editor')
		AND permission_id = (SELECT id FROM permissions WHERE code = 'posts.delete')`)

	p, err = bs.Preview(ctx, db, v2, fixed)
	if err != nil {
		t.Fatal(err)
	}
	checkPlan(t, p, []want{
		{"permissions", "billing.view", bs.ActInsert, ""},
		{"roles", "editor", bs.ActUpdate, "description"},
		{"roles", "editor", bs.ActKeepEdit, "name"},
		{"roles", "viewer", bs.ActUpdate, "name"},
		{"role_permissions", "admin, billing.view", bs.ActInsert, ""},
		{"role_permissions", "editor, posts.delete", bs.ActKeepDelete, ""},
	})
	if _, err = bs.Apply(ctx, db, v2, fixed); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, db, "SELECT name || '|' || description FROM roles WHERE code = 'editor'"); got != "Content Editor|Writes, edits and publishes posts" {
		t.Fatalf("editor after v2: %s", got)
	}
	if got := query1(t, db, "SELECT name FROM roles WHERE code = 'viewer'"); got != "Reader" {
		t.Fatalf("viewer after v2: %s", got)
	}
	if n := query1(t, db, "SELECT count(*) FROM role_permissions"); n != "10" {
		t.Fatalf("links after v2: %s", n)
	}

	// v3 renames Editor too: a conflict, which blocks the apply.
	p, err = bs.Apply(ctx, db, v3, fixed)
	if !errors.Is(err, bs.ErrBlocked) {
		t.Fatalf("v3 apply: %v", err)
	}
	checkPlan(t, p, []want{
		{"permissions", "users.manage", bs.ActRetire, ""},
		{"roles", "editor", bs.ActConflict, "name"},
		{"role_permissions", "editor, posts.delete", bs.ActKeepDelete, ""},
		{"role_permissions", "admin, users.manage", bs.ActDelete, ""},
	})
	if got := query1(t, db, "SELECT retired_at FROM permissions WHERE code = 'users.manage'"); got != "NULL" {
		t.Fatalf("a blocked apply wrote: %s", got)
	}

	// Keep the local name.
	opts := *fixed
	opts.Take = []bs.Resolution{{Table: "roles", Key: "editor", Column: "name", Take: bs.TakeLocal}}
	if p, err = bs.Apply(ctx, db, v3, &opts); err != nil {
		t.Fatal(err)
	}
	if c := p.Counts(); c.Resolved != 1 || c.Retires != 1 || c.Deletes != 1 {
		t.Fatalf("v3: %+v", c)
	}
	if got := query1(t, db, "SELECT name FROM roles WHERE code = 'editor'"); got != "Content Editor" {
		t.Fatalf("editor after v3: %s", got)
	}
	if got := query1(t, db, "SELECT retired_at FROM permissions WHERE code = 'users.manage'"); got != "2026-11-02T08:00:00Z" {
		t.Fatalf("retired_at: %s", got)
	}
	if n := query1(t, db, "SELECT count(*) FROM role_permissions"); n != "9" {
		t.Fatalf("links after v3: %s", n)
	}
	// And the conflict doesn't come back.
	if p, err = bs.Preview(ctx, db, v3, fixed); err != nil || p.Pending() {
		t.Fatalf("after v3: pending=%v %v %v", p.Pending(), err, p.Changes)
	}
	// History has three runs.
	runs, err := bs.History(ctx, db, 0)
	if err != nil || len(runs) != 3 {
		t.Fatalf("history: %v %v", runs, err)
	}
}
