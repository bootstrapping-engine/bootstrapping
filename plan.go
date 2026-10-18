// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Action is what a plan does, or decides not to do, for one row or one value.
type Action string

const (
	ActInsert     Action = "insert"      // a seed row that isn't in the table yet
	ActUpdate     Action = "update"      // a value changed in the seed, untouched locally
	ActDelete     Action = "delete"      // dropped from the seed, on_remove = "delete"
	ActRetire     Action = "retire"      // dropped from the seed, on_remove = { retire = ... }
	ActUnretire   Action = "unretire"    // back in the seed after being retired
	ActRename     Action = "rename"      // the seed row's natural key changed (_renamed_from)
	ActKeepEdit   Action = "keep-edit"   // edited locally, unchanged in the seed: the edit stays
	ActKeepDelete Action = "keep-delete" // deleted locally: it stays deleted
	ActConflict   Action = "conflict"    // changed on both sides, differently
	ActRelease    Action = "release"     // dropped from the seed, on_remove = "keep": left alone
	ActSkip       Action = "skip"        // can't be written now: a referenced row is missing
	ActRecord     Action = "record"      // already matches; the ledger records it
)

// Conflict kinds.
const (
	BothChanged   = "both-changed"   // the seed and the live row changed the same value differently
	ExistsAlready = "exists"         // a seed row already exists, with a different value, and was never applied
	RemoveEdited  = "removed-edited" // dropped from the seed with on_remove = "delete", but edited locally
)

// Change is one line of a plan.
type Change struct {
	Table    string  `json:"table"`
	Key      []Value `json:"-"`
	Action   Action  `json:"action"`
	Column   string  `json:"column,omitempty"`
	Base     *Value  `json:"-"` // the last-applied seed value, if any
	Seed     *Value  `json:"-"` // the new seed value
	Live     *Value  `json:"-"` // the value in the table now
	Conflict string  `json:"conflict,omitempty"`
	Resolved Take    `json:"resolved,omitempty"` // how a conflict is settled; empty while it blocks the apply
	Reason   string  `json:"reason,omitempty"`

	key string
}

// KeyText is the row's natural key as text: "editor", or "editor, posts.publish".
func (c Change) KeyText() string { return keyText(c.Key) }

// Blocking reports whether the change is a conflict nobody has settled.
func (c Change) Blocking() bool { return c.Action == ActConflict && c.Resolved == "" }

// Writes reports whether applying the change writes table data.
func (c Change) Writes() bool {
	switch c.Action {
	case ActInsert, ActUpdate, ActDelete, ActRetire, ActUnretire, ActRename:
		return true
	case ActConflict:
		return c.Resolved == TakeSeed
	}
	return false
}

func keyText(vals []Value) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = plainText(v)
	}
	return strings.Join(parts, ", ")
}

// plainText is a value without quotes, for keys.
func plainText(v Value) string {
	if v.Kind == Text {
		if strings.HasPrefix(v.S, missingPrefix) {
			return "(missing row " + strings.TrimPrefix(v.S, missingPrefix) + ")"
		}
		return v.S
	}
	return v.String()
}

// Display is a value as the plan prints it.
func Display(v Value) string {
	if v.Kind == Text && strings.HasPrefix(v.S, missingPrefix) {
		return "(missing row " + strings.TrimPrefix(v.S, missingPrefix) + ")"
	}
	return v.String()
}

// missingPrefix marks a reference whose row doesn't exist, so it compares
// unequal to every real value.
const missingPrefix = "\x00missing:"

// Counts sums a plan by kind.
type Counts struct {
	Inserts     int `json:"inserts"`
	Updates     int `json:"updates"`
	Deletes     int `json:"deletes"`
	Retires     int `json:"retires"`
	Unretires   int `json:"unretires"`
	Renames     int `json:"renames"`
	KeptEdits   int `json:"kept_edits"`
	KeptDeletes int `json:"kept_deletes"`
	Conflicts   int `json:"conflicts"`
	Resolved    int `json:"resolved"`
	Released    int `json:"released"`
	Skipped     int `json:"skipped"`
	Recorded    int `json:"recorded"`
}

