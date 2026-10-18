// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Kind is the storage class of a Value, the same five SQLite uses.
type Kind uint8

const (
	Null Kind = iota
	Integer
	Real
	Text
	Blob
)

// Value is one cell, in the form SQLite stores it. Values from seed files
// are converted with the column's type affinity first, so a seed's 1 and a
// stored "1" in a TEXT column compare equal, the way SQLite itself would
// have stored them.
type Value struct {
	Kind Kind
	I    int64
	F    float64
	S    string // Text, or the bytes of a Blob
}

// NullValue is SQL NULL.
var NullValue = Value{}

func IntValue(i int64) Value    { return Value{Kind: Integer, I: i} }
func RealValue(f float64) Value { return Value{Kind: Real, F: f} }
func TextValue(s string) Value  { return Value{Kind: Text, S: s} }
func BlobValue(b []byte) Value  { return Value{Kind: Blob, S: string(b)} }
func (v Value) IsNull() bool    { return v.Kind == Null }
func (v Value) Equal(w Value) bool {
	return v.Kind == w.Kind && v.I == w.I && sameFloat(v.F, w.F) && v.S == w.S
}

func sameFloat(a, b float64) bool {
	return a == b || (math.IsNaN(a) && math.IsNaN(b))
}

// Any returns the value as a database/sql argument.
func (v Value) Any() any {
	switch v.Kind {
	case Integer:
		return v.I
	case Real:
		return v.F
	case Text:
		return v.S
	case Blob:
		return []byte(v.S)
	}
	return nil
}

// String is the value as the plan and the command print it: NULL, a number,
// or a quoted string.
func (v Value) String() string {
	switch v.Kind {
	case Integer:
		return strconv.FormatInt(v.I, 10)
	case Real:
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	case Text:
		return strconv.Quote(v.S)
	case Blob:
		return "x'" + fmt.Sprintf("%x", v.S) + "'"
	}
	return "NULL"
}

// JSON is the value as plain JSON for -json output: null, a number, a
// string, or base64 for a blob.
func (v Value) JSON() any {
	switch v.Kind {
	case Integer:
		return v.I
	case Real:
		if math.IsInf(v.F, 0) || math.IsNaN(v.F) {
			return v.String()
		}
		return v.F
	case Text:
		return v.S
	case Blob:
		return base64.StdEncoding.EncodeToString([]byte(v.S))
	}
	return nil
}

// encode and decode give the lossless form kept in the ledger: "n", "i:42",
// "r:1.5", "t:text" or "b:base64".
func (v Value) encode() string {
	switch v.Kind {
	case Integer:
		return "i:" + strconv.FormatInt(v.I, 10)
	case Real:
		return "r:" + strconv.FormatFloat(v.F, 'g', -1, 64)
	case Text:
		return "t:" + v.S
	case Blob:
		return "b:" + base64.StdEncoding.EncodeToString([]byte(v.S))
	}
	return "n"
}

func decodeValue(s string) (Value, error) {
	if s == "n" {
		return NullValue, nil
	}
	if len(s) < 2 || s[1] != ':' {
		return NullValue, fmt.Errorf("bad stored value %q", s)
	}
	body := s[2:]
	switch s[0] {
	case 'i':
		i, err := strconv.ParseInt(body, 10, 64)
		return IntValue(i), err
	case 'r':
		f, err := strconv.ParseFloat(body, 64)
		return RealValue(f), err
	case 't':
		return TextValue(body), nil
	case 'b':
		b, err := base64.StdEncoding.DecodeString(body)
		return BlobValue(b), err
	}
	return NullValue, fmt.Errorf("bad stored value %q", s)
}

// fromDB turns what database/sql scanned into a Value.
func fromDB(x any) Value {
	switch t := x.(type) {
	case nil:
		return NullValue
	case int64:
		return IntValue(t)
	case int:
		return IntValue(int64(t))
	case float64:
		return RealValue(t)
	case bool:
		if t {
			return IntValue(1)
		}
		return IntValue(0)
	case string:
		return TextValue(t)
	case []byte:
		return BlobValue(bytes.Clone(t))
	case time.Time:
		return TextValue(t.UTC().Format(time.RFC3339Nano))
	}
	return TextValue(fmt.Sprint(x))
}

// Affinity is a column's SQLite type affinity, worked out from its declared
// type by the rules in section 3.1 of SQLite's datatype documentation.
type Affinity uint8

const (
	AffBlob Affinity = iota
	AffText
	AffNumeric
	AffInteger
	AffReal
)

