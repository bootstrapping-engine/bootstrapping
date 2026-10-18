// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Options tune Preview and Apply. The zero value is fine.
type Options struct {
	// Take settles conflicts. The first resolution that matches a conflict
	// wins; a conflict none matches follows the seed file's on_conflict.
	Take []Resolution
	// Now is the clock for retire timestamps and the run record. Defaults to
	// time.Now.
	Now func() time.Time
	// Progress, if set, is called after each statement Apply writes, with the
	// change it belongs to. It runs inside the transaction.
	Progress func(Change)
}

func (o *Options) resolve(table, key, column string, fallback Take) Take {
	if o != nil {
		for _, r := range o.Take {
			if r.matches(table, key, column) {
				return r.Take
			}
		}
	}
	return fallback
}

func (o *Options) now() time.Time {
	if o != nil && o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// ErrBlocked is returned by Apply when a conflict nobody settled stops it.
// Nothing is written. The plan that comes back with it lists the conflicts.
var ErrBlocked = errors.New("conflicts block the apply; settle them with on_conflict in the seed file or a Take resolution")

// Apply brings the database in line with the seed set in one transaction:
// it plans again inside the transaction, writes the data and the ledger
// together, and commits. If a conflict blocks it, it writes nothing and
// returns the plan with ErrBlocked. If anything fails, nothing is written.
//
// Apply uses one connection from db and holds SQLite's write lock for the
// whole transaction (BEGIN IMMEDIATE), so two applies never interleave: the
// second waits for the first and then finds nothing left to do.
func Apply(ctx context.Context, db *sql.DB, set *Set, opts *Options) (*Plan, error) {
	if opts == nil {
		opts = &Options{}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Wait for another writer rather than failing at once, unless the
	// connection already says how long to wait.
	var busy int
	if r := conn.QueryRowContext(ctx, "PRAGMA busy_timeout"); r.Scan(&busy) == nil && busy == 0 {
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 30000"); err == nil {
			defer conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA busy_timeout = 0")
		}
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("starting the transaction: %w", err)
	}
	done := false
	defer func() {
		if !done {
			conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	p, err := buildPlan(ctx, conn, set, opts)
	if err != nil {
		return nil, err
	}
	if p.Blocked() {
		return p, ErrBlocked
	}
	if !p.Pending() {
		return p, nil // in step already; don't record an empty run
	}
	if err := createLedger(ctx, conn); err != nil {
		return nil, err
	}
	w := &writer{ctx: ctx, q: conn, opts: opts, plan: p, at: opts.now().UTC().Format(time.RFC3339)}
	if err := w.q.QueryRowContext(ctx, "SELECT coalesce(max(id), 0) + 1 FROM "+runsTable).Scan(&w.run); err != nil {
		return nil, err
	}
	if err := w.write(); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}
	done = true
	p.Applied, p.Run = true, w.run
	return p, nil
}

type writer struct {
	ctx  context.Context
	q    *sql.Conn
	opts *Options
	plan *Plan
	at   string
	run  int64
	seq  int
}

func (w *writer) write() error {
	tables := w.plan.tables
	// Deletes go first, so a new row can reuse a unique value an old one
	// gave up, except rows a surviving row points at right now: those wait
	// until that row has been pointed elsewhere (foreign keys may be on).
	late := w.plan.pointedAt()
	if err := w.deletes(late, false); err != nil {
		return err
	}
	for _, tw := range tables {
		// Renames first, so a new row can take a name an old one gave up;
		// then inserts; then everything else; and references from a table to
		// itself last, once every row they can point at exists.
		for _, phase := range []phase{phaseRename, phaseInsert, phaseUpdate, phaseSelf} {
			for _, rw := range tw.rows {
				if err := w.writeRow(tw, rw, phase); err != nil {
					return err
				}
			}
		}
	}
	if err := w.deletes(late, true); err != nil {
		return err
	}
	for _, tw := range tables {
		for _, rw := range tw.rows {
			if err := w.writeLedger(tw, rw); err != nil {
				return err
			}
		}
	}
	for _, tw := range tables {
		for _, c := range tw.t.Columns {
			if c.Ref != nil {
				if _, err := w.q.ExecContext(w.ctx, "INSERT INTO "+refsTable+" (tbl, col, ref) VALUES (?, ?, ?) ON CONFLICT (tbl, col) DO UPDATE SET ref = excluded.ref",
					tw.t.Name, c.Name, c.Ref.String()); err != nil {
					return err
				}
			}
		}
	}
	c := w.plan.Counts()
	_, err := w.q.ExecContext(w.ctx, "INSERT INTO "+runsTable+" (id, at, seed_hash, version, inserts, updates, deletes, retires, kept, resolved) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		w.run, w.at, w.plan.SeedHash, Version, c.Inserts, c.Updates+c.Renames+c.Unretires, c.Deletes, c.Retires, c.KeptEdits+c.KeptDeletes, c.Resolved)
	return err
}

// deletes runs the plan's deletes, children before the tables they
// reference: the ones in late, or the ones not in it.
func (w *writer) deletes(late map[*rowWork]bool, wantLate bool) error {
	tables := w.plan.tables
	for i := len(tables) - 1; i >= 0; i-- {
		tw := tables[i]
		var dels []*rowWork
		for _, rw := range tw.rows {
			if rw.del && late[rw] == wantLate {
				dels = append(dels, rw)
			}
		}
		dels, err := w.selfOrder(tw, dels)
		if err != nil {
			return err
		}
		for _, rw := range dels {
			if err := w.exec(rw, ActDelete, "DELETE FROM "+quoteIdent(tw.t.Name)+" WHERE "+w.whereKey(tw), rw.live.rawKey...); err != nil {
				return fmt.Errorf("deleting %s %s: %w", tw.t.Name, keyText(rw.keyVals), err)
			}
			if err := w.log(tw, rw, ActDelete, "", nil, nil, describe(tw.t, rw.live.nat)); err != nil {
				return err
			}
		}
	}
	return nil
}

// selfOrder orders a table's deletes so a row that refers to another row of
// the same table goes before it. Rows that refer to each other in a loop
// have those references cleared first; they are all being deleted anyway.
func (w *writer) selfOrder(tw *tableWork, dels []*rowWork) ([]*rowWork, error) {
	var cols []string
	for _, c := range tw.t.Columns {
		if tw.selfRef(c.Name) && !tw.t.isKey(c.Name) {
			cols = append(cols, c.Name)
		}
	}
	if len(cols) == 0 || len(dels) < 2 {
		return dels, nil
	}
	pointsAt := func(from, to *rowWork) bool {
		for _, col := range cols {
			v := from.live.nat[col]
			if !v.IsNull() && v.Equal(to.live.nat[tw.refs[col].ref.Column]) {
				return true
			}
		}
		return false
	}
	var out []*rowWork
	left := dels
	for len(left) > 0 {
		var next, rest []*rowWork
		for _, rw := range left {
			wanted := false
			for _, other := range left {
				if other != rw && pointsAt(other, rw) {
					wanted = true
					break
				}
			}
			if wanted {
				rest = append(rest, rw)
			} else {
				next = append(next, rw)
			}
		}
		if len(next) == 0 {
			// A loop: clear the references, then any order will do.
			sets := make([]string, len(cols))
			for i, col := range cols {
				sets[i] = quoteIdent(col) + " = NULL"
			}
			for _, rw := range rest {
				args := make([]any, len(rw.live.rawKey))
				for i, v := range rw.live.rawKey {
					args[i] = v.Any()
				}
				if _, err := w.q.ExecContext(w.ctx, "UPDATE "+quoteIdent(tw.t.Name)+" SET "+strings.Join(sets, ", ")+" WHERE "+w.whereKey(tw), args...); err != nil {
					return nil, fmt.Errorf("deleting %s %s: %w", tw.t.Name, keyText(rw.keyVals), err)
				}
			}
			return append(out, rest...), nil
		}
		out = append(out, next...)
		left = rest
	}
	return out, nil
}

// pointedAt is the deleted rows that must wait until the end: those a row
// that stays refers to before the apply, and, since they wait too, those a
// waiting row refers to.
func (p *Plan) pointedAt() map[*rowWork]bool {
	out := map[*rowWork]bool{}
	for grew := true; grew; {
		grew = false
		for _, target := range p.tables {
			for _, rw := range target.rows {
				if !rw.del || out[rw] {
					continue
				}
				for _, from := range p.tables {
					early := map[*liveRow]bool{} // rows gone before the writes
					for _, r := range from.rows {
						if r.del && r.live != nil && !out[r] && r != rw {
							early[r.live] = true
						}
					}
					for _, c := range from.t.Columns {
						rm := from.refs[c.Name]
						if rm == nil || rm.ref.Table != target.t.Name {
							continue
						}
						want := rw.live.nat[rm.ref.Column]
						for _, lr := range from.live {
							if lr != rw.live && !early[lr] && lr.nat[c.Name].Equal(want) {
								out[rw], grew = true, true
							}
						}
					}
				}
			}
		}
	}
	return out
}

func (w *writer) whereKey(tw *tableWork) string {
	parts := make([]string, len(tw.t.Key))
	for i, k := range tw.t.Key {
		parts[i] = quoteIdent(k) + " IS ?"
	}
	return strings.Join(parts, " AND ")
}

type phase int

const (
	phaseRename phase = iota
	phaseInsert
	phaseUpdate
	phaseSelf
)

// selfRef reports whether a column refers to its own table.
func (tw *tableWork) selfRef(col string) bool {
	rm := tw.refs[col]
	return rm != nil && rm.ref.Table == tw.t.Name
}

func (w *writer) writeRow(tw *tableWork, rw *rowWork, ph phase) error {
	t := tw.t
	if rw.del || rw.skipped {
		return nil
	}
	switch ph {
	case phaseRename:
		if rw.renamedFrom == nil {
			return nil
		}
		if err := w.update(tw, rw, ActRename, rw.live.rawKey, false); err != nil {
			return err
		}
		return w.log(tw, rw, ActRename, "", nil, nil, "from "+keyText(rw.renamedFrom.natKey(t)))

	case phaseInsert:
		if !rw.insert {
			return nil
		}
		var cols, marks []string
		var args []Value
		for _, c := range t.Columns {
			if tw.selfRef(c.Name) && !t.isKey(c.Name) {
				continue // filled in once every row exists
			}
			v, err := w.stored(tw, c.Name, rw.seed[c.Name])
			if err != nil {
				return err
			}
			cols, marks, args = append(cols, quoteIdent(c.Name)), append(marks, "?"), append(args, v)
		}
		q := "INSERT INTO " + quoteIdent(t.Name) + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
		if err := w.exec(rw, ActInsert, q, args...); err != nil {
			return fmt.Errorf("inserting %s %s: %w", t.Name, keyText(rw.keyVals), err)
		}
		return w.log(tw, rw, ActInsert, "", nil, nil, describe(t, rw.seed))

	case phaseUpdate:
		if rw.insert || rw.live == nil {
			return nil
		}
		where, err := w.currentKey(tw, rw)
		if err != nil {
			return err
		}
		if rw.renamedFrom == nil {
			if err := w.update(tw, rw, ActUpdate, where, false); err != nil {
				return err
			}
		}
		for _, i := range rw.changes {
			if ch := w.plan.Changes[i]; ch.Action == ActConflict && ch.Resolved == TakeLocal && ch.Column != "" {
				lv, sv := *ch.Live, *ch.Seed
				if err := w.log(tw, rw, ActConflict, ch.Column, &lv, &sv, "kept the local value"); err != nil {
					return err
				}
			}
		}
		if rw.unretire || rw.retire {
			v, act, note := NullValue, ActUnretire, ""
			if rw.retire {
				v, act, note = TextValue(w.at), ActRetire, w.at
			}
			q := "UPDATE " + quoteIdent(t.Name) + " SET " + quoteIdent(t.RetireColumn) + " = ? WHERE " + w.whereKey(tw)
			if err := w.exec(rw, act, q, append([]Value{v}, where...)...); err != nil {
				return fmt.Errorf("%s %s %s: %w", act, t.Name, keyText(rw.keyVals), err)
			}
			return w.log(tw, rw, act, t.RetireColumn, nil, nil, note)
		}
		return nil

	case phaseSelf:
		if !rw.insert && rw.live == nil {
			return nil
		}
		where, err := w.currentKey(tw, rw)
		if err != nil {
			return err
		}
		if rw.insert {
			// The inserted row's references to its own table, now that the
			// rows they point at exist.
			for _, c := range t.Columns {
				if tw.selfRef(c.Name) && !t.isKey(c.Name) && !rw.seed[c.Name].IsNull() {
					rw.set(c.Name, rw.seed[c.Name])
				}
			}
		}
		return w.update(tw, rw, ActUpdate, where, true)
	}
	return nil
}

// update writes the row's pending values in one UPDATE: the ordinary
// columns, or (self) only the columns that refer to the row's own table.
// Inserted rows log nothing here; their insert says it all.
func (w *writer) update(tw *tableWork, rw *rowWork, act Action, where []Value, self bool) error {
	t := tw.t
	var cols []string
	var args []Value
	var logged []*Column
	for _, c := range t.Columns { // column order keeps statements stable
		v, ok := rw.sets[c.Name]
		if !ok || (tw.selfRef(c.Name) && !t.isKey(c.Name)) != self {
			continue
		}
		sv, err := w.stored(tw, c.Name, v)
		if err != nil {
			return err
		}
		cols, args = append(cols, quoteIdent(c.Name)+" = ?"), append(args, sv)
		if !t.isKey(c.Name) {
			logged = append(logged, c)
		}
	}
	if len(cols) == 0 {
		return nil
	}
	q := "UPDATE " + quoteIdent(t.Name) + " SET " + strings.Join(cols, ", ") + " WHERE " + w.whereKey(tw)
	if err := w.exec(rw, act, q, append(args, where...)...); err != nil {
		return fmt.Errorf("updating %s %s: %w", t.Name, keyText(rw.keyVals), err)
	}
	if rw.insert {
		return nil
	}
	for _, c := range logged {
		old, v := rw.live.nat[c.Name], rw.sets[c.Name]
		note := ""
		for _, i := range rw.changes {
			if ch := w.plan.Changes[i]; ch.Column == c.Name && ch.Action == ActConflict {
				note = "conflict, took the seed's value"
			}
		}
		if err := w.log(tw, rw, ActUpdate, c.Name, &old, &v, note); err != nil {
			return err
		}
	}
	return nil
}

// currentKey is the row's key as stored right now: the seed's for a row
// just inserted or renamed, the live row's otherwise.
func (w *writer) currentKey(tw *tableWork, rw *rowWork) ([]Value, error) {
	if !rw.insert && rw.renamedFrom == nil {
		return rw.live.rawKey, nil
	}
	out := make([]Value, len(tw.t.Key))
	for i, k := range tw.t.Key {
		v, err := w.stored(tw, k, rw.keyVals[i])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// describe lists a row's non-key values for the change log.
func describe(t *Table, vals map[string]Value) string {
	var parts []string
	for _, c := range t.Columns {
		if !t.isKey(c.Name) {
			parts = append(parts, c.Name+" "+Display(vals[c.Name]))
		}
	}
	return strings.Join(parts, ", ")
}

// stored turns a natural value into what the column stores: a reference
// becomes the primary key of the row it names.
func (w *writer) stored(tw *tableWork, col string, v Value) (Value, error) {
	rm := tw.refs[col]
	if rm == nil || v.IsNull() {
		return v, nil
	}
	rows, err := w.q.QueryContext(w.ctx, "SELECT "+quoteIdent(rm.target.idCol)+" FROM "+quoteIdent(rm.ref.Table)+" WHERE "+quoteIdent(rm.ref.Column)+" = ? LIMIT 2", v.Any())
	if err != nil {
		return NullValue, err
	}
	defer rows.Close()
	var ids []Value
	for rows.Next() {
		var x any
		if err := rows.Scan(&x); err != nil {
			return NullValue, err
		}
		ids = append(ids, fromDB(x))
	}
	if err := rows.Err(); err != nil {
		return NullValue, err
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return NullValue, fmt.Errorf("%s.%s: no %s row has %s = %s", tw.t.Name, col, rm.ref.Table, rm.ref.Column, Display(v))
	}
	return NullValue, fmt.Errorf("%s.%s: more than one %s row has %s = %s", tw.t.Name, col, rm.ref.Table, rm.ref.Column, Display(v))
}

func (w *writer) exec(rw *rowWork, act Action, q string, args ...Value) error {
	xs := make([]any, len(args))
	for i, a := range args {
		xs[i] = a.Any()
	}
	if _, err := w.q.ExecContext(w.ctx, q, xs...); err != nil {
		return err
	}
	if w.opts.Progress != nil {
		ch := Change{Action: act}
		for _, i := range rw.changes {
			if w.plan.Changes[i].Action == act {
				ch = w.plan.Changes[i]
				break
			}
		}
		if len(rw.changes) > 0 && ch.Table == "" {
			ch.Table, ch.Key = w.plan.Changes[rw.changes[0]].Table, rw.keyVals
		}
		w.opts.Progress(ch)
	}
	return nil
}

func (w *writer) log(tw *tableWork, rw *rowWork, act Action, col string, old, new *Value, note string) error {
	w.seq++
	var o, n any
	if old != nil {
		o = old.encode()
	}
	if new != nil {
		n = new.encode()
	}
	_, err := w.q.ExecContext(w.ctx, "INSERT INTO "+changesTable+" (run, seq, tbl, key, action, col, old, new, note) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		w.run, w.seq, tw.t.Name, rw.key, string(act), col, o, n, note)
	return err
}

func (w *writer) writeLedger(tw *tableWork, rw *rowWork) error {
	if (rw.oldKey != "" || rw.base != nil && rw.base.moved) && rw.ledgerOp == ledgerNone && rw.base != nil {
		// Only the key moved (a parent was renamed): carry the entry over.
		rw.ledgerOp, rw.ledgerVals, rw.ledgerRet = ledgerUpsert, rw.base.vals, rw.base.retired
	}
	if rw.oldKey != "" {
		if _, err := w.q.ExecContext(w.ctx, "DELETE FROM "+rowsTable+" WHERE tbl = ? AND key = ?", tw.t.Name, rw.oldKey); err != nil {
			return err
		}
	}
	switch rw.ledgerOp {
	case ledgerUpsert:
		ret := 0
		if rw.ledgerRet {
			ret = 1
		}
		_, err := w.q.ExecContext(w.ctx, "INSERT INTO "+rowsTable+" (tbl, key, vals, retired, run) VALUES (?, ?, ?, ?, ?) ON CONFLICT (tbl, key) DO UPDATE SET vals = excluded.vals, retired = excluded.retired, run = excluded.run",
			tw.t.Name, rw.key, encodeVals(rw.ledgerVals), ret, w.run)
		if err != nil {
			return err
		}
		for _, i := range rw.changes {
			if w.plan.Changes[i].Action == ActRecord && rw.base == nil {
				return w.log(tw, rw, ActRecord, "", nil, nil, "adopted: already matched the seed")
			}
		}
	case ledgerRemove:
		if _, err := w.q.ExecContext(w.ctx, "DELETE FROM "+rowsTable+" WHERE tbl = ? AND key = ?", tw.t.Name, rw.key); err != nil {
			return err
		}
		for _, i := range rw.changes {
			if w.plan.Changes[i].Action == ActRelease {
				return w.log(tw, rw, ActRelease, "", nil, nil, "dropped from the seed and left as it is")
			}
		}
	}
	return nil
}

// RowReport is everything Explain knows about one row.
type RowReport struct {
	Table   string         `json:"table"`
	Key     string         `json:"key"`
	InSeed  bool           `json:"in_seed"`
	InTable bool           `json:"in_table"`
	Managed bool           `json:"managed"` // the ledger knows it
	Retired bool           `json:"retired"`
	Fields  []FieldState   `json:"fields"`
	Changes []Change       `json:"changes"`
	History []LoggedChange `json:"history"`
}

// Explain reports one row: each managed value as the last apply left it,
// as the seed has it now and as the table has it, what an apply would do,
// and every change the ledger has logged for it. key is the natural key as
// the plan prints it, such as "editor" or "editor, posts.publish".
func Explain(ctx context.Context, db *sql.DB, set *Set, table, key string, opts *Options) (*RowReport, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	if set.Table(table) == nil {
		return nil, fmt.Errorf("no seed file covers table %q", table)
	}
	p, err := buildPlan(ctx, conn, set, opts)
	if err != nil {
		return nil, err
	}
	for _, tw := range p.tables {
		if tw.t.Name != table {
			continue
		}
		var match *rowWork
		for _, rw := range tw.rows {
			if normKey(keyText(rw.keyVals)) == normKey(key) {
				match = rw
			}
		}
		rep := &RowReport{Table: table, Key: key}
		if match == nil {
			// Not managed; it may still be a live row.
			for _, lr := range tw.live {
				if normKey(keyText(lr.natKey(tw.t))) == normKey(key) {
					rep.InTable = true
				}
			}
			return rep, nil
		}
		rep.Key = keyText(match.keyVals)
		rep.InSeed, rep.InTable, rep.Managed = match.seed != nil, match.live != nil, match.base != nil
		rep.Retired = match.base != nil && match.base.retired
		rep.Fields = match.fields
		for _, i := range match.changes {
			rep.Changes = append(rep.Changes, p.Changes[i])
		}
		if ok, err := ledgerExists(ctx, conn); err != nil {
			return nil, err
		} else if ok {
			if rep.History, err = readChangeLog(ctx, conn, table, match.key); err != nil {
				return nil, err
			}
		}
		return rep, nil
	}
	return nil, fmt.Errorf("no seed file covers table %q", table)
}