// Plan is what Preview found or Apply did.
type Plan struct {
	Changes  []Change
	SeedHash string
	Applied  bool  // true once Apply has committed it
	Run      int64 // the ledger's run number, once applied

	tables []*tableWork
}

// Counts sums the plan's changes.
func (p *Plan) Counts() Counts {
	var c Counts
	for _, ch := range p.Changes {
		switch ch.Action {
		case ActInsert:
			c.Inserts++
		case ActUpdate:
			c.Updates++
		case ActDelete:
			c.Deletes++
		case ActRetire:
			c.Retires++
		case ActUnretire:
			c.Unretires++
		case ActRename:
			c.Renames++
		case ActKeepEdit:
			c.KeptEdits++
		case ActKeepDelete:
			c.KeptDeletes++
		case ActConflict:
			c.Conflicts++
			if ch.Resolved != "" {
				c.Resolved++
			}
		case ActRelease:
			c.Released++
		case ActSkip:
			c.Skipped++
		case ActRecord:
			c.Recorded++
		}
	}
	return c
}

// Blocked reports whether a conflict nobody has settled stops the apply.
func (p *Plan) Blocked() bool {
	for _, c := range p.Changes {
		if c.Blocking() {
			return true
		}
	}
	return false
}

// Writes counts the changes that write table data.
func (p *Plan) Writes() int {
	n := 0
	for _, c := range p.Changes {
		if c.Writes() {
			n++
		}
	}
	return n
}

// Pending reports whether an apply would write anything: table data, or
// the ledger catching up with rows that already match.
func (p *Plan) Pending() bool {
	for _, t := range p.tables {
		for _, r := range t.rows {
			if r.ledgerOp != ledgerNone {
				return true
			}
		}
	}
	return p.Writes() > 0
}

// Resolution settles conflicts in an apply. Table, Key and Column narrow it
// down; empty means any. Key is the natural key as KeyText prints it.
type Resolution struct {
	Table  string
	Key    string
	Column string
	Take   Take
}

func (r Resolution) matches(table, key, column string) bool {
	return (r.Table == "" || r.Table == table) &&
		(r.Key == "" || normKey(r.Key) == normKey(key)) &&
		(r.Column == "" || r.Column == column)
}

func normKey(s string) string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ",")
}

// ---- working state ----

type liveRow struct {
	rawKey  []Value          // key columns as stored, for WHERE clauses
	nat     map[string]Value // every managed column, references as natural keys
	retired Value            // the retire column, if the table has one
}

type ledgerOp int

const (
	ledgerNone ledgerOp = iota
	ledgerUpsert
	ledgerRemove
)

// FieldState is one value of one row, as Explain reports it.
type FieldState struct {
	Column  string `json:"column"`
	Own     Own    `json:"own"`
	Base    *Value `json:"-"`
	Seed    *Value `json:"-"`
	Live    *Value `json:"-"`
	Verdict string `json:"verdict"`
}

type rowWork struct {
	key     string
	keyVals []Value
	seed    map[string]Value // nil when the row isn't in the seed
	live    *liveRow
	base    *ledgerRow

	renamedFrom *liveRow // set when the live row is found under its old key
	oldKey      string   // the ledger key it had before the rename

	insert, del, retire, unretire bool
	sets                          map[string]Value // natural values to write, by column

	ledgerOp   ledgerOp
	ledgerVals map[string]Value
	ledgerRet  bool

	changes []int // indexes into Plan.Changes
	fields  []FieldState
	skipped bool
}

type tableWork struct {
	t      *Table
	info   *tableInfo
	aff    map[string]Affinity // the affinity a column's natural value takes
	refs   map[string]*refMap  // by column
	rows   []*rowWork
	byKey  map[string]*rowWork
	live   map[string]*liveRow // every live row, managed or not
	after  map[string]bool     // natural keys present after the apply, for references into this table
	keyIdx map[string]int
}

// refMap translates a reference column between stored ids and the natural
// value seeds use.
type refMap struct {
	ref     *Ref
	target  *tableInfo
	toNat   map[string]Value // encoded id -> natural value
	present map[string]bool  // encoded natural values that exist now
}

