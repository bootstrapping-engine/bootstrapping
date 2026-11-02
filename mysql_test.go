// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	bs "github.com/bootstrapping-engine/bootstrapping"
	"github.com/go-sql-driver/mysql"
)

// Tests only MySQL and MariaDB need. They're skipped unless BS_MYSQL is set.

func myBackend(t *testing.T) backend {
	t.Helper()
	if myURL == "" {
		t.Skip("BS_MYSQL isn't set")
	}
	return backend{name: "mysql", my: true}
}

// openMyWith opens the database of an open test handle again, with other
// session variables, such as a user's own time zone or sql_mode.
func openMyWith(t *testing.T, db *sql.DB, params map[string]string) *sql.DB {
	t.Helper()
	cfg := myConfig(myURL, query1(t, db, "SELECT DATABASE()"))
	cfg.Params = params
	cfg.MultiStatements = true
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := sql.OpenDB(conn)
	t.Cleanup(func() { other.Close() })
	return other
}

const myTypesSchema = `
CREATE TABLE things (
	id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
	code VARCHAR(20) NOT NULL UNIQUE,
	i1 TINYINT, i4 INT, i8 BIGINT, u8 BIGINT UNSIGNED,
	num DECIMAL(30,10), num2 DECIMAL(10,2),
	f4 FLOAT, f8 DOUBLE,
	flag BOOLEAN,
	t TEXT, vc VARCHAR(10), ch CHAR(3),
	d DATE, dt DATETIME, ts TIMESTAMP NULL, tm TIME, dt6 DATETIME(6),
	j JSON, bin VARBINARY(10), bits BIT(8),
	e ENUM('sad', 'ok', 'happy'), st SET('a', 'b', 'c'), yr YEAR
)`

type myCase struct {
	col  string
	toml string // the value as written in the seed
	want string // the value as stored, read as text; empty to leave unchecked
}

var myCases = []myCase{
	{"i1", `7`, "7"}, {"i4", `"0042"`, "42"}, {"i8", `9007199254740993`, "9007199254740993"},
	{"i4", `true`, "1"}, {"u8", `"18446744073709551615"`, "18446744073709551615"},
	{"num", `1.5`, "1.5000000000"}, {"num2", `7`, "7.00"}, {"num2", `"3.14159"`, "3.14"},
	{"f4", `0.1`, ""}, {"f8", `0.1`, "0.1"}, {"f8", `1e300`, ""},
	{"flag", `true`, "1"}, {"flag", `"0"`, "0"},
	{"t", `"héllo, \"world\"\n"`, "héllo, \"world\"\n"}, {"t", `12`, "12"},
	{"vc", `"short"`, "short"}, {"ch", `"ab"`, "ab"},
	{"d", `2026-10-18`, "2026-10-18"},
	{"dt", `2026-10-18T09:30:00`, "2026-10-18 09:30:00"}, {"dt", `"2026-10-18 09:30"`, "2026-10-18 09:30:00"},
	{"ts", `2026-10-18T09:30:00+03:00`, ""}, {"ts", `"2026-10-18 06:30:00"`, ""},
	{"tm", `09:30:00`, "09:30:00"},
	{"dt6", `"2026-10-18 09:30:00.5"`, "2026-10-18 09:30:00.500000"},
	{"j", `"{\"b\": 1, \"a\": 2}"`, ""}, // MySQL normalizes JSON, MariaDB keeps the text
	{"bin", `"ab"`, "ab"},
	{"bits", `5`, ""},
	{"e", `"happy"`, "happy"}, {"st", `"b,a"`, "a,b"}, {"yr", `2026`, "2026"},
}

