// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The ledger: two tables in the application's own database, written in the
// same transaction as the data they describe.
//
//	_bootstrapping_rows     the last-applied seed values of every managed row
//	_bootstrapping_runs     one row per apply
//	_bootstrapping_changes  what each apply wrote, for history and explain
const (
	rowsTable    = "_bootstrapping_rows"
	runsTable    = "_bootstrapping_runs"
	changesTable = "_bootstrapping_changes"
)

var ledgerSchema = []string{
	`CREATE TABLE IF NOT EXISTS ` + rowsTable + ` (
		tbl     TEXT NOT NULL,
		key     TEXT NOT NULL,
		vals    TEXT NOT NULL,
		retired INTEGER NOT NULL DEFAULT 0,
		run     INTEGER NOT NULL,
		PRIMARY KEY (tbl, key)
	)`,
	`CREATE TABLE IF NOT EXISTS ` + runsTable + ` (
		id        INTEGER PRIMARY KEY,
		at        TEXT NOT NULL,
		seed_hash TEXT NOT NULL,
		version   TEXT NOT NULL,
		inserts   INTEGER NOT NULL,
		updates   INTEGER NOT NULL,
		deletes   INTEGER NOT NULL,
		retires   INTEGER NOT NULL,
		kept      INTEGER NOT NULL,
		resolved  INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ` + changesTable + ` (
		run    INTEGER NOT NULL,
		seq    INTEGER NOT NULL,
		tbl    TEXT NOT NULL,
		key    TEXT NOT NULL,
		action TEXT NOT NULL,
		col    TEXT NOT NULL DEFAULT '',
		old    TEXT,
		new    TEXT,
		note   TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (run, seq)
	)`,
}

// querier is what both *sql.DB and *sql.Conn offer.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

type colInfo struct {
	name     string
	declared string
	aff      Affinity
	pk       int
	notNull  bool
	hasDflt  bool
}

type tableInfo struct {
	name    string
	cols    map[string]colInfo
	idCol   string // the column a reference stores: the single primary key column, or rowid
	noRowid bool
}

func readTableInfo(ctx context.Context, q querier, table string) (*tableInfo, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, type, \"notnull\", dflt_value, pk FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ti := &tableInfo{name: table, cols: map[string]colInfo{}}
	var pks []colInfo
	for rows.Next() {
		var c colInfo
		var notNull int
		var dflt sql.NullString
		if err := rows.Scan(&c.name, &c.declared, &notNull, &dflt, &c.pk); err != nil {
			return nil, err
		}
		c.aff, c.notNull, c.hasDflt = affinityOf(c.declared), notNull != 0, dflt.Valid
		ti.cols[c.name] = c
		if c.pk > 0 {
			pks = append(pks, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ti.cols) == 0 {
		return nil, fmt.Errorf("table %q doesn't exist in the database (create it with your migrations first)", table)
	}
	var ddl string
	r, err := q.QueryContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table)
	if err != nil {
		return nil, err
	}
	if r.Next() {
		var s sql.NullString
		if err := r.Scan(&s); err != nil {
			r.Close()
			return nil, err
		}
		ddl = s.String
	}
	r.Close()
	ti.noRowid = strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID")
	switch {
	case len(pks) == 1:
		ti.idCol = pks[0].name
	case !ti.noRowid:
		ti.idCol = "rowid"
	}
	return ti, nil
}

func ledgerExists(ctx context.Context, q querier) (bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN (?, ?, ?)", rowsTable, runsTable, changesTable)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
	}
	return n == 3, rows.Err()
}

// Init creates the ledger tables if they don't exist. Apply does this on its
// own; Init is there for setups that create every table up front.
func Init(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return createLedger(ctx, conn)
}

func createLedger(ctx context.Context, q querier) error {
	for _, s := range ledgerSchema {
		if _, err := q.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("creating the ledger: %w", err)
		}
	}
	return nil
}

// ledgerRow is a managed row as the ledger remembers it: the seed values
// last applied.
type ledgerRow struct {
	key     string
	keyVals []Value
	vals    map[string]Value
	retired bool
}

