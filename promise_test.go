// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

// TestPromise checks the main promise on random histories: an admin's edit
// is never silently overwritten, and every seed change to a value nobody
// touched lands.
//
// Each history runs a table through a dozen seed versions. Between versions
// an admin edits values, deletes rows and adds rows of their own. Each apply
// settles conflicts by keeping the local value, taking the seed's, or not at
// all (and is then blocked). After every apply the test compares every
// value against rules worked out from its own record of what was applied
// last, not from the engine's ledger:
//
//   - merge column, unchanged locally, changed in the seed: the seed's value
//   - merge column, changed locally: the local value, unless the plan named
//     that value as a conflict and the apply took the seed's
//   - seed column: the seed's value; local column: the local value
//   - a row the admin deleted stays deleted; a row the seed adds appears
//   - a row dropped from the seed is left, deleted or retired as on_remove
//     says, and a delete never takes a locally edited row without a conflict
//   - rows the seed never named are not touched
//   - a blocked apply writes nothing, and a second apply writes nothing
const promiseSchema = `CREATE TABLE items (
	id INTEGER PRIMARY KEY,
	code TEXT NOT NULL UNIQUE,
	a TEXT, b INTEGER, s TEXT, l TEXT,
	retired_at TEXT)`

type cell struct{ a, b, s, l sql.NullString }

type itemRow struct {
	code    string
	vals    map[string]string // column -> value, "" key absent means NULL
	retired bool
}

var promiseCols = []string{"a", "b", "s", "l"}

type promiseStats struct {
	Histories      int `json:"histories"`
	SeedVersions   int `json:"seed_versions"`
	Applies        int `json:"applies"`
	Blocked        int `json:"blocked_applies"`
	ValuesChecked  int `json:"values_checked"`
	RowsChecked    int `json:"rows_checked"`
	KeptEdits      int `json:"local_edits_kept"`
	SeedLanded     int `json:"seed_changes_landed"`
	Conflicts      int `json:"conflicts_reported"`
	SettledSeed    int `json:"conflicts_settled_seed"`
	SettledLocal   int `json:"conflicts_settled_local"`
	LocalDeletes   int `json:"local_deletes_kept"`
	Retired        int `json:"rows_retired"`
	Deleted        int `json:"rows_deleted"`
	RepeatsChecked int `json:"repeat_applies_checked"`
}

func TestPromise(t *testing.T) {
	eachBackend(t, testPromise)
}

func testPromise(t *testing.T, b backend) {
	histories := 120 * scale()
	if testing.Short() {
		histories = 20
	}
	var st promiseStats
	for h := 0; h < histories; h++ {
		runHistory(t, b, int64(h), &st)
	}
	st.Histories = histories
	t.Logf("%+v", st)
	if st.KeptEdits == 0 || st.SeedLanded == 0 || st.Conflicts == 0 || st.SettledSeed == 0 || st.SettledLocal == 0 ||
		st.LocalDeletes == 0 || st.Retired == 0 || st.Deleted == 0 || st.Blocked == 0 {
		t.Fatalf("the histories didn't reach every case: %+v", st)
	}
	record(t, resultName("promise", b), st)
}

type nullable = *string

func str(s string) nullable { return &s }