func TestMySQLTypes(t *testing.T) {
	b := myBackend(t)
	ctx := context.Background()
	db := b.open(t)
	mustExec(t, db, myTypesSchema)
	var rows strings.Builder
	rows.WriteString("table = \"things\"\nkey = \"code\"\n")
	for i, c := range myCases {
		fmt.Fprintf(&rows, "[[row]]\ncode = \"r%02d\"\n%s = %s\n", i, c.col, c.toml)
	}
	set := setFrom(t, map[string]string{"things.toml": rows.String()})
	if _, err := bs.Apply(ctx, db, set, nil); err != nil {
		t.Fatal(err)
	}
	for i, c := range myCases {
		if c.want == "" {
			continue
		}
		got := query1(t, db, fmt.Sprintf(`SELECT CAST("%s" AS CHAR) FROM things WHERE code = ?`, c.col), fmt.Sprintf("r%02d", i))
		if got != c.want {
			t.Errorf("%s = %s: stored %q, want %q", c.col, c.toml, got, c.want)
		}
	}
	if got := query1(t, db, `SELECT CAST(bits AS UNSIGNED) FROM things WHERE bits IS NOT NULL`); got != "5" {
		t.Errorf("bits: %s", got)
	}
	// Planned again, nothing has changed, whatever the session's settings.
	for _, params := range []map[string]string{
		nil,
		{"time_zone": "'+05:00'"},
		{"sql_mode": "''"},
		{"sql_mode": "'ANSI_QUOTES'", "time_zone": "'-11:00'"},
		{"sql_mode": "'NO_BACKSLASH_ESCAPES,PAD_CHAR_TO_FULL_LENGTH'"},
	} {
		odd := db
		if params != nil {
			odd = openMyWith(t, db, params)
		}
		p, err := bs.Preview(ctx, odd, set, nil)
		if err != nil {
			t.Fatalf("%v: %v", params, err)
		}
		if p.Pending() {
			t.Fatalf("%v: values look changed: %s", params, actions(p))
		}
	}
	// An admin writing the same value another way isn't an edit.
	mustExec(t, db, "UPDATE things SET num2 = 7.000 WHERE num2 = 7")
	mustExec(t, db, "UPDATE things SET st = 'a,b,a' WHERE st IS NOT NULL")
	if p, err := bs.Preview(ctx, db, set, nil); err != nil || p.Pending() {
		t.Fatalf("after rewriting the same values: %v %s", err, actions(p))
	}
	// A real edit is one.
	mustExec(t, db, "UPDATE things SET num2 = 7.01 WHERE num2 = 7")
	p, err := bs.Preview(ctx, db, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(p); !strings.Contains(got, "keep-edit things/r06.num2") {
		t.Fatalf("an edit wasn't seen: %s", got)
	}
}

func TestMySQLBadValueSaysWhere(t *testing.T) {
	b := myBackend(t)
	db := b.open(t)
	mustExec(t, db, myTypesSchema)
	set := setFrom(t, map[string]string{"things.toml": "table = \"things\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\ni4 = 1\n[[row]]\ncode = \"b\"\ni4 = \"many\"\n"})
	_, err := bs.Preview(context.Background(), db, set, nil)
	if err == nil || !strings.HasPrefix(err.Error(), `things.toml: row 2, column "i4": "many": `) {
		t.Fatalf("got %v", err)
	}
}

// A value that doesn't fit its column stops the plan instead of being
// stored cut short or changed, which MySQL does with a warning outside
// strict mode.
func TestMySQLValueTooLong(t *testing.T) {
	b := myBackend(t)
	db := b.open(t)
	mustExec(t, db, `CREATE TABLE r (code VARCHAR(3) PRIMARY KEY, name VARCHAR(3), n TINYINT, e ENUM('x', 'y'))`)
	for _, row := range []string{
		"code = \"abc\"\nname = \"abcdef\"",
		"code = \"abcX\"",
		"code = \"abc\"\nn = 300",
		"code = \"abc\"\ne = \"z\"",
	} {
		set := setFrom(t, map[string]string{"r.toml": "table = \"r\"\nkey = \"code\"\n[[row]]\n" + row + "\n"})
		if _, err := bs.Preview(context.Background(), db, set, nil); err == nil || !strings.Contains(err.Error(), "row 1, column") {
			t.Errorf("%q: got %v", row, err)
		}
	}
	// What fits is fine.
	f := &fixture{t: t, db: db, ctx: context.Background()}
	expect(t, f.agree(map[string]string{"r.toml": "table = \"r\"\nkey = \"code\"\n[[row]]\ncode = \"ab\"\nname = \"xyz\"\nn = -5\ne = \"y\"\n"}), "insert r/ab")
}

// An apply puts the session back as it found it: the connection goes back
// to the application's pool.
func TestMySQLSessionRestored(t *testing.T) {
	b := myBackend(t)
	db := b.open(t)
	mustExec(t, db, b.ddl(promiseSchema))
	one := openMyWith(t, db, map[string]string{"sql_mode": "'ANSI_QUOTES'", "time_zone": "'+05:00'"})
	one.SetMaxOpenConns(1)
	set := setFrom(t, map[string]string{"items.toml": "table = \"items\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\na = \"x\"\n"})
	for i := 0; i < 2; i++ { // an apply that writes, then one with nothing to do
		if _, err := bs.Apply(context.Background(), one, set, nil); err != nil {
			t.Fatal(err)
		}
		if got := query1(t, one, "SELECT CONCAT(@@SESSION.sql_mode, ' ', @@SESSION.time_zone)"); got != "ANSI_QUOTES +05:00" {
			t.Fatalf("apply %d left the session as %q", i+1, got)
		}
	}
	if _, err := bs.Preview(context.Background(), one, set, nil); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, one, "SELECT IS_USED_LOCK(CONCAT('bootstrapping:', SHA1(DATABASE())))"); got != "NULL" {
		t.Fatalf("the apply lock is still held: %q", got)
	}
}

