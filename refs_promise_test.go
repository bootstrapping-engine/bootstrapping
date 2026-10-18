// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

// TestPromiseReferences runs random histories through three linked tables,
// with SQLite's foreign keys on: roles that point at a parent role, a list
// of permissions, and the links between them, which carry a note admins
// edit. Seed versions add, drop and rename roles, move parents, add and
// drop permissions and links, and change notes. Admins edit names and
// notes and delete links in between.
//
// After every apply it checks:
//
//   - the apply didn't fail (only a conflict may stop it), and did exactly
//     what Preview said it would
//   - SQLite finds no foreign key violation, and the database is intact
//   - a renamed role kept its id, and its links
//   - a note an admin edited since the last apply is unchanged, unless the
//     plan named it as a conflict and the apply took the seed's
//   - a second apply writes nothing
const refsSchema = `
CREATE TABLE roles (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, name TEXT,
	parent_id INTEGER REFERENCES roles (id), retired_at TEXT);
CREATE TABLE perms (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, retired_at TEXT);
CREATE TABLE links (role_id INTEGER NOT NULL REFERENCES roles (id), perm_id INTEGER NOT NULL REFERENCES perms (id),
	note TEXT, PRIMARY KEY (role_id, perm_id));`

type refsStats struct {
	Histories    int `json:"histories"`
	Applies      int `json:"applies"`
	Blocked      int `json:"blocked_applies"`
	Renames      int `json:"renames"`
	IDsKept      int `json:"renamed_ids_checked"`
	NotesChecked int `json:"edited_notes_checked"`
	NotesKept    int `json:"edited_notes_kept"`
	Skipped      int `json:"skipped_for_missing_rows"`
	Protected    int `json:"deletes_held_while_referred_to"`
	Deleted      int `json:"rows_deleted"`
	FKChecks     int `json:"foreign_key_checks"`
}

func TestPromiseReferences(t *testing.T) {
	histories := 60 * scale()
	if testing.Short() {
		histories = 10
	}
	var st refsStats
	for h := 0; h < histories; h++ {
		refsHistory(t, int64(1000+h), &st)
	}
	st.Histories = histories
	t.Logf("%+v", st)
	if st.Renames == 0 || st.NotesKept == 0 || st.Skipped == 0 || st.Protected == 0 || st.Deleted == 0 || st.Blocked == 0 {
		t.Fatalf("the histories didn't reach every case: %+v", st)
	}
	record(t, "references", st)
}

type refsSeed struct {
	roles map[string][2]string // code -> name, parent code ("" for none)
	perms map[string]bool
	links map[[2]string]string // role, perm -> note
	from  map[string]string    // new code -> old code, this version only
}

func (s refsSeed) files(roleRemove, permRemove string) map[string]string {
	var r, p, l strings.Builder
	fmt.Fprintf(&r, "table = \"roles\"\nkey = \"code\"\non_remove = %s\n[refs]\nparent_id = \"roles.code\"\n", roleRemove)
	for _, code := range sortedKeys(s.roles) {
		v := s.roles[code]
		fmt.Fprintf(&r, "\n[[row]]\ncode = %q\nname = %q\n", code, v[0])
		if v[1] != "" {
			fmt.Fprintf(&r, "parent_id = %q\n", v[1])
		}
		if old := s.from[code]; old != "" {
			fmt.Fprintf(&r, "_renamed_from = %q\n", old)
		}
	}
	fmt.Fprintf(&p, "table = \"perms\"\nkey = \"code\"\non_remove = %s\n", permRemove)
	for _, code := range sortedKeys(s.perms) {
		fmt.Fprintf(&p, "\n[[row]]\ncode = %q\n", code)
	}
	l.WriteString("table = \"links\"\nkey = [\"role_id\", \"perm_id\"]\non_remove = \"delete\"\n[refs]\nrole_id = \"roles.code\"\nperm_id = \"perms.code\"\n")
	for _, k := range linkKeys(s.links) {
		fmt.Fprintf(&l, "\n[[row]]\nrole_id = %q\nperm_id = %q\nnote = %q\n", k[0], k[1], s.links[k])
	}
	return map[string]string{"roles.toml": r.String(), "perms.toml": p.String(), "links.toml": l.String()}
}

