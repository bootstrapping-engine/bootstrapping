// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

// scale multiplies the size of the long tests. CI runs them at 1;
// scripts/record-tests.sh runs them at 10 and records the results.
func scale() int {
	if n, err := strconv.Atoi(os.Getenv("BS_SCALE")); err == nil && n > 0 {
		return n
	}
	return 1
}

// record writes a test's figures to $BS_RESULTS/name.json, when set.
func record(t *testing.T, name string, v any) {
	dir := os.Getenv("BS_RESULTS")
	if dir == "" {
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openPath(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=30000")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// setFrom builds a seed set from file name -> contents.
func setFrom(t testing.TB, files map[string]string) *bs.Set {
	t.Helper()
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	s, err := bs.LoadFS(m, ".")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// dump is every row of every table, ledger included, as text.
func dump(t testing.TB, db *sql.DB) string {
	t.Helper()
	q := "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name"
	if isPG(db) {
		q = "SELECT tablename FROM pg_tables WHERE schemaname = current_schema() ORDER BY tablename"
	}
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var b strings.Builder
	for _, tbl := range tables {
		r, err := db.Query(`SELECT * FROM "` + tbl + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		// PostgreSQL never gives back a sequence's numbers, even when the
		// transaction that drew them rolls back, so after a failed apply the
		// next one's new rows get other ids than a clean apply's would. The
		// test tables' ids are left out there; everything else must match.
		skip := -1
		if isPG(db) && tbl == "items" {
			for i, c := range cols {
				if c == "id" {
					skip = i
				}
			}
		}
		var lines []string
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if i == skip {
					continue
				}
				if bb, ok := v.([]byte); ok {
					v = string(bb)
				}
				parts[i] = fmt.Sprintf("%T:%v", v, v)
			}
			lines = append(lines, strings.Join(parts, "|"))
		}
		r.Close()
		sort.Strings(lines)
		fmt.Fprintf(&b, "== %s\n%s\n", tbl, strings.Join(lines, "\n"))
	}
	return b.String()
}

func mapFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}