// Preview works out what applying the seed set would do, without writing.
func Preview(ctx context.Context, db *sql.DB, set *Set, opts *Options) (*Plan, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	return buildPlan(ctx, conn, set, opts)
}

func buildPlan(ctx context.Context, q querier, set *Set, opts *Options) (*Plan, error) {
	if opts == nil {
		opts = &Options{}
	}
	hasLedger, err := ledgerExists(ctx, q)
	if err != nil {
		return nil, err
	}
	p := &Plan{SeedHash: set.Hash}
	works := map[string]*tableWork{}
	for _, t := range set.Tables {
		tw, err := loadTable(ctx, q, t, hasLedger, works)
		if err != nil {
			return nil, err
		}
		works[t.Name] = tw
		p.tables = append(p.tables, tw)
		tw.plan(p, opts)
	}
	return p, nil
}

func loadTable(ctx context.Context, q querier, t *Table, hasLedger bool, works map[string]*tableWork) (*tableWork, error) {
	info, err := readTableInfo(ctx, q, t.Name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.File, err)
	}
	tw := &tableWork{t: t, info: info, aff: map[string]Affinity{}, refs: map[string]*refMap{}, byKey: map[string]*rowWork{}, keyIdx: map[string]int{}}
	for i, k := range t.Key {
		tw.keyIdx[k] = i
	}
	for _, c := range t.Columns {
		ci, ok := info.cols[c.Name]
		if !ok {
			return nil, fmt.Errorf("%s: table %q has no column %q", t.File, t.Name, c.Name)
		}
		tw.aff[c.Name] = ci.aff
		if c.Ref != nil {
			rm, err := loadRef(ctx, q, c.Ref, works)
			if err != nil {
				return nil, fmt.Errorf("%s: column %q: %w", t.File, c.Name, err)
			}
			tw.refs[c.Name] = rm
			tw.aff[c.Name] = rm.target.cols[c.Ref.Column].aff
		}
	}
	if t.RetireColumn != "" {
		if _, ok := info.cols[t.RetireColumn]; !ok {
			return nil, fmt.Errorf("%s: table %q has no column %q to retire rows with", t.File, t.Name, t.RetireColumn)
		}
	}
	for name, ci := range info.cols {
		if ci.notNull && !ci.hasDflt && ci.pk == 0 && t.Column(name) == nil && name != t.RetireColumn {
			return nil, fmt.Errorf("%s: column %q of %q is NOT NULL with no default, so the seed must set it", t.File, name, t.Name)
		}
	}

	// The seed rows, converted the way SQLite would store them.
	seedRows := map[string]*rowWork{}
	var order []string
	renames := map[string][]Value{}
	for _, sr := range t.Rows {
		vals := map[string]Value{}
		for _, c := range t.Columns {
			x, ok := sr.Values[c.Name]
			if !ok {
				vals[c.Name] = NullValue
				continue
			}
			v, err := canonical(ctx, q, x, tw.aff[c.Name])
			if err != nil {
				return nil, fmt.Errorf("%s: row %d, column %q: %w", t.File, sr.Index, c.Name, err)
			}
			vals[c.Name] = v
		}
		kv := make([]Value, len(t.Key))
		for i, k := range t.Key {
			kv[i] = vals[k]
			if kv[i].IsNull() {
				return nil, fmt.Errorf("%s: row %d: key column %q is empty", t.File, sr.Index, k)
			}
		}
		key := encodeKey(kv)
		if _, dup := seedRows[key]; dup {
			return nil, fmt.Errorf("%s: row %d repeats the key %s", t.File, sr.Index, keyText(kv))
		}
		seedRows[key] = &rowWork{key: key, keyVals: kv, seed: vals}
		order = append(order, key)
		if sr.RenamedFrom != nil {
			old := make([]Value, len(t.Key))
			for i, k := range t.Key {
				v, err := canonical(ctx, q, sr.RenamedFrom[i], tw.aff[k])
				if err != nil {
					return nil, fmt.Errorf("%s: row %d, _renamed_from: %w", t.File, sr.Index, err)
				}
				old[i] = v
			}
			renames[key] = old
		}
	}

	// A table referring to itself by its key can point at rows its own seed
	// adds in this apply.
	for _, c := range t.Columns {
		if rm := tw.refs[c.Name]; rm != nil && rm.ref.Table == t.Name && len(t.Key) == 1 && t.Key[0] == rm.ref.Column {
			for _, key := range order {
				rm.present[key] = true
			}
		}
	}

	ledger := map[string]*ledgerRow{}
	if hasLedger {
		var err error
		if ledger, err = readLedger(ctx, q, t.Name); err != nil {
			return nil, err
		}
	}

	// The live rows, with references turned into natural keys. Columns the
	// ledger remembers but the seed no longer names are read too, as stored,
	// so a dropped row's local edits are still seen.
	cols := make([]string, 0, len(t.Columns)+1)
	for _, c := range t.Columns {
		cols = append(cols, c.Name)
	}
	extra := map[string]bool{}
	for _, lr := range ledger {
		for col := range lr.vals {
			if _, exists := info.cols[col]; exists && t.Column(col) == nil && col != t.RetireColumn {
				extra[col] = true
			}
		}
	}
	var extras []string
	for col := range extra {
		extras = append(extras, col)
	}
	sort.Strings(extras)
	cols = append(cols, extras...)
	if t.RetireColumn != "" {
		cols = append(cols, t.RetireColumn)
	}
	raw, err := readRows(ctx, q, t.Name, cols)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", t.Name, err)
	}
	live := map[string]*liveRow{}
	for _, r := range raw {
		lr := &liveRow{nat: map[string]Value{}}
		for i, c := range t.Columns {
			v := r[i]
			if rm := tw.refs[c.Name]; rm != nil && !v.IsNull() {
				if n, ok := rm.toNat[encodeKey([]Value{v})]; ok {
					v = n
				} else {
					v = TextValue(missingPrefix + plainText(v))
				}
			}
			lr.nat[c.Name] = v
		}
		for i, col := range extras {
			lr.nat[col] = r[len(t.Columns)+i]
		}
		if t.RetireColumn != "" {
			lr.retired = r[len(r)-1]
		}
		kv := make([]Value, len(t.Key))
		lr.rawKey = make([]Value, len(t.Key))
		for i, k := range t.Key {
			kv[i] = lr.nat[k]
			lr.rawKey[i] = r[i]
		}
		key := encodeKey(kv)
		if _, dup := live[key]; dup {
			return nil, fmt.Errorf("table %q has more than one row with the key %s; the key must identify one row", t.Name, keyText(kv))
		}
		live[key] = lr
	}
	tw.live = live

	// Renames move a row's identity before anything else is compared.
	consumed := map[string]bool{}
	for _, key := range order {
		rw := seedRows[key]
		old, ok := renames[key]
		if !ok {
			continue
		}
		oldKey := encodeKey(old)
		if live[key] != nil || ledger[key] != nil || ledger[oldKey] == nil || seedRows[oldKey] != nil || consumed[oldKey] {
			continue // already renamed, or nothing to rename
		}
		consumed[oldKey] = true
		rw.base, rw.oldKey = ledger[oldKey], oldKey
		if lr := live[oldKey]; lr != nil {
			rw.live, rw.renamedFrom = lr, lr
		}
	}

	// A table referring to itself sees a renamed row by its new name.
	for _, c := range t.Columns {
		rm := tw.refs[c.Name]
		if rm == nil || rm.ref.Table != t.Name || len(t.Key) != 1 || t.Key[0] != rm.ref.Column || t.isKey(c.Name) {
			continue
		}
		renamed := map[string]Value{}
		for _, key := range order {
			if rw := seedRows[key]; rw.renamedFrom != nil {
				renamed[encodeKey(rw.renamedFrom.natKey(t))] = rw.keyVals[0]
			}
		}
		for _, lr := range live {
			if n, ok := renamed[encodeKey([]Value{lr.nat[c.Name]})]; ok {
				lr.nat[c.Name] = n
			}
		}
	}

	add := func(rw *rowWork) {
		tw.rows = append(tw.rows, rw)
		tw.byKey[rw.key] = rw
	}
	for _, key := range order {
		rw := seedRows[key]
		if rw.oldKey == "" {
			rw.live, rw.base = live[key], ledger[key]
		}
		add(rw)
	}
	var rest []string
	for key := range ledger {
		if seedRows[key] == nil && !consumed[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		add(&rowWork{key: key, keyVals: ledger[key].keyVals, live: live[key], base: ledger[key]})
	}
	return tw, nil
}

