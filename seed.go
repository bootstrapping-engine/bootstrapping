// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Own says who owns a column's value once a row exists.
type Own string

const (
	// OwnMerge is the default: a three-way merge between the new seed, the
	// live row and the last-applied seed. An admin's edit is kept; a seed
	// change to an untouched value lands; both changed is a conflict.
	OwnMerge Own = "merge"
	// OwnSeed means the seed always wins, for values the application relies
	// on, such as a system flag.
	OwnSeed Own = "seed"
	// OwnLocal means the seed sets the value when the row is inserted and
	// never touches it again.
	OwnLocal Own = "local"
)

// Take is how a conflict is settled.
type Take string

const (
	TakeFail  Take = "fail"  // block the apply until someone decides
	TakeSeed  Take = "seed"  // the seed's value wins
	TakeLocal Take = "local" // the live value stays
)

// Remove is what happens to a row once it is dropped from the seed.
type Remove string

const (
	RemoveKeep   Remove = "keep"   // leave the row; stop managing it
	RemoveDelete Remove = "delete" // delete it, unless it was edited locally
	RemoveRetire Remove = "retire" // set the retire column to the time of the apply
)

// Column is one managed column of a seed table.
type Column struct {
	Name       string
	Own        Own
	OnConflict Take // empty means the table's default
	Ref        *Ref
}

// Ref says a column holds a reference to another table's row, written in
// the seed by that row's natural key: `role_id = "roles.code"` lets a seed
// say role_id = "editor" and stores the editor row's primary key.
type Ref struct {
	Table  string
	Column string
}

func (r Ref) String() string { return r.Table + "." + r.Column }

// Table is one seed file: the rows a table should contain and the rules for
// keeping them in step.
type Table struct {
	Name          string
	File          string
	Key           []string  // the natural key, the columns that identify a row
	Columns       []*Column // every managed column, key columns first
	OnRemove      Remove
	RetireColumn  string
	OnConflict    Take   // default for columns and row-level conflicts
	OnLocalDelete string // "respect" (default) or "restore"
	Rows          []SeedRow
}

// SeedRow is one [[row]] as written in the file.
type SeedRow struct {
	Index       int            // position in the file, from 1
	Values      map[string]any // column -> value as TOML decoded it; absent means NULL
	RenamedFrom []any          // the row's earlier natural key, if it was renamed
	Where       string         // where the row was written, for messages: "roles.toml: row 3" or "countries.csv: line 12"
}

func (r SeedRow) where(t *Table) string {
	if r.Where != "" {
		return r.Where
	}
	return fmt.Sprintf("%s: row %d", t.File, r.Index)
}

// Column returns the managed column with this name, or nil.
func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (t *Table) isKey(name string) bool {
	for _, k := range t.Key {
		if k == name {
			return true
		}
	}
	return false
}

func (t *Table) conflictPolicy(c *Column) Take {
	if c != nil && c.OnConflict != "" {
		return c.OnConflict
	}
	return t.OnConflict
}

// Set is every seed table, in the order they apply: a table comes after the
// tables its references point at.
type Set struct {
	Tables []*Table
	Hash   string // SHA-256 of the seed files' names and contents
}

// Table returns the seed table with this name, or nil.
func (s *Set) Table(name string) *Table {
	for _, t := range s.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// LoadDir reads every .toml file in a directory as a seed set, with the
// CSV files they name for their rows.
func LoadDir(dir string) (*Set, error) {
	if st, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("seed directory: %w", err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("seed directory: %s is not a directory", dir)
	}
	return LoadFS(os.DirFS(dir), ".")
}

// LoadFS reads every .toml file in dir of fsys as a seed set. Use it with
// go:embed to ship the seeds inside the application.
func LoadFS(fsys fs.FS, dir string) (*Set, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("seed directory: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("seed directory %s has no .toml files", dir)
	}
	h := sha256.New()
	set := &Set{}
	seen := map[string]string{}
	for _, name := range names {
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(data))
		h.Write(data)
		read := func(file string) ([]byte, error) {
			b, err := fs.ReadFile(fsys, path.Join(dir, file))
			if err == nil {
				fmt.Fprintf(h, "%s\x00%d\x00", file, len(b))
				h.Write(b)
			}
			return b, err
		}
		t, err := parseTable(name, data, read)
		if err != nil {
			return nil, err
		}
		if prev, ok := seen[t.Name]; ok {
			return nil, fmt.Errorf("%s: table %q is already seeded by %s", name, t.Name, prev)
		}
		seen[t.Name] = name
		set.Tables = append(set.Tables, t)
	}
	set.Hash = hex.EncodeToString(h.Sum(nil))
	if err := set.order(); err != nil {
		return nil, err
	}
	return set, nil
}