func affinityOf(declared string) Affinity {
	t := strings.ToUpper(declared)
	switch {
	case strings.Contains(t, "INT"):
		return AffInteger
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return AffText
	case strings.Contains(t, "BLOB"), t == "":
		return AffBlob
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return AffReal
	}
	return AffNumeric
}

// fromSeed converts a value decoded from a TOML seed file the way SQLite
// would convert it on insert into a column with this affinity.
func fromSeed(x any, aff Affinity) (Value, error) {
	var v Value
	switch t := x.(type) {
	case string:
		v = TextValue(t)
	case int64:
		v = IntValue(t)
	case float64:
		if math.IsNaN(t) {
			return NullValue, nil // SQLite stores NaN as NULL
		}
		v = RealValue(t)
	case bool:
		v = IntValue(0)
		if t {
			v = IntValue(1)
		}
	case time.Time:
		v = TextValue(seedTime(t))
	default:
		return NullValue, fmt.Errorf("unsupported value %v (%T): use a string, number, boolean or date", x, x)
	}
	return applyAffinity(v, aff), nil
}

// canonical converts a seed value with fromSeed, except a real going into
// a TEXT column: SQLite's rendering of reals as text rounds in its own way,
// so SQLite itself does that one conversion.
func canonical(ctx context.Context, q querier, x any, aff Affinity) (Value, error) {
	v, err := fromSeed(x, aff)
	if err != nil || aff != AffText {
		return v, err
	}
	if f, ok := x.(float64); ok && !math.IsNaN(f) {
		rows, err := q.QueryContext(ctx, "SELECT CAST(? AS TEXT)", f)
		if err != nil {
			return NullValue, err
		}
		defer rows.Close()
		var s string
		if rows.Next() {
			if err := rows.Scan(&s); err != nil {
				return NullValue, err
			}
		}
		return TextValue(s), rows.Err()
	}
	return v, nil
}

// applyAffinity is SQLite's conversion on storage: TEXT columns turn
// numbers into text, NUMERIC and INTEGER columns turn numeric text into a
// number, REAL columns turn integers into reals.
func applyAffinity(v Value, aff Affinity) Value {
	switch aff {
	case AffText:
		switch v.Kind {
		case Integer, Real:
			return TextValue(textOfNumber(v))
		}
	case AffNumeric, AffInteger:
		switch v.Kind {
		case Text:
			if n, ok := numericText(v.S); ok {
				return n
			}
		case Real:
			if n, ok := exactInt(v.F); ok {
				return n
			}
		}
	case AffReal:
		switch v.Kind {
		case Integer:
			return RealValue(float64(v.I))
		case Text:
			if n, ok := numericText(v.S); ok {
				if n.Kind == Integer {
					return RealValue(float64(n.I))
				}
				return n
			}
		}
	}
	return v
}

// textOfNumber is how SQLite renders a number stored into a TEXT column:
// a real with 17 significant digits, always with a decimal point
// ("1.0", "1.0e+20"), and infinities as Inf and -Inf.
func textOfNumber(v Value) string {
	if v.Kind == Integer {
		return strconv.FormatInt(v.I, 10)
	}
	switch {
	case math.IsInf(v.F, 1):
		return "Inf"
	case math.IsInf(v.F, -1):
		return "-Inf"
	}
	s := strconv.FormatFloat(v.F, 'g', 17, 64)
	mant, exp, hasExp := strings.Cut(s, "e")
	if !strings.Contains(mant, ".") {
		mant += ".0"
	}
	if hasExp {
		return mant + "e" + exp
	}
	return mant
}

// exactInt is a real that SQLite would store as an integer under NUMERIC or
// INTEGER affinity: whole, and inside the 64-bit range.
func exactInt(f float64) (Value, bool) {
	if f != math.Trunc(f) || f < -9.223372036854775808e18 || f >= 9.223372036854775808e18 {
		return NullValue, false
	}
	return IntValue(int64(f)), true
}

// numericText reports whether s is a well-formed number SQLite would store
// as INTEGER or REAL under NUMERIC affinity, and the number. Leading and
// trailing spaces are allowed, as SQLite allows them.
func numericText(s string) (Value, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return NullValue, false
	}
	if i, err := strconv.ParseInt(t, 10, 64); err == nil {
		return IntValue(i), true
	}
	if strings.ContainsAny(t, "xXpP_") || strings.EqualFold(strings.TrimLeft(t, "+-"), "inf") ||
		strings.EqualFold(strings.TrimLeft(t, "+-"), "infinity") || strings.EqualFold(t, "nan") {
		return NullValue, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return NullValue, false
	}
	if v, ok := exactInt(f); ok {
		return v, true
	}
	return RealValue(f), true
}