func runHistory(t *testing.T, b backend, seed int64, st *promiseStats) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	db := b.open(t)
	defer db.Close()
	if !b.pg {
		mustExec(t, db, "PRAGMA synchronous = OFF")
	}
	mustExec(t, db, b.ddl(promiseSchema))
	onRemove := []string{`"keep"`, `"delete"`, `{ retire = "retired_at" }`}[rng.Intn(3)]
	removeMode := map[string]string{`"keep"`: "keep", `"delete"`: "delete", `{ retire = "retired_at" }`: "retire"}[onRemove]

	values := map[string][]string{"a": {"x", "y", "z"}, "b": {"1", "2", "3"}, "s": {"p", "q"}, "l": {"m", "n"}}
	pick := func(col string) nullable {
		if rng.Intn(6) == 0 {
			return nil
		}
		vs := values[col]
		return str(vs[rng.Intn(len(vs))])
	}
	codes := make([]string, 24)
	for i := range codes {
		codes[i] = fmt.Sprintf("c%02d", i)
	}

	seedRows := map[string]map[string]nullable{} // the current seed version
	base := map[string]map[string]nullable{}     // what this test knows was applied last
	retiredBase := map[string]bool{}             // dropped rows the engine retired
	everSeeded := map[string]bool{}

	for v := 0; v < 12; v++ {
		st.SeedVersions++
		// A new seed version: add, drop and change rows.
		for _, c := range codes {
			r := seedRows[c]
			switch {
			case r == nil && rng.Intn(4) == 0:
				seedRows[c] = map[string]nullable{"a": pick("a"), "b": pick("b"), "s": pick("s"), "l": pick("l")}
			case r != nil && rng.Intn(10) == 0:
				delete(seedRows, c)
			case r != nil:
				for _, col := range promiseCols {
					if rng.Intn(4) == 0 {
						r[col] = pick(col)
					}
				}
			}
		}
		for c := range seedRows {
			everSeeded[c] = true
		}

		// The admin's turn.
		live := readItems(t, db)
		for _, c := range codes {
			r, ok := live[c]
			switch {
			case ok && rng.Intn(12) == 0:
				mustExec(t, db, "DELETE FROM items WHERE code = ?", c)
			case ok:
				for _, col := range promiseCols {
					if rng.Intn(5) == 0 {
						mustExec(t, db, "UPDATE items SET "+col+" = ? WHERE code = ?", nullArg(pick(col)), c)
					}
				}
				_ = r
			case !ok && rng.Intn(20) == 0:
				mustExec(t, db, "INSERT INTO items (code, a, b, s, l) VALUES (?, ?, ?, ?, ?)", c,
					nullArg(pick("a")), nullArg(pick("b")), nullArg(pick("s")), nullArg(pick("l")))
			}
		}

		set := setFrom(t, map[string]string{"items.toml": seedTOML(seedRows, onRemove)})
		take := []bs.Take{bs.TakeFail, bs.TakeLocal, bs.TakeSeed}[rng.Intn(3)]
		opts := &bs.Options{}
		if take != bs.TakeFail {
			opts.Take = []bs.Resolution{{Take: take}}
		}

		before := readItems(t, db)
		beforeDump := dump(t, db)
		pre, err := bs.Preview(ctx, db, set, opts)
		if err != nil {
			t.Fatalf("history %d v%d: preview: %v", seed, v, err)
		}
		plan, err := bs.Apply(ctx, db, set, opts)
		st.Applies++
		if err != nil && !errors.Is(err, bs.ErrBlocked) {
			t.Fatalf("history %d v%d: preview said %q, apply failed: %v", seed, v, actions(pre), err)
		}
		if actions(pre) != actions(plan) || pre.Blocked() != plan.Blocked() {
			t.Fatalf("history %d v%d: preview and apply differ:\npreview %s\n  apply %s", seed, v, actions(pre), actions(plan))
		}
		checkPending(t, plan)
		// Every conflict, blocking or settled, must be a real one by the
		// rules: a merge value changed on both sides, differently; a row
		// already there with a different value; or a dropped row edited.
		for _, ch := range plan.Changes {
			if ch.Action != bs.ActConflict {
				continue
			}
			c := ch.KeyText()
			s, inSeed := seedRows[c]
			b, inBase := base[c]
			l, wasLive := before[c]
			real := false
			switch {
			case ch.Column == "" && !inSeed && inBase && wasLive && removeMode == "delete":
				real = !same(l.get("a"), b["a"]) || !same(l.get("b"), b["b"])
			case (ch.Column == "a" || ch.Column == "b") && inSeed && wasLive && !inBase:
				real = !same(l.get(ch.Column), s[ch.Column])
			case (ch.Column == "a" || ch.Column == "b") && inSeed && wasLive && inBase:
				sv, bv, lv := s[ch.Column], b[ch.Column], l.get(ch.Column)
				real = !same(sv, bv) && !same(lv, bv) && !same(lv, sv)
			}
			if !real {
				t.Fatalf("history %d v%d, row %s: a conflict on %q that the rules don't call for\nbase %v\nseed %v\nbefore %v",
					seed, v, c, ch.Column, show(base[c]), show(seedRows[c]), showRow(before[c]))
			}
		}
		if errors.Is(err, bs.ErrBlocked) {
			st.Blocked++
			if after := dump(t, db); after != beforeDump {
				t.Fatalf("history %d v%d: a blocked apply wrote to the database", seed, v)
			}
			for _, ch := range plan.Changes {
				if ch.Blocking() {
					st.Conflicts++
				}
			}
			continue
		}
		if err != nil {
			t.Fatalf("history %d v%d: %v", seed, v, err)
		}
		after := readItems(t, db)

		conflicts := map[string]bs.Take{} // "code/col" or "code/" -> how it was settled
		for _, ch := range plan.Changes {
			if ch.Action == bs.ActConflict {
				conflicts[ch.KeyText()+"/"+ch.Column] = ch.Resolved
				st.Conflicts++
				if ch.Resolved == bs.TakeSeed {
					st.SettledSeed++
				} else {
					st.SettledLocal++
				}
			}
		}
		fail := func(code, format string, args ...any) {
			t.Helper()
			t.Fatalf("history %d v%d, row %s (on_remove %s, take %s): %s\nbase %v\nseed %v\nbefore %v\nafter %v",
				seed, v, code, removeMode, take, fmt.Sprintf(format, args...),
				show(base[code]), show(seedRows[code]), showRow(before[code]), showRow(after[code]))
		}

		for _, c := range codes {
			s, inSeed := seedRows[c]
			b, inBase := base[c]
			l, wasLive := before[c]
			a, isLive := after[c]
			st.RowsChecked++
			switch {
			case inSeed && !wasLive:
				if inBase {
					st.LocalDeletes++
					if isLive {
						fail(c, "the admin deleted it and the apply put it back")
					}
				} else {
					if !isLive {
						fail(c, "a new seed row wasn't inserted")
					}
					for _, col := range promiseCols {
						st.ValuesChecked++
						if !same(a.get(col), s[col]) {
							fail(c, "inserted %s = %s, want the seed's %s", col, sp(a.get(col)), sp(s[col]))
						}
					}
				}
			case inSeed && wasLive:
				if !isLive {
					fail(c, "a seed row disappeared")
				}
				for _, col := range promiseCols {
					st.ValuesChecked++
					sv, lv, av := s[col], l.get(col), a.get(col)
					var bv nullable
					if inBase {
						bv = b[col]
					}
					settled, conflict := conflicts[c+"/"+col]
					want := lv
					switch {
					case col == "s":
						want = sv
					case col == "l":
						want = lv
					case same(lv, sv):
						want = lv
					case !inBase:
						// Adopted with a different value: must be a conflict.
						if !conflict {
							fail(c, "%s: the row was already there with %s, the seed says %s, and no conflict was reported", col, sp(lv), sp(sv))
						}
						if settled == bs.TakeSeed {
							want = sv
						}
					case same(sv, bv):
						if !same(lv, bv) {
							st.KeptEdits++
						}
						want = lv
					case same(lv, bv):
						st.SeedLanded++
						want = sv
					default:
						if !conflict {
							fail(c, "%s: changed in the seed (%s -> %s) and locally (%s) with no conflict", col, sp(bv), sp(sv), sp(lv))
						}
						if settled == bs.TakeSeed {
							want = sv
						}
					}
					if conflict && col != "a" && col != "b" {
						fail(c, "%s: a %s column reported a conflict", col, map[string]string{"s": "seed", "l": "local"}[col])
					}
					if !same(av, want) {
						fail(c, "%s = %s after the apply, want %s", col, sp(av), sp(want))
					}
				}
				if l.retired != a.retired && !(inBase && retiredBase[c]) {
					fail(c, "the retire column changed")
				}
			case !inSeed && inBase && wasLive:
				if retiredBase[c] {
					if !rowSame(l, a) {
						fail(c, "an already retired row was changed")
					}
					continue
				}
				switch removeMode {
				case "keep":
					if !isLive || !rowSame(l, a) {
						fail(c, "on_remove = keep changed the row")
					}
				case "retire":
					st.Retired++
					if !isLive || !a.retired || !sameVals(l, a) {
						fail(c, "on_remove = retire didn't retire the row, or changed more")
					}
				case "delete":
					edited := !same(l.get("a"), b["a"]) || !same(l.get("b"), b["b"])
					settled, conflict := conflicts[c+"/"]
					switch {
					case edited && !conflict:
						fail(c, "deleted a locally edited row without a conflict")
					case edited && settled == bs.TakeLocal:
						if !isLive || !rowSame(l, a) {
							fail(c, "the conflict kept the row, but it changed")
						}
					default:
						st.Deleted++
						if isLive {
							fail(c, "on_remove = delete left the row")
						}
					}
				}
			default:
				// Never in the seed, or gone from both: untouched.
				if wasLive != isLive || (wasLive && !rowSame(l, a)) {
					fail(c, "a row the seed doesn't manage was changed")
				}
			}
		}

		// The apply settled everything: a second one finds nothing to write.
		again, err := bs.Apply(ctx, db, set, opts)
		checkPending(t, again)
		if err != nil || again.Applied || again.Pending() {
			t.Fatalf("history %d v%d: a second apply wasn't empty: applied=%v err=%v %v", seed, v, again.Applied, err, again.Changes)
		}
		st.RepeatsChecked++

		// What this test now knows was applied.
		next := map[string]map[string]nullable{}
		for c, r := range seedRows {
			cp := map[string]nullable{}
			for k, x := range r {
				cp[k] = x
			}
			next[c] = cp
		}
		for c := range base {
			if _, ok := seedRows[c]; ok {
				delete(retiredBase, c)
				continue
			}
			_, wasLive := before[c]
			if wasLive && removeMode == "retire" {
				next[c] = base[c] // the engine keeps a retired row in its ledger
				retiredBase[c] = true
			}
			if retiredBase[c] {
				next[c] = base[c]
			}
		}
		for c := range next {
			if _, live := after[c]; !live && retiredBase[c] {
				delete(next, c) // a retired row the admin then deleted is forgotten
				delete(retiredBase, c)
			}
		}
		base = next
	}
}

