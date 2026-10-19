// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

// These tests run applies in child processes: the test binary started
// again with BS_CHILD set. TestMain sends those runs to childMain.

func TestMain(m *testing.M) {
	if os.Getenv("BS_CHILD") != "" {
		os.Exit(childMain())
	}
	os.Exit(m.Run())
}

var testClock = func() time.Time { return time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) }

// childMain applies $BS_SEEDS to $BS_DB. With $BS_KILL_AFTER = k it stops
// after its k-th statement, says "ready" and waits to be killed. It prints
// one JSON line with what the apply did.
func childMain() int {
	set, err := bs.LoadDir(os.Getenv("BS_SEEDS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	db, err := openChild(os.Getenv("BS_DB"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()
	opts := &bs.Options{Now: testClock}
	if k, _ := strconv.Atoi(os.Getenv("BS_KILL_AFTER")); k > 0 {
		n := 0
		opts.Progress = func(bs.Change) {
			if n++; n == k {
				fmt.Println("ready")
				os.Stdout.Sync()
				time.Sleep(time.Hour)
			}
		}
	}
	if os.Getenv("BS_GO") != "" {
		fmt.Println("waiting")
		os.Stdout.Sync()
		bufio.NewReader(os.Stdin).ReadString('\n') // start together
	}
	p, err := bs.Apply(context.Background(), db, set, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"applied": p.Applied, "run": p.Run, "writes": p.Writes()})
	return 0
}

// bigSeeds writes a seed directory with n rows, version v of them.
func bigSeeds(t *testing.T, dir string, n, v int) string {
	t.Helper()
	d := filepath.Join(dir, fmt.Sprintf("seeds-v%d", v))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("table = \"items\"\nkey = \"code\"\non_remove = \"delete\"\n\n[columns]\ns = \"seed\"\nl = \"local\"\n")
	rng := rand.New(rand.NewSource(int64(v)))
	for i := 0; i < n; i++ {
		// Each version drops some rows, adds others and changes values.
		if rng.Intn(10) == 0 && v > 1 {
			continue
		}
		fmt.Fprintf(&b, "\n[[row]]\ncode = \"r%05d\"\na = \"a%d-%d\"\nb = %d\ns = \"s%d\"\nl = \"l%d\"\n",
			i, i, v*rng.Intn(3), i*v, v, i)
	}
	for i := 0; i < n/10; i++ {
		fmt.Fprintf(&b, "\n[[row]]\ncode = \"v%d-%04d\"\na = \"new\"\n", v, i)
	}
	if err := os.WriteFile(filepath.Join(d, "items.toml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func applyIn(t *testing.T, path, seeds string, progress func(bs.Change)) *bs.Plan {
	t.Helper()
	db := openLoc(t, path)
	defer db.Close()
	set, err := bs.LoadDir(seeds)
	if err != nil {
		t.Fatal(err)
	}
	p, err := bs.Apply(context.Background(), db, set, &bs.Options{Now: testClock, Progress: progress})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func dumpPath(t *testing.T, path string) string {
	t.Helper()
	db := openLoc(t, path)
	defer db.Close()
	return dump(t, db)
}

func child(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{"BS_CHILD=1"}, env...)...)
	cmd.Stderr = os.Stderr
	return cmd
}

type crashStats struct {
	Rows             int     `json:"rows_in_seed"`
	Statements       int     `json:"statements_per_apply"`
	ApplySeconds     float64 `json:"apply_seconds"`
	Trials           int     `json:"trials"`
	KilledMidway     int     `json:"killed_after_a_statement"`
	KilledByTimer    int     `json:"killed_at_a_random_moment"`
	FoundBefore      int     `json:"found_as_before"`
	FoundAfter       int     `json:"found_as_after"`
	IntegrityPassed  int     `json:"integrity_checks_passed,omitempty"` // SQLite's integrity check
	ReappliesMatched int     `json:"reapplies_matching_a_clean_apply"`
}

// TestCrash kills applies with SIGKILL (TerminateProcess on Windows) part
// way through, and checks the database is always either exactly as it was
// before or exactly as a finished apply leaves it, never in between; that
// SQLite's integrity check passes; and that applying again ends in exactly
// the state a clean apply gives.
func TestCrash(t *testing.T) {
	eachBackend(t, testCrash)
}

const itemsDDL = "CREATE TABLE items (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, a TEXT, b INTEGER, s TEXT, l TEXT)"

func testCrash(t *testing.T, b backend) {
	dir := t.TempDir()
	rows := 1500
	v1, v2 := bigSeeds(t, dir, rows, 1), bigSeeds(t, dir, rows, 2)
	// prepare makes the starting state: v1 applied, then local edits, so
	// the next apply keeps some and conflicts with none. On SQLite it's
	// made once and copied; on PostgreSQL it's made again in a new schema,
	// the same way each time.
	prepare := func(loc string) {
		db := openLoc(t, loc)
		mustExec(t, db, b.ddl(itemsDDL))
		db.Close()
		applyIn(t, loc, v1, nil)
		db = openLoc(t, loc)
		mustExec(t, db, "UPDATE items SET l = 'edited' WHERE id % 7 = 0")
		mustExec(t, db, "DELETE FROM items WHERE id % 50 = 0")
		db.Close()
	}
	start := newLoc(t, b, dir, "start")
	prepare(start)
	fresh := func(name string) string {
		loc := newLoc(t, b, dir, name)
		if b.pg {
			prepare(loc)
		} else {
			copyFile(t, start, loc)
		}
		return loc
	}
	before := dumpPath(t, start)

	// A clean apply, counted and timed: the outcome every trial must match.
	ref := fresh("ref")
	statements := 0
	began := time.Now()
	applyIn(t, ref, v2, func(bs.Change) { statements++ })
	took := time.Since(began)
	after := dumpPath(t, ref)
	if before == after || statements < 100 {
		t.Fatalf("the reference apply did too little: %d statements", statements)
	}

	st := crashStats{Rows: rows, Statements: statements, ApplySeconds: took.Seconds()}
	trials := 40 * scale()
	if b.pg {
		trials = 10 * scale() // each trial builds its starting state again, which takes a few seconds
	}
	if testing.Short() {
		trials = 6
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < trials; i++ {
		path := fresh(fmt.Sprintf("trial%d", i))
		midway := i%2 == 0
		var cmd *exec.Cmd
		if midway {
			k := 1 + rng.Intn(statements)
			cmd = child(t, "BS_DB="+path, "BS_SEEDS="+v2, "BS_KILL_AFTER="+strconv.Itoa(k))
			out, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			line, _ := bufio.NewReader(out).ReadString('\n')
			if strings.TrimSpace(line) != "ready" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatalf("trial %d: the child never reached statement %d: %q", i, k, line)
			}
			st.KilledMidway++
		} else {
			cmd = child(t, "BS_DB="+path, "BS_SEEDS="+v2)
			cmd.Stdout = io.Discard
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Anywhere from startup to well past a normal apply's end.
			time.Sleep(time.Duration(rng.Int63n(int64(3*took) + int64(20*time.Millisecond))))
			st.KilledByTimer++
		}
		cmd.Process.Kill()
		cmd.Wait()

		got := dumpPath(t, path)
		switch got {
		case before:
			st.FoundBefore++
		case after:
			if midway {
				t.Fatalf("trial %d: killed inside the transaction, yet the apply committed", i)
			}
			st.FoundAfter++
		default:
			t.Fatalf("trial %d: the database is neither as it was nor as an apply leaves it", i)
		}
		if !b.pg {
			db := openLoc(t, path)
			var ok string
			if err := db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil || ok != "ok" {
				t.Fatalf("trial %d: integrity check: %q %v", i, ok, err)
			}
			db.Close()
			st.IntegrityPassed++
		}
		applyIn(t, path, v2, nil)
		if dumpPath(t, path) != after {
			t.Fatalf("trial %d: applying again didn't give the clean apply's result", i)
		}
		st.ReappliesMatched++
		st.Trials++
		dropLoc(t, path)
	}
	t.Logf("%+v", st)
	record(t, resultName("crash", b), st)
}

type processStats struct {
	Rounds        int `json:"rounds"`
	Processes     int `json:"processes_per_round"`
	Applies       int `json:"applies_started"`
	Wrote         int `json:"applies_that_wrote"`
	FoundNothing  int `json:"applies_that_found_nothing_to_do"`
	StatesMatched int `json:"rounds_matching_a_single_apply"`
}

// TestProcesses starts eight processes applying the same seed version to
// one database file at the same moment, round after round. Each round
// exactly one of them must write; the rest wait for its lock and then find
// nothing to do; and the file must end up exactly as a single apply leaves it.
func TestProcesses(t *testing.T) {
	eachBackend(t, testProcesses)
}

func testProcesses(t *testing.T, b backend) {
	dir := t.TempDir()
	path, ref := newLoc(t, b, dir, "shared"), newLoc(t, b, dir, "ref")
	for _, p := range []string{path, ref} {
		db := openLoc(t, p)
		mustExec(t, db, b.ddl(itemsDDL))
		db.Close()
	}
	rounds, procs := 6*scale(), 8
	if testing.Short() {
		rounds = 2
	}
	st := processStats{Processes: procs}
	for r := 1; r <= rounds; r++ {
		seeds := bigSeeds(t, dir, 300, r)
		applyIn(t, ref, seeds, nil)

		var cmds []*exec.Cmd
		var outs []*bufio.Reader
		var ins []io.WriteCloser
		for i := 0; i < procs; i++ {
			cmd := child(t, "BS_DB="+path, "BS_SEEDS="+seeds, "BS_GO=1")
			out, _ := cmd.StdoutPipe()
			in, _ := cmd.StdinPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(out)
			if line, _ := br.ReadString('\n'); strings.TrimSpace(line) != "waiting" {
				t.Fatalf("child: %q", line)
			}
			cmds, outs, ins = append(cmds, cmd), append(outs, br), append(ins, in)
		}
		for _, in := range ins { // all at once
			in.Write([]byte("go\n"))
		}
		wrote := 0
		for i, cmd := range cmds {
			line, _ := outs[i].ReadString('\n')
			if err := cmd.Wait(); err != nil {
				t.Fatalf("round %d: a process failed: %v", r, err)
			}
			var res struct {
				Applied bool `json:"applied"`
			}
			if err := json.Unmarshal([]byte(line), &res); err != nil {
				t.Fatalf("round %d: %q", r, line)
			}
			st.Applies++
			if res.Applied {
				wrote++
				st.Wrote++
			} else {
				st.FoundNothing++
			}
		}
		if wrote != 1 {
			t.Fatalf("round %d: %d processes wrote, want exactly 1", r, wrote)
		}
		if dumpPath(t, path) != dumpPath(t, ref) {
			t.Fatalf("round %d: the shared file differs from a single apply's result", r)
		}
		st.StatesMatched++
		st.Rounds++
	}
	t.Logf("%+v", st)
	record(t, resultName("processes", b), st)
}

func openChild(loc string) (*sql.DB, error) {
	if strings.Contains(loc, "://") {
		return sql.Open("postgres", loc)
	}
	return sql.Open("sqlite3", "file:"+loc+"?_busy_timeout=30000")
}

// A location is where a test database lives: a SQLite file, or a
// PostgreSQL URL whose search_path is a schema of its own.

func newLoc(t *testing.T, b backend, dir, name string) string {
	t.Helper()
	if !b.pg {
		return filepath.Join(dir, name+".db")
	}
	db := b.open(t) // makes the schema and drops it after the test
	schema := currentSchema(t, db)
	db.Close()
	return pgURLWith(schema)
}

func openLoc(t *testing.T, loc string) *sql.DB {
	t.Helper()
	if !strings.Contains(loc, "://") {
		return openPath(t, loc)
	}
	db, err := sql.Open("postgres", loc)
	if err != nil {
		t.Fatal(err)
	}
	pgDBs.Store(db, true)
	return db
}

func dropLoc(t *testing.T, loc string) {
	if !strings.Contains(loc, "://") {
		os.Remove(loc)
		return
	}
	u, _ := url.Parse(loc)
	pgAdmin.Exec("DROP SCHEMA " + u.Query().Get("search_path") + " CASCADE")
}