func loadRef(ctx context.Context, q querier, ref *Ref, works map[string]*tableWork) (*refMap, error) {
	target, err := readTableInfo(ctx, q, ref.Table)
	if err != nil {
		return nil, fmt.Errorf("reference %s: %w", ref, err)
	}
	if _, ok := target.cols[ref.Column]; !ok {
		return nil, fmt.Errorf("reference %s: table %q has no column %q", ref, ref.Table, ref.Column)
	}
	if target.idCol == "" {
		return nil, fmt.Errorf("reference %s: %q has no single primary key or rowid to point at", ref, ref.Table)
	}
	rows, err := readRows(ctx, q, ref.Table, []string{target.idCol, ref.Column})
	if err != nil {
		return nil, fmt.Errorf("reference %s: %w", ref, err)
	}
	rm := &refMap{ref: ref, target: target, toNat: map[string]Value{}, present: map[string]bool{}}
	count := map[string]int{}
	for _, r := range rows {
		rm.toNat[encodeKey([]Value{r[0]})] = r[1]
		if !r[1].IsNull() {
			k := encodeKey([]Value{r[1]})
			rm.present[k] = true
			count[k]++
		}
	}
	for k, n := range count {
		if n > 1 {
			v, _ := decodeKey(k)
			return nil, fmt.Errorf("reference %s: %d rows of %q have %s = %s, so it can't tell them apart", ref, n, ref.Table, ref.Column, keyText(v))
		}
	}
	// A reference into a seed table by its key sees that table as it will be
	// after this apply: rows it inserts count, rows it deletes don't, and a
	// renamed row answers to its new name.
	if tw := works[ref.Table]; tw != nil && len(tw.t.Key) == 1 && tw.t.Key[0] == ref.Column {
		rm.present = tw.after
		renamed := map[string]Value{}
		for _, rw := range tw.rows {
			if rw.renamedFrom != nil {
				renamed[encodeKey(rw.renamedFrom.natKey(tw.t))] = rw.keyVals[0]
			}
		}
		for id, nat := range rm.toNat {
			if n, ok := renamed[encodeKey([]Value{nat})]; ok {
				rm.toNat[id] = n
			}
		}
	}
	return rm, nil
}