// checkPending checks a plan says it would write exactly when one of its
// lines is a write: data, or the ledger recording or forgetting a row.
func checkPending(t *testing.T, p *bs.Plan) {
	t.Helper()
	shows := false
	for _, c := range p.Changes {
		switch c.Action {
		case bs.ActInsert, bs.ActUpdate, bs.ActDelete, bs.ActRetire, bs.ActUnretire, bs.ActRename, bs.ActRecord, bs.ActRelease:
			shows = true
		case bs.ActConflict:
			shows = shows || c.Resolved != ""
		}
	}
	if !p.Applied && p.Pending() != shows && !p.Blocked() {
		t.Fatalf("Pending() is %v, but the plan's lines say %v: %s", p.Pending(), shows, actions(p))
	}
}

func readItems(t *testing.T, db *sql.DB) map[string]itemRow {
	t.Helper()
	rows, err := db.Query("SELECT code, a, b, s, l, retired_at FROM items")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]itemRow{}
	for rows.Next() {
		var code string
		var vals [4]sql.NullString
		var ret sql.NullString
		if err := rows.Scan(&code, &vals[0], &vals[1], &vals[2], &vals[3], &ret); err != nil {
			t.Fatal(err)
		}
		r := itemRow{code: code, vals: map[string]string{}, retired: ret.Valid}
		for i, col := range promiseCols {
			if vals[i].Valid {
				r.vals[col] = vals[i].String
			}
		}
		out[code] = r
	}
	return out
}

