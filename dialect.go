// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Bootstrapping works with SQLite and PostgreSQL. dbConn hides most of the
// difference: the SQL in this package is written with ? placeholders and
// SQLite's names, and dbConn rewrites it for PostgreSQL. What differs more
// than a placeholder (reading the schema, how values compare, locking) is
// done here, by dialect.

// querier is what *sql.DB and *sql.Conn offer.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type dbConn struct {
	q  querier
	pg bool
}

// newDBConn works out which database q talks to. It must run outside a
// transaction: on PostgreSQL a failed statement spoils the transaction it
// runs in.
func newDBConn(ctx context.Context, q querier) (*dbConn, error) {
	var v string
	if err := q.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err == nil {
		return &dbConn{q: q}, nil
	}
	if err := q.QueryRowContext(ctx, "SELECT current_setting('server_version_num')").Scan(&v); err == nil {
		n, _ := strconv.Atoi(v)
		if n < 120000 {
			return nil, fmt.Errorf("PostgreSQL %d.%d is too old; Bootstrapping needs 12 or later", n/10000, n%10000/100)
		}
		return &dbConn{q: q, pg: true}, nil
	}
	return nil, errors.New("the database is neither SQLite nor PostgreSQL, the two Bootstrapping supports")
}

func (d *dbConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.q.QueryContext(ctx, d.rebind(query), d.args(args)...)
}

func (d *dbConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.q.ExecContext(ctx, d.rebind(query), d.args(args)...)
}

func (d *dbConn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.q.QueryRowContext(ctx, d.rebind(query), d.args(args)...)
}

// rebind turns ? placeholders into $1, $2... for PostgreSQL, leaving quoted
// names and strings alone.
func (d *dbConn) rebind(query string) string {
	if !d.pg || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	n := 0
	var quote byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '?':
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// args turns Values into what the driver takes: Go values for SQLite, and
// text for PostgreSQL, which each placeholder casts to its column's type.
func (d *dbConn) args(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if v, ok := a.(Value); ok {
			if d.pg {
				out[i] = pgArg(v)
			} else {
				out[i] = v.Any()
			}
			continue
		}
		out[i] = a
	}
	return out
}

// ph is the placeholder for a value going into (or compared with) column
// c: on PostgreSQL the text it's sent as is cast to the column's type.
func (d *dbConn) ph(c colInfo) string {
	if !d.pg {
		return "?"
	}
	return "CAST(CAST(? AS text) AS " + c.declared + ")"
}

// eq compares a column with a placeholder. Keys are never NULL, so = does;
// on SQLite IS keeps the old statements, which also match NULL.
func (d *dbConn) eq(col string, c colInfo) string {
	if d.pg {
		return quoteIdent(col) + " = " + d.ph(c)
	}
	return quoteIdent(col) + " IS ?"
}

// begin starts the transaction a plan is made in. For an apply (write) it
// also takes the lock that keeps two applies from interleaving: SQLite's
// write lock, or on PostgreSQL an advisory lock taken before the
// transaction, so its snapshot sees what an apply before it committed. It
// returns the function that ends the transaction, by commit or rollback,
// and releases the lock.
func (d *dbConn) begin(ctx context.Context, write bool) (end func(commit bool) error, err error) {
	bg := context.WithoutCancel(ctx)
	if !d.pg {
		var after func()
		if write {
			// Wait for another writer rather than failing at once, unless the
			// connection already says how long to wait.
			var busy int
			if d.q.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy) == nil && busy == 0 {
				if _, err := d.q.ExecContext(ctx, "PRAGMA busy_timeout = 30000"); err == nil {
					after = func() { d.q.ExecContext(bg, "PRAGMA busy_timeout = 0") }
				}
			}
		}
		stmt := "BEGIN"
		if write {
			stmt = "BEGIN IMMEDIATE"
		}
		if _, err := d.q.ExecContext(ctx, stmt); err != nil {
			if after != nil {
				after()
			}
			return nil, fmt.Errorf("starting the transaction: %w", err)
		}
		return d.finisher(bg, after), nil
	}

	var unlock func()
	if write {
		const lock = "SELECT pg_advisory_lock(hashtext('bootstrapping:' || current_schema()))"
		if _, err := d.q.ExecContext(ctx, lock); err != nil {
			return nil, fmt.Errorf("waiting for another apply: %w", err)
		}
		unlock = func() {
			d.q.ExecContext(bg, "SELECT pg_advisory_unlock(hashtext('bootstrapping:' || current_schema()))")
		}
	}
	stmt := "BEGIN ISOLATION LEVEL REPEATABLE READ"
	if !write {
		stmt += " READ ONLY"
	}
	if _, err := d.q.ExecContext(ctx, stmt); err != nil {
		if unlock != nil {
			unlock()
		}
		return nil, fmt.Errorf("starting the transaction: %w", err)
	}
	// Values are compared as text, so the text has to be the same every
	// time, whatever the server's or the role's settings.
	for _, s := range []string{
		"SET LOCAL TimeZone = 'UTC'",
		"SET LOCAL DateStyle = 'ISO, YMD'",
		"SET LOCAL IntervalStyle = 'postgres'",
		"SET LOCAL bytea_output = 'hex'",
		"SET LOCAL extra_float_digits = 1",
	} {
		if _, err := d.q.ExecContext(ctx, s); err != nil {
			d.q.ExecContext(bg, "ROLLBACK")
			if unlock != nil {
				unlock()
			}
			return nil, err
		}
	}
	return d.finisher(bg, unlock), nil
}

