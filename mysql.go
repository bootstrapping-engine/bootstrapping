// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- MySQL and MariaDB ----
//
// MySQL 8.0+ and MariaDB 10.6+ are handled the way PostgreSQL is: each
// value is compared as the text the server prints for it (CAST(col AS CHAR),
// or HEX for binary types), under session settings begin fixes, and seed
// values are converted by the server itself, by storing them in a temporary
// table that has the column's own type. The package's SQL is written with
// double-quoted names; rebind turns those into backticks, so no sql_mode
// with ANSI_QUOTES is needed.

// mysqlMode is the sql_mode a plan or an apply runs under: strict, so a
// value that doesn't fit its column is an error rather than a warning and a
// silently changed value; and 0 written into an AUTO_INCREMENT column is 0.
const mysqlMode = "STRICT_ALL_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_AUTO_VALUE_ON_ZERO,NO_BACKSLASH_ESCAPES,NO_ENGINE_SUBSTITUTION"

// mysqlLockName is the lock two applies to the same database take turns
// on; GET_LOCK names are at most 64 characters.
const mysqlLockName = "CONCAT('bootstrapping:', SHA1(COALESCE(DATABASE(), '')))"

// probeMySQL reports whether q talks to MySQL 8.0+ or MariaDB 10.6+.
func probeMySQL(ctx context.Context, q querier) (bool, error) {
	var v string
	if err := q.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return false, nil
	}
	var db sql.NullString
	if err := q.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&db); err != nil {
		return false, nil
	}
	major, minor := versionParts(v)
	if strings.Contains(strings.ToLower(v), "mariadb") {
		if major < 10 || major == 10 && minor < 6 {
			return false, fmt.Errorf("MariaDB %s is too old; Bootstrapping needs 10.6 or later", v)
		}
	} else if major < 8 {
		return false, fmt.Errorf("MySQL %s is too old; Bootstrapping needs 8.0 or later", v)
	}
	if !db.Valid {
		return false, fmt.Errorf("no database selected: name one in the connection (user:pass@tcp(host)/dbname)")
	}
	return true, nil
}

func versionParts(v string) (int, int) {
	parts := strings.SplitN(v, ".", 3)
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	return major, minor
}

