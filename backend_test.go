// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// The tests that matter most run on every database Bootstrapping supports:
// SQLite always, PostgreSQL when BS_POSTGRES names a server to test on,
// such as postgres://postgres@localhost:5432/postgres?sslmode=disable, and
// MySQL or MariaDB when BS_MYSQL does, such as mysql://root@127.0.0.1:3306/.
// Each PostgreSQL test gets a schema of its own and each MySQL test a
// database of its own, dropped afterwards. BS_BACKENDS (say "mysql")
// narrows the run to the backends it names.

type backend struct {
	name string
	pg   bool
	my   bool
}

// sqlite reports whether b is SQLite.
func (b backend) sqlite() bool { return !b.pg && !b.my }

var (
	pgURL = os.Getenv("BS_POSTGRES")
	myURL = os.Getenv("BS_MYSQL")
)

func backends() []backend {
	out := []backend{{name: "sqlite"}}
	if pgURL != "" {
		out = append(out, backend{name: "postgres", pg: true})
	}
	if myURL != "" {
		out = append(out, backend{name: "mysql", my: true})
	}
	if only := os.Getenv("BS_BACKENDS"); only != "" {
		var keep []backend
		for _, b := range out {
			if strings.Contains(","+only+",", ","+b.name+",") {
				keep = append(keep, b)
			}
		}
		out = keep
	}
	return out
}

// eachBackend runs f as a subtest per database.
func eachBackend(t *testing.T, f func(t *testing.T, b backend)) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) { f(t, b) })
	}
}

var (
	pgDBs     sync.Map // *sql.DB -> true for PostgreSQL handles, so helpers rebind ?
	myDBs     sync.Map // *sql.DB -> true for MySQL handles
	schemaSeq atomic.Int64
	pgAdmin   *sql.DB
	pgOnce    sync.Once
	myAdmin   *sql.DB
	myOnce    sync.Once
	myCollate string // a binary collation without trailing-space padding
)

// open is an empty database with foreign keys enforced.
func (b backend) open(t testing.TB) *sql.DB {
	t.Helper()
	if b.my {
		return openMy(t, newMyDB(t))
	}
	if !b.pg {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db")+"?_foreign_keys=1&_busy_timeout=30000")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	pgOnce.Do(func() {
		var err error
		if pgAdmin, err = sql.Open("postgres", pgURL); err != nil {
			panic(err)
		}
	})
	schema := fmt.Sprintf("bs_test_%d_%d", os.Getpid(), schemaSeq.Add(1))
	if _, err := pgAdmin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("creating a test schema on %s: %v", pgURL, err)
	}
	db, err := sql.Open("postgres", pgURLWith(schema))
	if err != nil {
		t.Fatal(err)
	}
	pgDBs.Store(db, true)
	t.Cleanup(func() {
		db.Close()
		pgDBs.Delete(db)
		pgAdmin.Exec("DROP SCHEMA " + schema + " CASCADE")
	})
	return db
}

// newMyDB makes a database for a MySQL test, dropped after it, and returns
// its mysql:// URL. Its default collation is binary and doesn't pad, so
// keys compare as the exact text, as the tests on other databases expect.
func newMyDB(t testing.TB) string {
	t.Helper()
	myOnce.Do(func() {
		cfg := myConfig(myURL, "")
		db, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			panic(err)
		}
		myAdmin = db
		var v string
		if err := db.QueryRow("SELECT VERSION()").Scan(&v); err != nil {
			panic(fmt.Sprintf("BS_MYSQL %s: %v", myURL, err))
		}
		myCollate = "utf8mb4_0900_bin"
		if strings.Contains(strings.ToLower(v), "mariadb") {
			myCollate = "utf8mb4_nopad_bin"
		}
	})
	name := fmt.Sprintf("bs_test_%d_%d", os.Getpid(), schemaSeq.Add(1))
	if _, err := myAdmin.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE " + myCollate); err != nil {
		t.Fatalf("creating a test database on %s: %v", myURL, err)
	}
	t.Cleanup(func() { myAdmin.Exec("DROP DATABASE IF EXISTS " + name) })
	u, err := url.Parse(myURL)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

// myConfig is the driver's configuration for a mysql:// URL, with the
// database replaced by db. Test connections use ANSI mode, so the tests'
// own SQL can quote names with " and join text with ||; Bootstrapping sets
// its own mode for its work.
func myConfig(loc, db string) *mysql.Config {
	u, err := url.Parse(loc)
	if err != nil {
		panic(err)
	}
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr, cfg.DBName = "tcp", u.Host, db
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	cfg.Params = map[string]string{"sql_mode": "'ANSI,STRICT_ALL_TABLES'"}
	cfg.MultiStatements = true // the tests run schemas of several statements
	return cfg
}

// openMy opens a test database by its mysql:// URL.
func openMy(t testing.TB, loc string) *sql.DB {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := mysql.NewConnector(myConfig(loc, strings.TrimPrefix(u.Path, "/")))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	myDBs.Store(db, true)
	t.Cleanup(func() {
		db.Close()
		myDBs.Delete(db)
	})
	return db
}

// pgURLWith is the test server's URL with search_path set to schema.
func pgURLWith(schema string) string {
	u, err := url.Parse(pgURL)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// ddl adapts a SQLite schema written for the tests to PostgreSQL or MySQL.
func (b backend) ddl(s string) string {
	if b.my {
		return strings.NewReplacer(
			"INTEGER PRIMARY KEY", "INTEGER NOT NULL AUTO_INCREMENT PRIMARY KEY",
			" TEXT", " VARCHAR(255)",
			" REAL", " DOUBLE",
			" BLOB", " LONGBLOB",
			" WITHOUT ROWID", "",
			" system ", ` "system" `, // a reserved word in MySQL
		).Replace(s)
	}
	if !b.pg {
		return s
	}
	return strings.NewReplacer(
		"INTEGER PRIMARY KEY", "BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY",
		" REAL", " DOUBLE PRECISION",
		" BLOB", " BYTEA",
		" WITHOUT ROWID", "",
	).Replace(s)
}

// rebind turns ? into $n for a PostgreSQL handle.
func rebind(db *sql.DB, q string) string {
	if _, ok := pgDBs.Load(db); !ok {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isPG(db *sql.DB) bool {
	_, ok := pgDBs.Load(db)
	return ok
}

func isMy(db *sql.DB) bool {
	_, ok := myDBs.Load(db)
	return ok
}

func isSQLite(db *sql.DB) bool { return !isPG(db) && !isMy(db) }

// resultName is the name of a test's results file: the PostgreSQL run's
// figures go next to SQLite's.
func resultName(name string, b backend) string {
	if b.pg {
		return name + "-postgres"
	}
	if b.my {
		return name + "-mysql"
	}
	return name
}