func linkKeys(m map[[2]string]string) [][2]string {
	var keys [][2]string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func refsHistory(t *testing.T, seed int64, st *refsStats) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "r.db")+"?_foreign_keys=1&_busy_timeout=30000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, refsSchema)
	removes := []string{`"keep"`, `"delete"`, `{ retire = "retired_at" }`}
	roleRemove, permRemove := removes[rng.Intn(3)], removes[rng.Intn(3)]

	s := refsSeed{roles: map[string][2]string{}, perms: map[string]bool{}, links: map[[2]string]string{}}
	next := 0
	newCode := func(prefix string) string { next++; return fmt.Sprintf("%s%d", prefix, next) }
	notes := []string{"n1", "n2", "n3"}
	edited := map[[2]string]bool{} // links an admin edited since the last apply, by code
	var lastApplied map[string]bool

	for v := 0; v < 10; v++ {
		s.from = map[string]string{}
		// Roles: add, drop, rename (only rows the last apply wrote), reparent.
		for _, code := range sortedKeys(s.roles) {
			switch r := rng.Intn(10); {
			case r == 0:
				delete(s.roles, code)
			case r == 1 && lastApplied[code]:
				n := newCode("r")
				s.roles[n] = s.roles[code]
				s.from[n] = code
				delete(s.roles, code)
				for _, k := range linkKeys(s.links) {
					if note := s.links[k]; k[0] == code {
						delete(s.links, k)
						s.links[[2]string{n, k[1]}] = note
					}
				}
			}
		}
		if rng.Intn(2) == 0 || len(s.roles) < 2 {
			s.roles[newCode("r")] = [2]string{"R", ""}
		}
		codes := sortedKeys(s.roles)
		for _, code := range codes {
			v := s.roles[code]
			if rng.Intn(4) == 0 {
				switch rng.Intn(4) {
				case 0:
					v[1] = ""
				case 1:
					v[1] = "ghost" // a role that never exists: skipped
				default:
					v[1] = codes[rng.Intn(len(codes))]
				}
			}
			if rng.Intn(5) == 0 {
				v[0] = []string{"A", "B", "C"}[rng.Intn(3)]
			}
			s.roles[code] = v
		}
		// Permissions and links.
		for _, code := range sortedKeys(s.perms) {
			if rng.Intn(8) == 0 {
				delete(s.perms, code)
			}
		}
		if rng.Intn(2) == 0 || len(s.perms) == 0 {
			s.perms[newCode("p")] = true
		}
		perms := sortedKeys(s.perms)
		for _, k := range linkKeys(s.links) {
			if rng.Intn(6) == 0 {
				delete(s.links, k)
			} else if rng.Intn(4) == 0 {
				s.links[k] = notes[rng.Intn(len(notes))]
			}
		}
		for i := 0; i < 3; i++ {
			s.links[[2]string{codes[rng.Intn(len(codes))], perms[rng.Intn(len(perms))]}] = notes[rng.Intn(len(notes))]
		}

		// The admin's turn: edit notes and names, delete some links.
		rows, err := db.Query("SELECT role_id, perm_id, r.code, p.code FROM links JOIN roles r ON r.id = role_id JOIN perms p ON p.id = perm_id ORDER BY 3, 4")
		if err != nil {
			t.Fatal(err)
		}
		type liveLink struct {
			ids   [2]int64
			codes [2]string
		}
		var live []liveLink
		for rows.Next() {
			var l liveLink
			rows.Scan(&l.ids[0], &l.ids[1], &l.codes[0], &l.codes[1])
			live = append(live, l)
		}
		rows.Close()
		for _, l := range live {
			switch rng.Intn(8) {
			case 0:
				mustExec(t, db, "UPDATE links SET note = ? WHERE role_id = ? AND perm_id = ?", "local "+notes[rng.Intn(3)], l.ids[0], l.ids[1])
				edited[l.codes] = true
			case 1:
				mustExec(t, db, "DELETE FROM links WHERE role_id = ? AND perm_id = ?", l.ids[0], l.ids[1])
				delete(edited, l.codes)
			}
		}
		if rng.Intn(3) == 0 {
			mustExec(t, db, "UPDATE roles SET name = 'local' WHERE id = (SELECT min(id) FROM roles)")
		}

		set := setFrom(t, s.files(roleRemove, permRemove))
		opts := &bs.Options{}
		if take := rng.Intn(3); take > 0 {
			opts.Take = []bs.Resolution{{Take: []bs.Take{"", bs.TakeLocal, bs.TakeSeed}[take]}}
		}
		idsBefore := codeIDs(t, db)
		notesBefore := linkNotes(t, db)
		pre, err := bs.Preview(ctx, db, set, opts)
		if err != nil {
			t.Fatalf("history %d v%d: preview: %v", seed, v, err)
		}
		p, err := bs.Apply(ctx, db, set, opts)
		st.Applies++
		where := fmt.Sprintf("history %d v%d (roles %s, perms %s)", seed, v, roleRemove, permRemove)
		if err != nil && !errors.Is(err, bs.ErrBlocked) {
			t.Fatalf("%s: preview said %q, apply failed: %v", where, actions(pre), err)
		}
		if actions(pre) != actions(p) {
			t.Fatalf("%s: preview and apply differ:\npreview %s\n  apply %s", where, actions(pre), actions(p))
		}
		var n int
		db.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&n)
		var ok string
		db.QueryRow("PRAGMA integrity_check").Scan(&ok)
		st.FKChecks++
		if n != 0 || ok != "ok" {
			t.Fatalf("%s: %d foreign key violations, integrity %q", where, n, ok)
		}
		if errors.Is(err, bs.ErrBlocked) {
			st.Blocked++
			continue
		}
		idsAfter := codeIDs(t, db)
		notesAfter := linkNotes(t, db)
		settled := map[string]bs.Take{}
		for _, c := range p.Changes {
			switch c.Action {
			case bs.ActRename:
				st.Renames++
				old := s.from[c.KeyText()]
				if idsBefore["roles/"+old] == 0 || idsAfter["roles/"+c.KeyText()] != idsBefore["roles/"+old] {
					t.Fatalf("%s: renaming %s to %s changed its id", where, old, c.KeyText())
				}
				st.IDsKept++
			case bs.ActSkip:
				if c.Column == "" {
					st.Protected++
				} else {
					st.Skipped++
				}
			case bs.ActDelete:
				st.Deleted++
			case bs.ActConflict:
				settled[c.Table+"/"+c.KeyText()+"/"+c.Column] = c.Resolved
			}
		}
		renamedTo := map[string]string{}
		for c := range applied(p, bs.ActRename) {
			renamedTo[s.from[c]] = c
		}
		for k := range edited {
			before, wasThere := notesBefore[k]
			if to := renamedTo[k[0]]; to != "" {
				k[0] = to
			}
			after, isThere := notesAfter[k]
			if !wasThere || !isThere {
				continue
			}
			st.NotesChecked++
			key := "links/" + k[0] + ", " + k[1] + "/note"
			if settled[key] == bs.TakeSeed {
				continue
			}
			if before != after {
				t.Fatalf("%s: the admin's note on %s changed from %q to %q with no conflict taken for the seed\n%s", where, key, before, after, actions(p))
			}
			st.NotesKept++
		}
		edited = map[[2]string]bool{}
		again, err := bs.Apply(ctx, db, set, opts)
		if err != nil || again.Applied {
			t.Fatalf("%s: a second apply wasn't empty: %v %s", where, err, actions(again))
		}
		checkPending(t, again)
		lastApplied = map[string]bool{}
		for code := range s.roles {
			if idsAfter["roles/"+code] != 0 {
				lastApplied[code] = true
			}
		}
	}
}

func codeIDs(t *testing.T, db *sql.DB) map[string]int64 {
	out := map[string]int64{}
	for _, tbl := range []string{"roles", "perms"} {
		rows, err := db.Query("SELECT code, id FROM " + tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var code string
			var id int64
			rows.Scan(&code, &id)
			out[tbl+"/"+code] = id
		}
		rows.Close()
	}
	return out
}

func linkNotes(t *testing.T, db *sql.DB) map[[2]string]string {
	out := map[[2]string]string{}
	rows, err := db.Query("SELECT r.code, p.code, coalesce(note, '') FROM links JOIN roles r ON r.id = role_id JOIN perms p ON p.id = perm_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k [2]string
		var note string
		rows.Scan(&k[0], &k[1], &note)
		out[k] = note
	}
	return out
}

// applied is the keys of the roles the plan changed with act.
func applied(p *bs.Plan, act bs.Action) map[string]bool {
	out := map[string]bool{}
	for _, c := range p.Changes {
		if c.Table == "roles" && c.Action == act {
			out[c.KeyText()] = true
		}
	}
	return out
}