func TestMySQLConcurrentApplies(t *testing.T) {
	b := myBackend(t)
	db := b.open(t)
	mustExec(t, db, b.ddl(promiseSchema))
	var rows strings.Builder
	rows.WriteString("table = \"items\"\nkey = \"code\"\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&rows, "[[row]]\ncode = \"c%03d\"\na = \"x\"\nb = %d\n", i, i)
	}
	set := setFrom(t, map[string]string{"items.toml": rows.String()})
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan *bs.Plan, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p, err := bs.Apply(context.Background(), db, set, nil)
			if err != nil {
				errs <- err
				return
			}
			results <- p
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	wrote := 0
	for p := range results {
		if p.Applied {
			wrote++
		}
	}
	if wrote != 1 {
		t.Fatalf("%d applies wrote, want exactly 1", wrote)
	}
	if got := query1(t, db, "SELECT CONCAT(count(*), ' ', count(DISTINCT code)) FROM items"); got != "500 500" {
		t.Fatalf("items: %s", got)
	}
	if got := query1(t, db, "SELECT count(*) FROM _bootstrapping_runs"); got != "1" {
		t.Fatalf("runs: %s", got)
	}
}

// An admin who edits a row while an apply runs waits for the apply to
// finish, so the apply never writes over an edit it didn't plan with; the
// edit lands after it and the next plan keeps it.
func TestMySQLEditDuringApply(t *testing.T) {
	b := myBackend(t)
	ctx := context.Background()
	db := b.open(t)
	mustExec(t, db, b.ddl(promiseSchema))
	v1 := setFrom(t, map[string]string{"items.toml": "table = \"items\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\na = \"x\"\n[[row]]\ncode = \"z\"\na = \"x\"\n"})
	if _, err := bs.Apply(ctx, db, v1, nil); err != nil {
		t.Fatal(err)
	}
	v2 := setFrom(t, map[string]string{"items.toml": "table = \"items\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\na = \"y\"\n[[row]]\ncode = \"z\"\na = \"y\"\n"})
	other := openMyWith(t, db, nil)
	var applyDone, editDone time.Time
	var editErr error
	var wg sync.WaitGroup
	started := false
	opts := &bs.Options{Progress: func(c bs.Change) {
		if started { // after the first write (row a), an admin edits row z
			return
		}
		started = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, editErr = other.Exec("UPDATE items SET a = 'admin' WHERE code = 'z'")
			editDone = time.Now()
		}()
		time.Sleep(300 * time.Millisecond) // long enough for the edit to reach the server
	}}
	if _, err := bs.Apply(ctx, db, v2, opts); err != nil {
		t.Fatal(err)
	}
	applyDone = time.Now()
	wg.Wait()
	var me *mysql.MySQLError
	switch {
	case editErr == nil:
		if editDone.Before(applyDone) {
			t.Fatal("the admin's edit didn't wait for the apply")
		}
		if got := query1(t, db, "SELECT a FROM items WHERE code = 'z'"); got != "admin" {
			t.Fatalf("z after both: %s", got)
		}
		p, err := bs.Preview(ctx, db, v2, nil)
		if err != nil || !strings.Contains(actions(p), "keep-edit items/z.a") {
			t.Fatalf("the edit isn't kept: %v %s", err, actions(p))
		}
	case errors.As(editErr, &me) && me.Number == 1213:
		// InnoDB chose the admin's statement as a deadlock victim: the edit
		// failed loudly, and the apply's write stands.
		if got := query1(t, db, "SELECT a FROM items WHERE code = 'z'"); got != "y" {
			t.Fatalf("z: %s", got)
		}
	default:
		t.Fatalf("the admin's edit: %v", editErr)
	}
}