func readLedger(ctx context.Context, q querier, table string) (map[string]*ledgerRow, error) {
	out := map[string]*ledgerRow{}
	rows, err := q.QueryContext(ctx, "SELECT key, vals, retired FROM "+rowsTable+" WHERE tbl = ?", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, vals string
		var retired int
		if err := rows.Scan(&key, &vals, &retired); err != nil {
			return nil, err
		}
		kv, err := decodeKey(key)
		if err != nil {
			return nil, fmt.Errorf("ledger row %s/%s: %w", table, key, err)
		}
		vm, err := decodeVals(vals)
		if err != nil {
			return nil, fmt.Errorf("ledger row %s/%s: %w", table, key, err)
		}
		out[key] = &ledgerRow{key: key, keyVals: kv, vals: vm, retired: retired != 0}
	}
	return out, rows.Err()
}

func encodeKey(vals []Value) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = v.encode()
	}
	b, _ := json.Marshal(parts)
	return string(b)
}

func decodeKey(s string) ([]Value, error) {
	var parts []string
	if err := json.Unmarshal([]byte(s), &parts); err != nil {
		return nil, err
	}
	out := make([]Value, len(parts))
	for i, p := range parts {
		v, err := decodeValue(p)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func encodeVals(m map[string]Value) string {
	enc := make(map[string]string, len(m))
	for k, v := range m {
		enc[k] = v.encode()
	}
	b, _ := json.Marshal(enc) // map keys are sorted, so equal maps encode equally
	return string(b)
}

func decodeVals(s string) (map[string]Value, error) {
	var enc map[string]string
	if err := json.Unmarshal([]byte(s), &enc); err != nil {
		return nil, err
	}
	out := make(map[string]Value, len(enc))
	for k, e := range enc {
		v, err := decodeValue(e)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// readRows reads the given columns of every row of a table. Each column is
// read as +column, an expression with no declared type, so the driver
// hands back what SQLite stored rather than converting by type name (as
// drivers do for BOOLEAN, DATE and DATETIME columns).
func readRows(ctx context.Context, q querier, table string, cols []string) ([][]Value, error) {
	qc := make([]string, len(cols))
	for i, c := range cols {
		qc[i] = "+" + quoteIdent(c)
	}
	rows, err := q.QueryContext(ctx, "SELECT "+strings.Join(qc, ", ")+" FROM "+quoteIdent(table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]Value
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		vals := make([]Value, len(cols))
		for i, x := range raw {
			vals[i] = fromDB(x)
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// Run is one apply, as History reports it.
type Run struct {
	ID       int64  `json:"id"`
	At       string `json:"at"`
	SeedHash string `json:"seed_hash"`
	Version  string `json:"version"`
	Inserts  int    `json:"inserts"`
	Updates  int    `json:"updates"`
	Deletes  int    `json:"deletes"`
	Retires  int    `json:"retires"`
	Kept     int    `json:"kept"`
	Resolved int    `json:"resolved"`
}

// History returns the last n applies, newest first (all of them if n <= 0).
func History(ctx context.Context, db *sql.DB, n int) ([]Run, error) {
	ok, err := ledgerExists(ctx, db)
	if err != nil || !ok {
		return nil, err
	}
	q := "SELECT id, at, seed_hash, version, inserts, updates, deletes, retires, kept, resolved FROM " + runsTable + " ORDER BY id DESC"
	if n > 0 {
		q += fmt.Sprintf(" LIMIT %d", n)
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.At, &r.SeedHash, &r.Version, &r.Inserts, &r.Updates, &r.Deletes, &r.Retires, &r.Kept, &r.Resolved); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoggedChange is one entry of the change log kept by Apply.
type LoggedChange struct {
	Run    int64  `json:"run"`
	At     string `json:"at"`
	Action string `json:"action"`
	Column string `json:"column,omitempty"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
	Note   string `json:"note,omitempty"`
}

func readChangeLog(ctx context.Context, q querier, table, key string) ([]LoggedChange, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.run, r.at, c.action, c.col, c.old, c.new, c.note
		FROM `+changesTable+` c JOIN `+runsTable+` r ON r.id = c.run
		WHERE c.tbl = ? AND c.key = ? ORDER BY c.run, c.seq`, table, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LoggedChange
	for rows.Next() {
		var l LoggedChange
		var old, nw sql.NullString
		if err := rows.Scan(&l.Run, &l.At, &l.Action, &l.Column, &old, &nw, &l.Note); err != nil {
			return nil, err
		}
		if old.Valid {
			if v, err := decodeValue(old.String); err == nil {
				l.Old = v.String()
			}
		}
		if nw.Valid {
			if v, err := decodeValue(nw.String); err == nil {
				l.New = v.String()
			}
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

var errNoLedger = errors.New("no ledger")