// ---- the merge ----

func (tw *tableWork) plan(p *Plan, opts *Options) {
	t := tw.t
	for _, rw := range tw.rows {
		if rw.renamedFrom != nil {
			tw.planRename(p, rw)
		}
		switch {
		case rw.seed != nil && rw.live != nil:
			tw.planBoth(p, opts, rw)
		case rw.seed != nil && rw.base != nil:
			// In the seed and in the ledger, gone from the table: deleted locally.
			if t.OnLocalDelete == "restore" {
				tw.planInsert(p, rw, "deleted locally; on_local_delete = \"restore\" puts it back")
			} else {
				tw.change(p, rw, Change{Action: ActKeepDelete, Reason: "deleted locally; it stays deleted"})
				rw.upsertLedger(rw.seed, false)
				for _, c := range t.Columns {
					if !t.isKey(c.Name) {
						rw.field(c, rw.baseVal(c.Name), rw.seedVal(c.Name), nil, "deleted locally")
					}
				}
			}
		case rw.seed != nil:
			tw.planInsert(p, rw, "")
		case rw.live != nil:
			tw.planRemoved(p, opts, rw)
		default:
			// Gone from the seed and from the table: just forget it.
			rw.ledgerOp = ledgerRemove
		}
	}
	tw.recordLines(p)
	tw.computeAfter()
}