// order sorts the tables so each comes after the seed tables it references,
// keeping file order otherwise.
func (s *Set) order() error {
	byName := map[string]*Table{}
	for _, t := range s.Tables {
		byName[t.Name] = t
	}
	var out []*Table
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(t *Table, trail []string) error
	visit = func(t *Table, trail []string) error {
		switch state[t.Name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("seed tables reference each other in a loop: %s", strings.Join(append(trail, t.Name), " -> "))
		}
		state[t.Name] = 1
		for _, c := range t.Columns {
			if c.Ref == nil || c.Ref.Table == t.Name {
				continue
			}
			if dep, ok := byName[c.Ref.Table]; ok {
				if err := visit(dep, append(trail, t.Name)); err != nil {
					return err
				}
			}
		}
		state[t.Name] = 2
		out = append(out, t)
		return nil
	}
	for _, t := range s.Tables {
		if err := visit(t, nil); err != nil {
			return err
		}
	}
	s.Tables = out
	return nil
}

// ParseTable reads one seed file. name is used in error messages. A file
// whose rows are in a CSV file needs LoadDir or LoadFS, which can read it.
func ParseTable(name string, data []byte) (*Table, error) {
	return parseTable(name, data, func(file string) ([]byte, error) {
		return nil, fmt.Errorf("can't read %s: use LoadDir or LoadFS for seed files with rows in a CSV file", file)
	})
}

