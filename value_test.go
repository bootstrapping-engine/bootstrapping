// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// TestAffinityMatchesSQLite stores seed-style values in columns of every
// declared type SQLite users write, and checks the engine's conversion
// gives exactly what SQLite stored. If they differed, a plan would see a
// change that isn't one, or miss one that is.
func TestAffinityMatchesSQLite(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "aff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA synchronous = OFF; PRAGMA journal_mode = MEMORY"); err != nil {
		t.Fatal(err)
	}
	types := []string{"INTEGER", "INT", "BIGINT", "TEXT", "VARCHAR(20)", "CLOB", "REAL", "DOUBLE", "FLOAT",
		"NUMERIC", "DECIMAL(10,2)", "BOOLEAN", "DATE", "DATETIME", "BLOB", ""}
	for i, ty := range types {
		if _, err := db.Exec(fmt.Sprintf("CREATE TABLE t%d (v %s)", i, ty)); err != nil {
			t.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(7))
	fixed := []any{
		"", " ", "0", "1", "-1", "+5", "007", " 42 ", "42 ", "1.0", "1.5", "-0.0", "1e3", "1E3", "2.5e-3", ".5", "5.",
		"9223372036854775807", "9223372036854775808", "-9223372036854775808", "1e19", "123456789012345678901234567890",
		"abc", "1abc", "0x10", "1_000", "inf", "NaN", "Infinity", "true", "2026-10-18", "１２",
		int64(0), int64(1), int64(-7), int64(math.MaxInt64), int64(math.MinInt64),
		0.0, 1.0, -1.0, 1.5, 0.1, 1e15, 1e16, 1e20, 123456789.123, -2.5e-8, 9.223372036854775e18,
		true, false,
		time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC),
	}
	var values []any
	values = append(values, fixed...)
	for i := 0; i < 400*scaleOf(); i++ {
		switch rng.Intn(5) {
		case 0:
			values = append(values, rng.Int63n(2000000)-1000000)
		case 1:
			values = append(values, (rng.Float64()-0.5)*math.Pow(10, float64(rng.Intn(24)-6)))
		case 2:
			values = append(values, strconv.FormatInt(rng.Int63n(100000)-50000, 10))
		case 3:
			values = append(values, strconv.FormatFloat((rng.Float64()-0.5)*1000, 'f', rng.Intn(5), 64))
		default:
			values = append(values, strconv.FormatFloat(rng.Float64()*1e6, 'e', rng.Intn(6), 64))
		}
	}
	checks := 0
	for i, ty := range types {
		aff := affinityOf(ty)
		for _, x := range values {
			want, err := canonical(context.Background(), &dbConn{q: db}, x, aff)
			if err != nil {
				t.Fatal(err)
			}
			arg := want.Any() // what the engine writes
			if v, ok := x.(time.Time); ok {
				arg = seedTime(v)
			}
			// Insert the raw seed value the way a driver would pass it, then read
			// back what SQLite stored.
			raw := x
			switch v := x.(type) {
			case bool:
				raw = map[bool]int64{true: 1, false: 0}[v]
			case time.Time:
				raw = seedTime(v)
			}
			if _, err := db.Exec(fmt.Sprintf("DELETE FROM t%d", i)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(fmt.Sprintf("INSERT INTO t%d (v) VALUES (?)", i), raw); err != nil {
				t.Fatal(err)
			}
			var stored any
			if err := db.QueryRow(fmt.Sprintf("SELECT +v FROM t%d", i)).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			got := fromDB(stored)
			if !got.Equal(want) {
				t.Errorf("%s column, seed value %#v: SQLite stored %s (%v), the engine expects %s (%v)", ty, x, got, got.Kind, want, want.Kind)
			}
			// And what the engine writes reads back as itself.
			if _, err := db.Exec(fmt.Sprintf("UPDATE t%d SET v = ?", i), arg); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(fmt.Sprintf("SELECT +v FROM t%d", i)).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if again := fromDB(stored); !again.Equal(want) {
				t.Errorf("%s column, value %s: written and read back as %s", ty, want, again)
			}
			checks += 2
		}
	}
	t.Logf("%d declared types, %d values, %d checks", len(types), len(values), checks)
	recordResult(t, "affinity", map[string]int{"declared_types": len(types), "values": len(values), "checks": checks})
}

func TestValueEncoding(t *testing.T) {
	vals := []Value{NullValue, IntValue(0), IntValue(math.MinInt64), RealValue(0.1), RealValue(math.Inf(-1)), RealValue(-0.0),
		TextValue(""), TextValue("t:x"), TextValue("café\nline"), BlobValue([]byte{0, 1, 255}), BlobValue(nil)}
	for _, v := range vals {
		d, err := decodeValue(v.encode())
		if err != nil || !d.Equal(v) {
			t.Errorf("%s: round trip gave %s, %v", v, d, err)
		}
	}
	key := []Value{TextValue("a, b"), IntValue(3)}
	if k, err := decodeKey(encodeKey(key)); err != nil || len(k) != 2 || !k[0].Equal(key[0]) || !k[1].Equal(key[1]) {
		t.Errorf("key round trip: %v %v", k, err)
	}
	m := map[string]Value{"b": IntValue(2), "a": TextValue("x")}
	if encodeVals(m) != `{"a":"t:x","b":"i:2"}` {
		t.Errorf("vals encoding isn't canonical: %s", encodeVals(m))
	}
	for _, bad := range []string{"", "x", "i:x", "q:1", "b:%%"} {
		if _, err := decodeValue(bad); err == nil {
			t.Errorf("decodeValue(%q) accepted it", bad)
		}
	}
}
