// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Command bootstrapping-demo runs an application's roles and permissions
// through three seed versions, with an admin's edits in between, a
// conflict, and an apply killed halfway through. It checks every result and
// ends with "All checks passed."
//
//	go run ./examples/permissions
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	bs "github.com/bootstrapping-engine/bootstrapping"
	"github.com/bootstrapping-engine/bootstrapping/internal/cli"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql seeds
var files embed.FS

var clock = func() time.Time { return time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) }

func main() {
	child := flag.String("crash-child", "", "internal: apply v3 to this database and stop halfway")
	dir := flag.String("dir", "", "keep the demo database in this directory instead of a temporary one")
	flag.Parse()
	if *child != "" {
		crashChild(*child)
		return
	}
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "\nFAILED:", err)
		os.Exit(1)
	}
}

func seeds(version string) *bs.Set {
	sub, err := fs.Sub(files, "seeds/"+version)
	must(err)
	set, err := bs.LoadFS(sub, ".")
	must(err)
	return set
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

func open(path string) *sql.DB {
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=30000")
	must(err)
	return db
}

func show(p *bs.Plan) {
	var b bytes.Buffer
	cli.RenderPlan(&b, p, false)
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if line == "" {
			fmt.Println()
		} else {
			fmt.Println("   " + line)
		}
	}
}

type checker struct {
	db *sql.DB
	n  int
}

func (c *checker) value(q string, want string) error {
	var s sql.NullString
	if err := c.db.QueryRow(q).Scan(&s); err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	got := "NULL"
	if s.Valid {
		got = s.String
	}
	c.n++
	if got != want {
		return fmt.Errorf("%s: got %q, want %q", q, got, want)
	}
	return nil
}

// state is the whole of the demo's tables, as text, for before-and-after
// comparisons.
func state(db *sql.DB) string {
	var b strings.Builder
	for _, q := range []string{
		"SELECT code, name, coalesce(description, ''), system, coalesce(retired_at, '') FROM roles ORDER BY code",
		"SELECT code, coalesce(description, ''), coalesce(retired_at, '') FROM permissions ORDER BY code",
		"SELECT r.code, p.code FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id ORDER BY 1, 2",
		"SELECT count(*) FROM _bootstrapping_runs",
		"SELECT tbl || ' ' || key || ' ' || vals || ' ' || retired FROM _bootstrapping_rows ORDER BY tbl, key",
	} {
		rows, err := db.Query(q)
		must(err)
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			must(rows.Scan(ptrs...))
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
	}
	return b.String()
}