func parseTable(name string, data []byte, read func(string) ([]byte, error)) (*Table, error) {
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{name}, args...)...)
	}
	t := &Table{File: name, OnRemove: RemoveKeep, OnConflict: TakeFail, OnLocalDelete: "respect"}
	for k := range raw {
		switch k {
		case "table", "key", "columns", "refs", "on_remove", "on_conflict", "on_local_delete", "row", "rows":
		default:
			return nil, fail("unknown setting %q (expected table, key, columns, refs, on_remove, on_conflict, on_local_delete, rows or [[row]])", k)
		}
	}

	var ok bool
	if t.Name, ok = raw["table"].(string); !ok || strings.TrimSpace(t.Name) == "" {
		return nil, fail(`missing table = "name"`)
	}
	switch k := raw["key"].(type) {
	case string:
		t.Key = []string{k}
	case []any:
		for _, x := range k {
			s, ok := x.(string)
			if !ok || s == "" {
				return nil, fail("key must be a column name or a list of column names")
			}
			t.Key = append(t.Key, s)
		}
	case nil:
		return nil, fail(`missing key = "column" (the natural key that identifies a row)`)
	default:
		return nil, fail("key must be a column name or a list of column names")
	}
	if len(t.Key) == 0 {
		return nil, fail("key is empty")
	}
	if dup := firstDuplicate(t.Key); dup != "" {
		return nil, fail("key names column %q twice", dup)
	}

	if v, ok := raw["on_conflict"]; ok {
		s, _ := v.(string)
		if t.OnConflict = Take(s); !validTake(t.OnConflict) {
			return nil, fail(`on_conflict must be "fail", "seed" or "local"`)
		}
	}
	if v, ok := raw["on_local_delete"]; ok {
		s, _ := v.(string)
		if s != "respect" && s != "restore" {
			return nil, fail(`on_local_delete must be "respect" or "restore"`)
		}
		t.OnLocalDelete = s
	}
	switch v := raw["on_remove"].(type) {
	case nil:
	case string:
		switch Remove(v) {
		case RemoveKeep, RemoveDelete:
			t.OnRemove = Remove(v)
		case RemoveRetire:
			return nil, fail(`on_remove = "retire" needs the column to set: on_remove = { retire = "retired_at" }`)
		default:
			return nil, fail(`on_remove must be "keep", "delete" or { retire = "column" }`)
		}
	case map[string]any:
		col, ok := v["retire"].(string)
		if !ok || col == "" || len(v) != 1 {
			return nil, fail(`on_remove must be "keep", "delete" or { retire = "column" }`)
		}
		t.OnRemove, t.RetireColumn = RemoveRetire, col
	default:
		return nil, fail(`on_remove must be "keep", "delete" or { retire = "column" }`)
	}

	declared := map[string]*Column{}
	if v, ok := raw["columns"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fail("[columns] must be a table of column = ownership")
		}
		for col, spec := range m {
			c := &Column{Name: col}
			switch s := spec.(type) {
			case string:
				c.Own = Own(s)
			case map[string]any:
				for k := range s {
					if k != "own" && k != "on_conflict" {
						return nil, fail("column %q: unknown setting %q (expected own and on_conflict)", col, k)
					}
				}
				own, _ := s["own"].(string)
				if own == "" {
					own = string(OwnMerge)
				}
				c.Own = Own(own)
				if oc, ok := s["on_conflict"]; ok {
					o, _ := oc.(string)
					if c.OnConflict = Take(o); !validTake(c.OnConflict) {
						return nil, fail(`column %q: on_conflict must be "fail", "seed" or "local"`, col)
					}
				}
			default:
				return nil, fail(`column %q: expected "merge", "seed", "local" or { own = ..., on_conflict = ... }`, col)
			}
			if c.Own != OwnMerge && c.Own != OwnSeed && c.Own != OwnLocal {
				return nil, fail(`column %q: ownership must be "merge", "seed" or "local"`, col)
			}
			if t.isKey(col) {
				return nil, fail("column %q is part of the key; key columns identify rows and have no ownership", col)
			}
			declared[col] = c
		}
	}
	refs := map[string]*Ref{}
	if v, ok := raw["refs"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fail(`[refs] must be a table of column = "table.column"`)
		}
		for col, spec := range m {
			s, _ := spec.(string)
			tbl, c, found := strings.Cut(s, ".")
			if !found || tbl == "" || c == "" || strings.Contains(c, ".") {
				return nil, fail(`ref %q must be "table.column", the referenced table and the column that names its rows`, col)
			}
			refs[col] = &Ref{Table: tbl, Column: c}
		}
	}

	rowsRaw, _ := raw["row"].([]map[string]any)
	if raw["row"] != nil && rowsRaw == nil {
		return nil, fail("rows must be written as [[row]] tables")
	}
	var wheres []string
	if spec, ok := raw["rows"]; ok {
		if raw["row"] != nil {
			return nil, fail("rows come from [[row]] tables or from a CSV file with rows = \"file.csv\", not both")
		}
		var err error
		if rowsRaw, wheres, err = readCSVRows(spec, read); err != nil {
			return nil, fail("%v", err)
		}
	}
	cols := map[string]bool{}
	for i, r := range rowsRaw {
		row := SeedRow{Index: i + 1, Values: map[string]any{}}
		if wheres != nil {
			row.Where = wheres[i]
		}
		rowFail := func(format string, args ...any) error {
			if row.Where != "" {
				return fail("%s"+format, append([]any{row.Where}, args...)...)
			}
			return fail("row %d"+format, append([]any{i + 1}, args...)...)
		}
		for k, v := range r {
			if k == "_renamed_from" {
				switch x := v.(type) {
				case []any:
					row.RenamedFrom = x
				default:
					row.RenamedFrom = []any{x}
				}
				if len(row.RenamedFrom) != len(t.Key) {
					return nil, rowFail(": _renamed_from needs %d value(s), one per key column", len(t.Key))
				}
				continue
			}
			if strings.HasPrefix(k, "_") {
				return nil, rowFail(": unknown setting %q (names starting with _ are reserved; the only one is _renamed_from)", k)
			}
			if err := checkScalar(v); err != nil {
				return nil, rowFail(", column %q: %v", k, err)
			}
			row.Values[k] = v
			cols[k] = true
		}
		for _, k := range t.Key {
			if _, ok := row.Values[k]; !ok {
				return nil, rowFail(" has no value for key column %q", k)
			}
		}
		if t.RetireColumn != "" {
			if _, ok := row.Values[t.RetireColumn]; ok {
				return nil, rowFail(" sets %q, the retire column; that column is managed by on_remove", t.RetireColumn)
			}
		}
		t.Rows = append(t.Rows, row)
	}

	for _, k := range t.Key {
		t.Columns = append(t.Columns, &Column{Name: k, Own: OwnSeed, Ref: refs[k]})
	}
	var rest []string
	for c := range cols {
		if !t.isKey(c) {
			rest = append(rest, c)
		}
	}
	for c := range declared {
		if !cols[c] {
			rest = append(rest, c)
		}
	}
	for c := range refs { // a declared reference no row sets is managed, as NULL
		if !cols[c] && declared[c] == nil && !t.isKey(c) {
			rest = append(rest, c)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		c := declared[name]
		if c == nil {
			c = &Column{Name: name, Own: OwnMerge}
		}
		c.Ref = refs[name]
		t.Columns = append(t.Columns, c)
	}
	if t.RetireColumn != "" && t.Column(t.RetireColumn) != nil {
		return nil, fail("%q is the retire column and can't also be a managed column", t.RetireColumn)
	}
	return t, nil
}

func validTake(t Take) bool { return t == TakeFail || t == TakeSeed || t == TakeLocal }

func firstDuplicate(xs []string) string {
	seen := map[string]bool{}
	for _, x := range xs {
		if seen[x] {
			return x
		}
		seen[x] = true
	}
	return ""
}

func checkScalar(v any) error {
	switch v.(type) {
	case string, int64, float64, bool, time.Time:
		return nil
	case []any, map[string]any, []map[string]any:
		return errors.New("lists and tables can't be stored in a column; use a string")
	}
	return fmt.Errorf("unsupported value of type %T", v)
}

// seedTime renders a TOML date or time the way it was written: a local
// date stays 2026-10-18, a local time 09:30:00, an offset date-time is
// stored in UTC.
func seedTime(t time.Time) string {
	switch t.Location().String() {
	case "date-local":
		return t.Format("2006-01-02")
	case "time-local":
		return t.Format("15:04:05.999999999")
	case "datetime-local":
		return t.Format("2006-01-02T15:04:05.999999999")
	}
	return t.UTC().Format(time.RFC3339Nano)
}