func (tw *tableWork) planRename(p *Plan, rw *rowWork) {
	from := keyText(rw.renamedFrom.natKey(tw.t))
	tw.change(p, rw, Change{Action: ActRename, Reason: "renamed from " + from})
	for _, k := range tw.t.Key {
		rw.set(k, rw.seed[k])
	}
}

func (lr *liveRow) natKey(t *Table) []Value {
	kv := make([]Value, len(t.Key))
	for i, k := range t.Key {
		kv[i] = lr.nat[k]
	}
	return kv
}

func (tw *tableWork) planInsert(p *Plan, rw *rowWork, reason string) {
	if c, missing := tw.unresolved(rw.seed, nil); missing {
		tw.change(p, rw, Change{Action: ActSkip, Column: c, Reason: fmt.Sprintf("%s refers to %s %s, which doesn't exist", c, tw.refs[c].ref, Display(rw.seed[c]))})
		rw.skipped = true
		return
	}
	rw.insert = true
	rw.upsertLedger(rw.seed, false)
	tw.change(p, rw, Change{Action: ActInsert, Reason: reason})
	for _, c := range tw.t.Columns {
		if !tw.t.isKey(c.Name) {
			rw.field(c, rw.baseVal(c.Name), rw.seedVal(c.Name), nil, "insert")
		}
	}
}

// unresolved reports the first reference column among vals (or only cols,
// if given) whose target row won't exist after the apply.
func (tw *tableWork) unresolved(vals map[string]Value, cols []string) (string, bool) {
	check := func(c string) bool {
		rm := tw.refs[c]
		v := vals[c]
		return rm != nil && !v.IsNull() && !rm.present[encodeKey([]Value{v})]
	}
	if cols != nil {
		for _, c := range cols {
			if check(c) {
				return c, true
			}
		}
		return "", false
	}
	for _, c := range tw.t.Columns {
		if check(c.Name) {
			return c.Name, true
		}
	}
	return "", false
}

func (tw *tableWork) planBoth(p *Plan, opts *Options, rw *rowWork) {
	t := tw.t
	ledgerVals := map[string]Value{}
	for k, v := range rw.seed {
		ledgerVals[k] = v
	}
	if rw.base != nil && rw.base.retired && t.RetireColumn != "" && !rw.live.retired.IsNull() {
		rw.unretire = true
		tw.change(p, rw, Change{Action: ActUnretire, Reason: "back in the seed"})
	}
	for _, c := range t.Columns {
		if t.isKey(c.Name) {
			continue
		}
		s, l := rw.seed[c.Name], rw.live.nat[c.Name]
		b := rw.baseVal(c.Name) // nil when the row was never applied
		sp, lp := s, l
		switch {
		case l.Equal(s):
			rw.field(c, b, &sp, &lp, "in sync")
		case c.Own == OwnLocal:
			rw.field(c, b, &sp, &lp, "owned locally; the seed only sets it on insert")
		case c.Own == OwnSeed:
			if tw.writeField(p, rw, c, Change{Action: ActUpdate, Column: c.Name, Base: b, Seed: &sp, Live: &lp, Reason: "owned by the seed"}, ledgerVals) {
				rw.field(c, b, &sp, &lp, "update: owned by the seed")
			}
		case b == nil:
			// Never applied before, and the table already has a different value.
			tw.conflict(p, opts, rw, c, Change{Action: ActConflict, Column: c.Name, Seed: &sp, Live: &lp, Conflict: ExistsAlready,
				Reason: "the row was already there with a different value"}, ledgerVals)
		case s.Equal(*b):
			tw.change(p, rw, Change{Action: ActKeepEdit, Column: c.Name, Base: b, Seed: &sp, Live: &lp, Reason: "edited locally; the seed didn't change it"})
			rw.field(c, b, &sp, &lp, "kept local edit")
		case l.Equal(*b):
			if tw.writeField(p, rw, c, Change{Action: ActUpdate, Column: c.Name, Base: b, Seed: &sp, Live: &lp}, ledgerVals) {
				rw.field(c, b, &sp, &lp, "update: changed in the seed")
			}
		default:
			tw.conflict(p, opts, rw, c, Change{Action: ActConflict, Column: c.Name, Base: b, Seed: &sp, Live: &lp, Conflict: BothChanged,
				Reason: "changed in the seed and locally"}, ledgerVals)
		}
	}
	rw.upsertLedger(ledgerVals, false)
}