// get is a value as the seed model holds it: nil for NULL.
func (r itemRow) get(col string) nullable {
	if v, ok := r.vals[col]; ok {
		return &v
	}
	return nil
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(rebind(db, q), args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func nullArg(v nullable) any {
	if v == nil {
		return nil
	}
	return *v
}

// same compares a live value (present in the map or not) with a seed value.
func same(x any, y nullable) bool {
	var xs *string
	switch v := x.(type) {
	case string:
		xs = &v
	case nullable:
		xs = v
	}
	if xs == nil || y == nil {
		return xs == nil && y == nil
	}
	return *xs == *y
}

func sp(x any) string {
	switch v := x.(type) {
	case string:
		return fmt.Sprintf("%q", v)
	case nullable:
		if v == nil {
			return "NULL"
		}
		return fmt.Sprintf("%q", *v)
	}
	return "?"
}

func rowSame(x, y itemRow) bool { return x.retired == y.retired && sameVals(x, y) }

func sameVals(x, y itemRow) bool {
	for _, col := range promiseCols {
		xv, xok := x.vals[col]
		yv, yok := y.vals[col]
		if xok != yok || xv != yv {
			return false
		}
	}
	return true
}

func show(r map[string]nullable) string {
	if r == nil {
		return "(none)"
	}
	var parts []string
	for _, col := range promiseCols {
		parts = append(parts, col+"="+sp(r[col]))
	}
	return strings.Join(parts, " ")
}

func showRow(r itemRow) string {
	if r.code == "" {
		return "(none)"
	}
	var parts []string
	for _, col := range promiseCols {
		if v, ok := r.vals[col]; ok {
			parts = append(parts, col+"="+fmt.Sprintf("%q", v))
		} else {
			parts = append(parts, col+"=NULL")
		}
	}
	if r.retired {
		parts = append(parts, "retired")
	}
	return strings.Join(parts, " ")
}

func seedTOML(rows map[string]map[string]nullable, onRemove string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table = \"items\"\nkey = \"code\"\non_remove = %s\n\n[columns]\na = \"merge\"\nb = \"merge\"\ns = \"seed\"\nl = \"local\"\n", onRemove)
	var codes []string
	for c := range rows {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		fmt.Fprintf(&b, "\n[[row]]\ncode = %q\n", c)
		for _, col := range promiseCols {
			v := rows[c][col]
			if v == nil {
				continue // absent means NULL
			}
			if col == "b" {
				fmt.Fprintf(&b, "%s = %s\n", col, *v)
			} else {
				fmt.Fprintf(&b, "%s = %q\n", col, *v)
			}
		}
	}
	return b.String()
}
