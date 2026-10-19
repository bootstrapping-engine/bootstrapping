// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// readCSVRows reads the rows a seed file keeps in a CSV file:
//
//	rows = "countries.csv"
//	rows = { file = "countries.csv", delimiter = ";", null = "\\N" }
//
// The first line names the columns. Every value is text, which the column's
// type converts the way the database would; a cell that equals null (empty
// by default) is NULL. A _renamed_from column gives a row's old key. It
// returns the rows and where each one was written, for messages.
func readCSVRows(spec any, read func(string) ([]byte, error)) ([]map[string]any, []string, error) {
	file, delim, null := "", ',', ""
	switch s := spec.(type) {
	case string:
		file = s
	case map[string]any:
		for k, v := range s {
			str, ok := v.(string)
			if !ok {
				return nil, nil, fmt.Errorf("rows: %s must be a string", k)
			}
			switch k {
			case "file":
				file = str
			case "delimiter":
				r, size := utf8.DecodeRuneInString(str)
				if size == 0 || size != len(str) || r == '"' || r == '\r' || r == '\n' || r == utf8.RuneError {
					return nil, nil, errors.New(`rows: delimiter must be one character, not a quote or a line break`)
				}
				delim = r
			case "null":
				null = str
			default:
				return nil, nil, fmt.Errorf("rows: unknown setting %q (expected file, delimiter and null)", k)
			}
		}
	default:
		return nil, nil, errors.New(`rows must be "file.csv" or { file = "file.csv", delimiter = ",", null = "" }`)
	}
	if file == "" {
		return nil, nil, errors.New(`rows: missing file = "file.csv"`)
	}
	clean := path.Clean(file)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(file, `\`) {
		return nil, nil, fmt.Errorf("rows: %q must be a path inside the seed directory, written with /", file)
	}
	data, err := read(clean)
	if err != nil {
		return nil, nil, fmt.Errorf("rows: %w", err)
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // a byte order mark
	if !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("%s: not UTF-8 text", clean)
	}

	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = delim
	header, err := r.Read()
	if err == io.EOF {
		return nil, nil, fmt.Errorf("%s: empty; the first line must name the columns", clean)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", clean, err)
	}
	seen := map[string]bool{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, nil, fmt.Errorf("%s: line 1, column %d has no name", clean, i+1)
		}
		if seen[h] {
			return nil, nil, fmt.Errorf("%s: line 1 names column %q twice", clean, h)
		}
		seen[h] = true
		header[i] = h
	}

	var rows []map[string]any
	var wheres []string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) && errors.Is(pe.Err, csv.ErrFieldCount) {
				return nil, nil, fmt.Errorf("%s: line %d has %d values, the header names %d columns", clean, pe.StartLine, len(rec), len(header))
			}
			return nil, nil, fmt.Errorf("%s: %w", clean, err)
		}
		line, _ := r.FieldPos(0)
		row := map[string]any{}
		for i, cell := range rec {
			if header[i] == "_renamed_from" {
				if cell != "" && cell != null {
					row["_renamed_from"] = csvText(cell)
				}
				continue
			}
			if cell != null {
				row[header[i]] = csvText(cell)
			}
		}
		rows = append(rows, row)
		wheres = append(wheres, fmt.Sprintf("%s: line %d", clean, line))
	}
	return rows, wheres, nil
}

// csvText is a value from a CSV cell. It's text, as a TOML string is, except
// that true and false in a numeric column are 1 and 0, as TOML's true and
// false are, so moving rows between TOML and CSV changes nothing.
type csvText string

func (c csvText) boolean() (bool, bool) {
	switch strings.ToLower(string(c)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}