func (d *dbConn) finisher(bg context.Context, after func()) func(bool) error {
	done := false
	return func(commit bool) error {
		if done {
			return nil
		}
		done = true
		var err error
		if commit {
			if _, err = d.q.ExecContext(bg, "COMMIT"); err != nil {
				d.q.ExecContext(bg, "ROLLBACK")
				err = fmt.Errorf("committing: %w", err)
			}
		} else {
			d.q.ExecContext(bg, "ROLLBACK")
		}
		if after != nil {
			after()
		}
		return err
	}
}

// tableExists reports whether a table of that name is visible, unqualified,
// to the queries Bootstrapping runs.
func (d *dbConn) tableExists(ctx context.Context, name string) (bool, error) {
	var n int
	var err error
	if d.pg {
		err = d.QueryRowContext(ctx, "SELECT count(*) FROM pg_catalog.pg_class WHERE oid = to_regclass(quote_ident(?)) AND relkind IN ('r', 'p')", name).Scan(&n)
	} else {
		err = d.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n)
	}
	return n > 0, err
}

// ---- PostgreSQL values ----
//
// On PostgreSQL every value is compared as the text the server prints for
// it, under the settings begin fixes: the database's own idea of equal for
// that type. Integer, floating-point and bytea columns become Integer, Real
// and Blob Values, so plans and the ledger show them as such; every other
// type is Text. Seed values are cast to the column's type by the server, so
// a seed's 7 in a numeric(5,2) column is "7.00", as stored.

// pgKind is the Value kind a PostgreSQL type's values take.
func pgKind(typname string) Kind {
	switch typname {
	case "int2", "int4", "int8":
		return Integer
	case "float4", "float8":
		return Real
	case "bytea":
		return Blob
	}
	return Text
}

// pgValue is a value as PostgreSQL printed it.
func pgValue(s sql.NullString, k Kind) (Value, error) {
	if !s.Valid {
		return NullValue, nil
	}
	switch k {
	case Integer:
		i, err := strconv.ParseInt(s.String, 10, 64)
		return IntValue(i), err
	case Real:
		f, err := strconv.ParseFloat(s.String, 64)
		return RealValue(f), err
	case Blob:
		b, err := hex.DecodeString(strings.TrimPrefix(s.String, `\x`))
		return BlobValue(b), err
	}
	return TextValue(s.String), nil
}

// pgArg is a Value as text PostgreSQL reads back as the same value.
func pgArg(v Value) any {
	switch v.Kind {
	case Null:
		return nil
	case Integer:
		return strconv.FormatInt(v.I, 10)
	case Real:
		return pgFloat(v.F)
	case Blob:
		return `\x` + hex.EncodeToString([]byte(v.S))
	}
	return v.S
}

func pgFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// pgSeedText is a value from a seed file as the text to cast to the
// column's type. A boolean going into a numeric column is 1 or 0, as on
// SQLite.
func pgSeedText(x any, c colInfo) (string, error) {
	switch t := x.(type) {
	case string:
		return t, nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return pgFloat(t), nil
	case bool:
		if c.category == 'N' {
			if t {
				return "1", nil
			}
			return "0", nil
		}
		return strconv.FormatBool(t), nil
	case time.Time:
		return seedTime(t), nil
	}
	return "", fmt.Errorf("unsupported value %v (%T): use a string, number, boolean or date", x, x)
}

// pgCast casts seed texts to a column's type on the server, all in one
// statement, and returns each one's value as stored. If one doesn't fit the
// type, the error says which.
func (d *dbConn) pgCast(ctx context.Context, c colInfo, texts []string) (map[string]Value, error) {
	out := make(map[string]Value, len(texts))
	if len(texts) == 0 {
		return out, nil
	}
	b, _ := json.Marshal(texts)
	q := "SELECT v, CAST(CAST(v AS " + c.declared + ") AS text) FROM json_array_elements_text(CAST(? AS json)) AS v"
	// A failed statement spoils a PostgreSQL transaction, so each attempt
	// runs in a savepoint it can be rolled back to.
	try := func(f func() error) error {
		if _, err := d.ExecContext(ctx, "SAVEPOINT bootstrapping_cast"); err != nil {
			return err
		}
		if err := f(); err != nil {
			d.ExecContext(ctx, "ROLLBACK TO SAVEPOINT bootstrapping_cast")
			d.ExecContext(ctx, "RELEASE SAVEPOINT bootstrapping_cast")
			return err
		}
		_, err := d.ExecContext(ctx, "RELEASE SAVEPOINT bootstrapping_cast")
		return err
	}
	err := try(func() error {
		rows, err := d.QueryContext(ctx, q, string(b))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var in string
			var s sql.NullString
			if err := rows.Scan(&in, &s); err != nil {
				return err
			}
			v, err := pgValue(s, c.kind)
			if err != nil {
				return err
			}
			out[in] = v
		}
		return rows.Err()
	})
	if err == nil {
		return out, nil
	}
	// Find the value at fault, for a message that names it.
	for _, t := range texts {
		e := try(func() error {
			var s sql.NullString
			return d.QueryRowContext(ctx, "SELECT CAST(CAST(CAST(? AS text) AS "+c.declared+") AS text)", t).Scan(&s)
		})
		if e != nil {
			return nil, &castError{text: t, err: e}
		}
	}
	return nil, err
}

type castError struct {
	text string
	err  error
}

func (e *castError) Error() string {
	return fmt.Sprintf("%q: %v", e.text, e.err)
}