// mysqlIdents turns the package's double-quoted names into backticks,
// leaving string literals alone.
func mysqlIdents(query string) string {
	if !strings.Contains(query, `"`) {
		return query
	}
	var b strings.Builder
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch c {
		case '\'':
			j := i + 1
			for j < len(query) {
				if query[j] == '\'' {
					if j+1 < len(query) && query[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			b.WriteString(query[i:min(j+1, len(query))])
			i = j
		case '"':
			b.WriteByte('`')
			j := i + 1
			for j < len(query) {
				if query[j] == '"' {
					if j+1 < len(query) && query[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				if query[j] == '`' {
					b.WriteString("``")
				} else {
					b.WriteByte(query[j])
				}
				j++
			}
			b.WriteByte('`')
			i = j
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// beginMySQL is begin for MySQL and MariaDB: fixed session settings, the
// apply lock, and one REPEATABLE READ transaction. Everything is put back
// when the transaction ends.
func (d *dbConn) beginMySQL(ctx context.Context, write bool) (func(bool) error, error) {
	bg := context.WithoutCancel(ctx)
	var mode, zone string
	if err := d.q.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode, @@SESSION.time_zone").Scan(&mode, &zone); err != nil {
		return nil, err
	}
	restore := func() {
		d.q.ExecContext(bg, "SET SESSION sql_mode = "+sqlString(mode)+", time_zone = "+sqlString(zone))
	}
	if _, err := d.q.ExecContext(ctx, "SET SESSION sql_mode = '"+mysqlMode+"', time_zone = '+00:00'"); err != nil {
		restore()
		return nil, err
	}
	after := restore
	if write {
		var got sql.NullInt64
		if err := d.q.QueryRowContext(ctx, "SELECT GET_LOCK("+mysqlLockName+", 31536000)").Scan(&got); err != nil || got.Int64 != 1 {
			restore()
			if err == nil {
				err = errors.New("timed out")
			}
			return nil, fmt.Errorf("waiting for another apply: %w", err)
		}
		after = func() {
			d.q.ExecContext(bg, "SELECT RELEASE_LOCK("+mysqlLockName+")")
			restore()
		}
	}
	if _, err := d.q.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		after()
		return nil, err
	}
	// A plan isn't READ ONLY: converting seed values writes a temporary
	// table, which a read-only transaction may not create.
	stmt := "START TRANSACTION"
	if !write {
		stmt = "START TRANSACTION WITH CONSISTENT SNAPSHOT"
	}
	if _, err := d.q.ExecContext(ctx, stmt); err != nil {
		after()
		return nil, fmt.Errorf("starting the transaction: %w", err)
	}
	d.lockReads = write
	return d.finisher(bg, func() {
		d.lockReads = false
		d.q.ExecContext(bg, "DROP TEMPORARY TABLE IF EXISTS "+mysqlCastTable)
		after()
	}), nil
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// mysqlLedgerSchema is the ledger for MySQL: no TEXT in keys, and binary
// collations, so keys compare as the bytes they are. It is created before
// the apply's transaction, since DDL commits implicitly there.
var mysqlLedgerSchema = []string{
	`CREATE TABLE IF NOT EXISTS ` + rowsTable + ` (
		tbl     VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		"key"   VARBINARY(2816) NOT NULL,
		vals    LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		retired INTEGER NOT NULL DEFAULT 0,
		run     BIGINT NOT NULL,
		PRIMARY KEY (tbl, "key")
	) ENGINE = InnoDB`,
	`CREATE TABLE IF NOT EXISTS ` + runsTable + ` (
		id        BIGINT NOT NULL PRIMARY KEY,
		at        VARCHAR(64) NOT NULL,
		seed_hash VARCHAR(128) NOT NULL,
		version   VARCHAR(64) NOT NULL,
		inserts   BIGINT NOT NULL,
		updates   BIGINT NOT NULL,
		deletes   BIGINT NOT NULL,
		retires   BIGINT NOT NULL,
		kept      BIGINT NOT NULL,
		resolved  BIGINT NOT NULL
	) ENGINE = InnoDB`,
	`CREATE TABLE IF NOT EXISTS ` + changesTable + ` (
		run    BIGINT NOT NULL,
		seq    BIGINT NOT NULL,
		tbl    VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		"key"  VARBINARY(2816) NOT NULL,
		action VARCHAR(32) NOT NULL,
		col    VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
		old    LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin,
		new    LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin,
		note   TEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		PRIMARY KEY (run, seq),
		KEY bootstrapping_changes_row (tbl, "key")
	) ENGINE = InnoDB`,
	`CREATE TABLE IF NOT EXISTS ` + refsTable + ` (
		tbl VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		col VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		ref TEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
		PRIMARY KEY (tbl, col)
	) ENGINE = InnoDB`,
}

// readTableInfoMySQL reads a table's columns from information_schema.
func readTableInfoMySQL(ctx context.Context, q *dbConn, table string) (*tableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, DATA_TYPE, IS_NULLABLE,
			COLUMN_DEFAULT IS NOT NULL, EXTRA, COLUMN_KEY,
			COALESCE(CHARACTER_SET_NAME, ''), COALESCE(COLLATION_NAME, '')
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ti := &tableInfo{name: table, cols: map[string]colInfo{}}
	var pks []colInfo
	for rows.Next() {
		var c colInfo
		var dataType, nullable, extra, key, charset, collation string
		var hasDefault bool
		if err := rows.Scan(&c.name, &c.declared, &dataType, &nullable, &hasDefault, &extra, &key, &charset, &collation); err != nil {
			return nil, err
		}
		dataType = strings.ToLower(dataType)
		extra = strings.ToUpper(extra)
		c.notNull = nullable == "NO"
		c.hasDflt = hasDefault || strings.Contains(extra, "AUTO_INCREMENT")
		c.always = strings.Contains(extra, "GENERATED") && !strings.Contains(extra, "DEFAULT_GENERATED")
		c.dataType = dataType
		c.kind, c.category = mysqlKind(dataType, c.declared)
		if charset != "" {
			c.charset = " CHARACTER SET " + charset
			if collation != "" {
				c.charset += " COLLATE " + collation
			}
		}
		if key == "PRI" {
			c.pk = 1
			pks = append(pks, c)
		}
		ti.cols[c.name] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ti.cols) == 0 {
		return nil, fmt.Errorf("table %q doesn't exist in the database (create it with your migrations first)", table)
	}
	// An apply is all or nothing only on a transactional engine; MyISAM,
	// MEMORY and the like keep what was written before a failure. A view
	// has no engine and writes through to its table's.
	var engine, transactional sql.NullString
	err = q.QueryRowContext(ctx, `SELECT t.ENGINE, e.TRANSACTIONS FROM information_schema.TABLES t
		LEFT JOIN information_schema.ENGINES e ON e.ENGINE = t.ENGINE
		WHERE t.TABLE_SCHEMA = DATABASE() AND t.TABLE_NAME = ?`, table).Scan(&engine, &transactional)
	if err != nil {
		return nil, err
	}
	if engine.Valid && transactional.String != "YES" {
		return nil, fmt.Errorf("table %q uses the %s engine, which has no transactions; Bootstrapping needs InnoDB (or another transactional engine) so an apply is all or nothing", table, engine.String)
	}
	if len(pks) == 1 {
		ti.idCol = pks[0].name // no rowid: a referenced table needs a one-column primary key
	}
	return ti, nil
}

// mysqlKind is the Value kind of a column's values and whether the type is
// numeric. BIGINT UNSIGNED can exceed int64, so it stays text.
func mysqlKind(dataType, columnType string) (Kind, byte) {
	switch dataType {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bit":
		return Integer, 'N'
	case "bigint":
		if strings.Contains(strings.ToLower(columnType), "unsigned") {
			return Text, 'N'
		}
		return Integer, 'N'
	case "float", "double", "real":
		return Real, 'N'
	case "decimal", "numeric", "dec", "fixed":
		return Text, 'N'
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return Blob, 0
	}
	return Text, 0
}

// mysqlRead is the expression a column is read as: its value as text,
// as hex for binary types, as a number for BIT.
func mysqlRead(expr string, c colInfo) string {
	switch {
	case c.kind == Blob:
		return "HEX(" + expr + ")"
	case c.dataType == "bit":
		return "CAST(" + expr + " AS UNSIGNED)"
	}
	return "CAST(" + expr + " AS CHAR)"
}

// mysqlValue is a value as mysqlRead printed it.
func mysqlValue(s sql.NullString, k Kind) (Value, error) {
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
		b, err := hex.DecodeString(s.String)
		return BlobValue(b), err
	}
	return TextValue(s.String), nil
}

// mysqlSeedText is a value from a seed file as the text to store in the
// column. A boolean in a numeric column is 1 or 0; a date-time is written
// the way MySQL and MariaDB both read it, in UTC when it has an offset.
func mysqlSeedText(x any, c colInfo) (string, error) {
	switch t := x.(type) {
	case string:
		return t, nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	case csvText:
		if b, ok := t.boolean(); ok && c.category == 'N' {
			return mysqlSeedText(b, c)
		}
		return string(t), nil
	case bool:
		if c.category == 'N' {
			if t {
				return "1", nil
			}
			return "0", nil
		}
		return strconv.FormatBool(t), nil
	case time.Time:
		switch t.Location().String() {
		case "date-local":
			return t.Format("2006-01-02"), nil
		case "time-local":
			return t.Format("15:04:05.999999"), nil
		case "datetime-local":
			return t.Format("2006-01-02 15:04:05.999999"), nil
		}
		return t.UTC().Format("2006-01-02 15:04:05.999999"), nil
	}
	return "", fmt.Errorf("unsupported value %v (%T): use a string, number, boolean or date", x, x)
}

const mysqlCastTable = "_bootstrapping_cast"

// mysqlCast stores seed texts in a temporary table whose column has c's own
// type, character set and collation, in strict mode, and reads them back
// as stored. A value that doesn't fit is an error that names it, so nothing
// is ever stored shortened or changed by a warning.
func (d *dbConn) mysqlCast(ctx context.Context, c colInfo, texts []string) (map[string]Value, error) {
	out := make(map[string]Value, len(texts))
	if len(texts) == 0 {
		return out, nil
	}
	if _, err := d.ExecContext(ctx, "DROP TEMPORARY TABLE IF EXISTS "+mysqlCastTable); err != nil {
		return nil, err
	}
	if _, err := d.ExecContext(ctx, "CREATE TEMPORARY TABLE "+mysqlCastTable+" (n INT NOT NULL PRIMARY KEY, v "+c.declared+c.charset+" NULL)"); err != nil {
		return nil, fmt.Errorf("a temporary column of type %s: %w", c.declared, err)
	}
	ph := "?"
	if c.dataType == "bit" {
		ph = "CAST(? AS UNSIGNED)"
	}
	insert := func(from int, list []string) error {
		marks := make([]string, len(list))
		args := make([]any, 0, 2*len(list))
		for i, s := range list {
			marks[i] = "(?, " + ph + ")"
			args = append(args, from+i, s)
		}
		_, err := d.ExecContext(ctx, "INSERT INTO "+mysqlCastTable+" (n, v) VALUES "+strings.Join(marks, ", "), args...)
		return err
	}
	const batch = 500
	for from := 0; from < len(texts); from += batch {
		list := texts[from:min(from+batch, len(texts))]
		if err := insert(from, list); err != nil {
			// Find the value at fault, for a message that names it.
			for i, s := range list {
				d.ExecContext(ctx, "DELETE FROM "+mysqlCastTable+" WHERE n = ?", from+i)
				if e := insert(from+i, []string{s}); e != nil {
					return nil, &castError{text: s, err: e}
				}
			}
			return nil, err
		}
	}
	rows, err := d.QueryContext(ctx, "SELECT n, "+mysqlRead("v", c)+" FROM "+mysqlCastTable+" ORDER BY n")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		var s sql.NullString
		if err := rows.Scan(&n, &s); err != nil {
			return nil, err
		}
		v, err := mysqlValue(s, c.kind)
		if err != nil {
			return nil, err
		}
		out[texts[n]] = v
	}
	return out, rows.Err()
}