// writeField adds an update, unless the value refers to a row that won't
// exist, in which case it is skipped and the ledger keeps the old base so
// the update is tried again next time.
func (tw *tableWork) writeField(p *Plan, rw *rowWork, c *Column, ch Change, ledgerVals map[string]Value) bool {
	if col, missing := tw.unresolved(map[string]Value{c.Name: *ch.Seed}, []string{c.Name}); missing {
		tw.change(p, rw, Change{Action: ActSkip, Column: col, Reason: fmt.Sprintf("%s refers to %s %s, which doesn't exist", col, tw.refs[col].ref, Display(*ch.Seed))})
		tw.keepBase(rw, c.Name, ledgerVals)
		rw.field(c, ch.Base, ch.Seed, ch.Live, "skipped: missing reference")
		return false
	}
	tw.change(p, rw, ch)
	rw.set(c.Name, *ch.Seed)
	return true
}

func (tw *tableWork) keepBase(rw *rowWork, col string, ledgerVals map[string]Value) {
	if rw.base != nil {
		if bv, ok := rw.base.vals[col]; ok {
			ledgerVals[col] = bv
			return
		}
	}
	delete(ledgerVals, col)
}

func (tw *tableWork) conflict(p *Plan, opts *Options, rw *rowWork, c *Column, ch Change, ledgerVals map[string]Value) {
	take := opts.resolve(tw.t.Name, keyText(rw.keyVals), c.Name, tw.t.conflictPolicy(c))
	if take != TakeFail {
		ch.Resolved = take
	}
	switch take {
	case TakeSeed:
		if col, missing := tw.unresolved(map[string]Value{c.Name: *ch.Seed}, []string{c.Name}); missing {
			ch.Resolved = ""
			ch.Reason += fmt.Sprintf("; the seed's value refers to %s %s, which doesn't exist", tw.refs[col].ref, Display(*ch.Seed))
			tw.change(p, rw, ch)
			tw.keepBase(rw, c.Name, ledgerVals)
			rw.field(c, ch.Base, ch.Seed, ch.Live, "conflict")
			return
		}
		rw.set(c.Name, *ch.Seed)
		rw.field(c, ch.Base, ch.Seed, ch.Live, "conflict, settled: the seed's value")
	case TakeLocal:
		rw.field(c, ch.Base, ch.Seed, ch.Live, "conflict, settled: the local value")
	default:
		tw.keepBase(rw, c.Name, ledgerVals)
		rw.field(c, ch.Base, ch.Seed, ch.Live, "conflict")
	}
	tw.change(p, rw, ch)
}