func run(dir string) error {
	ctx := context.Background()
	if dir == "" {
		tmp, err := os.MkdirTemp("", "bootstrapping-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	path := filepath.Join(dir, "app.db")
	os.Remove(path)
	db := open(path)
	defer db.Close()
	schema, err := files.ReadFile("schema.sql")
	must(err)
	if _, err := db.Exec(string(schema)); err != nil {
		return err
	}
	opts := &bs.Options{Now: clock}
	c := &checker{db: db}

	fmt.Println("Bootstrapping demo: an application's roles and permissions, through three")
	fmt.Println("seed versions, with an admin's edits in between.")

	fmt.Println("\n1. Ship v1 to an empty database.")
	p, err := bs.Apply(ctx, db, seeds("v1"), opts)
	if err != nil {
		return err
	}
	fmt.Printf("   %s\n", cli.Summary(p))
	for _, e := range []error{
		c.value("SELECT count(*) FROM roles", "3"),
		c.value("SELECT count(*) FROM permissions", "5"),
		c.value("SELECT count(*) FROM role_permissions", "10"),
	} {
		if e != nil {
			return e
		}
	}

	fmt.Println("\n2. An admin renames the Editor role to \"Content Editor\" and takes")
	fmt.Println("   posts.delete away from editors.")
	if _, err := db.Exec("UPDATE roles SET name = 'Content Editor' WHERE code = 'editor'"); err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM role_permissions
		WHERE role_id = (SELECT id FROM roles WHERE code = 'editor')
		AND permission_id = (SELECT id FROM permissions WHERE code = 'posts.delete')`); err != nil {
		return err
	}

	fmt.Println("\n3. Ship v2: a billing permission for admins, Viewer renamed to Reader,")
	fmt.Println("   and a new description for Editor. The plan:")
	fmt.Println()
	if p, err = bs.Preview(ctx, db, seeds("v2"), opts); err != nil {
		return err
	}
	show(p)
	if p, err = bs.Apply(ctx, db, seeds("v2"), opts); err != nil {
		return err
	}
	fmt.Printf("   %s\n", cli.Summary(p))
	for _, e := range []error{
		c.value("SELECT name FROM roles WHERE code = 'editor'", "Content Editor"),
		c.value("SELECT description FROM roles WHERE code = 'editor'", "Writes, edits and publishes posts"),
		c.value("SELECT name FROM roles WHERE code = 'viewer'", "Reader"),
		c.value("SELECT count(*) FROM role_permissions", "10"),
		c.value(`SELECT count(*) FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id
			WHERE r.code = 'editor' AND p.code = 'posts.delete'`, "0"),
		c.value(`SELECT count(*) FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id
			WHERE r.code = 'admin' AND p.code = 'billing.view'`, "1"),
	} {
		if e != nil {
			return e
		}
	}

	fmt.Println("\n4. Ship v3. It drops users.manage, and it renames Editor too, to")
	fmt.Println("   \"Editorial\". That's a conflict with the admin's name, so the apply")
	fmt.Println("   stops before writing anything:")
	fmt.Println()
	p, err = bs.Apply(ctx, db, seeds("v3"), opts)
	if !errors.Is(err, bs.ErrBlocked) {
		return fmt.Errorf("v3 should be blocked by a conflict, got %v", err)
	}
	show(p)
	if err := c.value("SELECT coalesce(retired_at, 'NULL') FROM permissions WHERE code = 'users.manage'", "NULL"); err != nil {
		return err
	}

	fmt.Println("\n5. Settle it by keeping the admin's name, and kill the apply halfway")
	fmt.Println("   through, after its first statement.")
	before := state(db)
	stmts, err := crash(path)
	if err != nil {
		return err
	}
	after := state(db)
	c.n++
	if before != after {
		return fmt.Errorf("the killed apply left changes behind:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if err := c.value("PRAGMA integrity_check", "ok"); err != nil {
		return err
	}
	fmt.Printf("   Killed after %d statement. The database is exactly as v2 left it,\n", stmts)
	fmt.Println("   and SQLite's integrity check passes.")

	fmt.Println("\n6. Run the same apply again.")
	opts.Take = []bs.Resolution{{Table: "roles", Key: "editor", Column: "name", Take: bs.TakeLocal}}
	if p, err = bs.Apply(ctx, db, seeds("v3"), opts); err != nil {
		return err
	}
	fmt.Printf("   %s\n", cli.Summary(p))
	for _, e := range []error{
		c.value("SELECT name FROM roles WHERE code = 'editor'", "Content Editor"),
		c.value("SELECT retired_at FROM permissions WHERE code = 'users.manage'", "2026-11-02T08:00:00Z"),
		c.value("SELECT count(*) FROM role_permissions", "9"),
		c.value("SELECT count(*) FROM _bootstrapping_runs", "3"),
	} {
		if e != nil {
			return e
		}
	}
	opts.Take = nil
	if p, err = bs.Preview(ctx, db, seeds("v3"), opts); err != nil {
		return err
	}
	c.n++
	if p.Pending() || p.Blocked() {
		return fmt.Errorf("after v3 the plan should be empty: %s", cli.Summary(p))
	}
	fmt.Printf("   The next plan: %s\n", cli.Summary(p))

	fmt.Printf("\nAll checks passed (%d).\n", c.n)
	return nil
}

// crash runs the v3 apply in a child process and kills it once the child
// says it has written its first statement inside the transaction.
func crash(path string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(self, "-crash-child", path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready ") {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("the child apply didn't reach its first statement: %q %v", line, err)
	}
	var n int
	fmt.Sscanf(strings.TrimPrefix(line, "ready "), "%d", &n)
	if err := cmd.Process.Kill(); err != nil {
		return 0, err
	}
	cmd.Wait()
	return n, nil
}

func crashChild(path string) {
	db := open(path)
	n := 0
	opts := &bs.Options{Now: clock,
		Take: []bs.Resolution{{Table: "roles", Key: "editor", Column: "name", Take: bs.TakeLocal}},
		Progress: func(bs.Change) {
			n++
			fmt.Printf("ready %d\n", n)
			os.Stdout.Sync()
			time.Sleep(time.Hour) // the parent kills us here, mid-transaction
		}}
	_, err := bs.Apply(context.Background(), db, seeds("v3"), opts)
	fmt.Fprintln(os.Stderr, "the child apply finished, which it shouldn't:", err)
	os.Exit(1)
}