// Names MySQL reserves work as table and column names.
func TestMySQLReservedNames(t *testing.T) {
	b := myBackend(t)
	db := b.open(t)
	mustExec(t, db, "CREATE TABLE \"order\" (id INTEGER NOT NULL AUTO_INCREMENT PRIMARY KEY, \"key\" VARCHAR(20) NOT NULL UNIQUE, \"group\" VARCHAR(20), \"select\" INTEGER)")
	mustExec(t, db, "CREATE TABLE \"limit\" (\"key\" VARCHAR(20) PRIMARY KEY, order_id INTEGER, retired_at DATETIME)")
	f := &fixture{t: t, db: db, ctx: context.Background()}
	seeds := map[string]string{
		"order.toml": "table = \"order\"\nkey = \"key\"\n[[row]]\nkey = \"a\"\ngroup = \"g\"\nselect = 1\n[[row]]\nkey = \"b\"\n",
		"limit.toml": "table = \"limit\"\nkey = \"key\"\non_remove = { retire = \"retired_at\" }\n[refs]\norder_id = \"order.key\"\n" +
			"[[row]]\nkey = \"x\"\norder_id = \"a\"\n[[row]]\nkey = \"y\"\norder_id = \"b\"\n",
	}
	expect(t, f.agree(seeds), "insert order/a; insert order/b; insert limit/x; insert limit/y")
	seeds["limit.toml"] = strings.Replace(seeds["limit.toml"], "[[row]]\nkey = \"y\"\norder_id = \"b\"\n", "", 1)
	expect(t, f.agree(seeds), "retire limit/y")
	if got := f.one("SELECT CAST(retired_at AS CHAR) FROM \"limit\" WHERE \"key\" = 'y'"); got != "2026-11-02 08:00:00" {
		t.Fatalf("retired at: %s", got)
	}
}

func TestMySQLRetireColumnType(t *testing.T) {
	b := myBackend(t)
	db := b.open(t)
	mustExec(t, db, `CREATE TABLE r (code VARCHAR(10) PRIMARY KEY, retired BOOLEAN); CREATE TABLE s (code VARCHAR(10) PRIMARY KEY, retired DATE)`)
	set := setFrom(t, map[string]string{"r.toml": "table = \"r\"\nkey = \"code\"\non_remove = { retire = \"retired\" }\n[[row]]\ncode = \"a\"\n"})
	if _, err := bs.Preview(context.Background(), db, set, nil); err == nil || !strings.Contains(err.Error(), "can't hold the time a row was retired") {
		t.Fatalf("got %v", err)
	}
	f := &fixture{t: t, db: db, ctx: context.Background()}
	seed := func(rows string) map[string]string {
		return map[string]string{"s.toml": "table = \"s\"\nkey = \"code\"\non_remove = { retire = \"retired\" }\n" + rows}
	}
	f.agree(seed("[[row]]\ncode = \"a\"\n[[row]]\ncode = \"b\"\n"))
	expect(t, f.agree(seed("[[row]]\ncode = \"a\"\n")), "retire s/b")
	if got := f.one("SELECT CAST(retired AS CHAR) FROM s WHERE code = 'b'"); got != "2026-11-02" {
		t.Fatalf("retired on the apply's day: %s", got)
	}
}

// A table without transactions can't be applied to all or nothing, so it's
// refused before anything is written.
func TestMySQLNonTransactionalTable(t *testing.T) {
	b := myBackend(t)
	db := b.open(t)
	mustExec(t, db, `CREATE TABLE r (code VARCHAR(10) PRIMARY KEY, n INTEGER) ENGINE = MyISAM`)
	set := setFrom(t, map[string]string{"r.toml": "table = \"r\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\nn = 1\n"})
	if _, err := bs.Apply(context.Background(), db, set, nil); err == nil || !strings.Contains(err.Error(), "has no transactions") {
		t.Fatalf("got %v", err)
	}
	if got := query1(t, db, "SELECT count(*) FROM r"); got != "0" {
		t.Fatalf("rows: %s", got)
	}
}