func (tw *tableWork) planRemoved(p *Plan, opts *Options, rw *rowWork) {
	t := tw.t
	for _, c := range t.Columns {
		if !t.isKey(c.Name) {
			lv := rw.live.nat[c.Name]
			rw.field(c, rw.baseVal(c.Name), nil, &lv, "dropped from the seed")
		}
	}
	if rw.base.retired {
		return // retired already; nothing more to do
	}
	switch t.OnRemove {
	case RemoveKeep:
		tw.change(p, rw, Change{Action: ActRelease, Reason: "dropped from the seed; on_remove = \"keep\" leaves the row as it is"})
		rw.ledgerOp = ledgerRemove
	case RemoveRetire:
		if rw.live.retired.IsNull() {
			rw.retire = true
			tw.change(p, rw, Change{Action: ActRetire, Reason: "dropped from the seed"})
		}
		rw.upsertLedger(rw.base.vals, true)
	case RemoveDelete:
		// Edited means a merge column differs from what was last applied,
		// including columns the seed stopped naming when it dropped the row.
		var edited []string
		var cols []string
		for col := range rw.base.vals {
			cols = append(cols, col)
		}
		sort.Strings(cols)
		for _, col := range cols {
			if t.isKey(col) {
				continue
			}
			if c := t.Column(col); c != nil && c.Own != OwnMerge {
				continue
			}
			lv, ok := rw.live.nat[col]
			if !ok {
				continue // the column is gone from the table
			}
			if !lv.Equal(rw.base.vals[col]) {
				edited = append(edited, col)
			}
		}
		if len(edited) == 0 {
			rw.del = true
			rw.ledgerOp = ledgerRemove
			tw.change(p, rw, Change{Action: ActDelete, Reason: "dropped from the seed"})
			return
		}
		take := opts.resolve(t.Name, keyText(rw.keyVals), "", t.OnConflict)
		ch := Change{Action: ActConflict, Conflict: RemoveEdited,
			Reason: "dropped from the seed, but edited locally (" + strings.Join(edited, ", ") + ")"}
		switch take {
		case TakeSeed:
			ch.Resolved = TakeSeed
			rw.del = true
			rw.ledgerOp = ledgerRemove
		case TakeLocal:
			ch.Resolved = TakeLocal
			rw.ledgerOp = ledgerRemove
		}
		tw.change(p, rw, ch)
	}
}

// computeAfter records which natural keys exist once the apply is done,
// for tables that reference this one.
func (tw *tableWork) computeAfter() {
	after := map[string]bool{}
	for key := range tw.live { // every live row, not only managed ones
		after[key] = true
	}
	tw.after = after
	if len(tw.t.Key) != 1 {
		return
	}
	for _, rw := range tw.rows {
		switch {
		case rw.del:
			delete(after, encodeKey(rw.live.natKey(tw.t)))
		case rw.insert:
			after[rw.key] = true
		case rw.renamedFrom != nil:
			delete(after, encodeKey(rw.renamedFrom.natKey(tw.t)))
			after[rw.key] = true
		}
	}
}

func (tw *tableWork) change(p *Plan, rw *rowWork, ch Change) {
	ch.Table, ch.Key, ch.key = tw.t.Name, rw.keyVals, rw.key
	rw.changes = append(rw.changes, len(p.Changes))
	p.Changes = append(p.Changes, ch)
}

func (rw *rowWork) set(col string, v Value) {
	if rw.sets == nil {
		rw.sets = map[string]Value{}
	}
	rw.sets[col] = v
}

func (rw *rowWork) baseVal(col string) *Value {
	if rw.base == nil {
		return nil
	}
	v, ok := rw.base.vals[col]
	if !ok {
		v = NullValue // a column the seed didn't manage when it was last applied
	}
	return &v
}

func (rw *rowWork) seedVal(col string) *Value {
	if rw.seed == nil {
		return nil
	}
	v := rw.seed[col]
	return &v
}

func (rw *rowWork) field(c *Column, b, s, l *Value, verdict string) {
	rw.fields = append(rw.fields, FieldState{Column: c.Name, Own: c.Own, Base: b, Seed: s, Live: l, Verdict: verdict})
}

func (rw *rowWork) upsertLedger(vals map[string]Value, retired bool) {
	rw.ledgerOp, rw.ledgerVals, rw.ledgerRet = ledgerUpsert, vals, retired
	if rw.base != nil && rw.oldKey == "" && rw.base.retired == retired && encodeVals(rw.base.vals) == encodeVals(vals) {
		rw.ledgerOp = ledgerNone // the ledger already says this
	}
}

// recordLines adds a record line for rows whose only change is the ledger
// catching up, so a plan never hides a write.
func (tw *tableWork) recordLines(p *Plan) {
	for _, rw := range tw.rows {
		if rw.ledgerOp != ledgerNone && len(rw.changes) == 0 && rw.live != nil && rw.seed != nil {
			reason := "already matches the seed"
			if rw.base != nil {
				reason = "matches the seed's new values"
			}
			tw.change(p, rw, Change{Action: ActRecord, Reason: reason})
		}
	}
}
