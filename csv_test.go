// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping_test

import (
	"strings"
	"testing"
	"testing/fstest"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

const countriesSchema = `
CREATE TABLE currencies (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, digits INTEGER);
CREATE TABLE countries (id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, name TEXT, population INTEGER,
	area REAL, currency_id INTEGER REFERENCES currencies (id), note TEXT);`

func TestCSVRows(t *testing.T) {
	eachBackend(t, testCSVRows)
}

func testCSVRows(t *testing.T, b backend) {
	f := fixtureOn(t, b, countriesSchema)
	files := map[string]string{
		"currencies.toml": "table = \"currencies\"\nkey = \"code\"\nrows = \"currencies.csv\"\n",
		"currencies.csv":  "\xef\xbb\xbfcode,digits\nEUR,2\nJPY,0\n",
		"countries.toml":  "table = \"countries\"\nkey = \"code\"\non_remove = \"delete\"\nrows = \"data/countries.csv\"\n[refs]\ncurrency_id = \"currencies.code\"\n",
		"data/countries.csv": "code,name,population,area,currency_id,note\n" +
			"DE,Germany,84000000,357588.5,EUR,\n" +
			"JP,Japan,124000000,377975,JPY,\"Says \"\"hello\"\",\nin two lines\"\n" +
			"\n" + // blank lines are skipped
			"FR,France,68000000,,EUR,\n",
	}
	expect(t, f.agree(files), "insert currencies/EUR; insert currencies/JPY; insert countries/DE; insert countries/JP; insert countries/FR")

	// Text from the CSV is stored the way the column's type stores it, and
	// an empty cell is NULL.
	if b.pg {
		if got := f.one("SELECT population + 1 || ' ' || area * 2 FROM countries WHERE code = 'DE'"); got != "84000001 715177" {
			t.Fatalf("DE: %s", got)
		}
		if got := f.one("SELECT count(*) FROM countries WHERE code = 'FR' AND area IS NULL AND note IS NULL"); got != "1" {
			t.Fatalf("FR: %s", got)
		}
	} else {
		if got := f.one("SELECT typeof(population) || ' ' || typeof(area) || ' ' || quote(area) FROM countries WHERE code = 'DE'"); got != "integer real 357588.5" {
			t.Fatalf("DE: %s", got)
		}
		if got := f.one("SELECT quote(area) || ' ' || quote(note) FROM countries WHERE code = 'FR'"); got != "NULL NULL" {
			t.Fatalf("FR: %s", got)
		}
	}
	if got := f.one("SELECT note FROM countries WHERE code = 'JP'"); got != "Says \"hello\",\nin two lines" {
		t.Fatalf("JP note: %q", got)
	}
	if got := f.one("SELECT c.code FROM countries JOIN currencies c ON c.id = currency_id WHERE countries.code = 'JP'"); got != "JPY" {
		t.Fatalf("JP currency: %s", got)
	}

	// The same rows in TOML plan nothing: a number in the CSV is the same
	// value as the number written in TOML.
	toml := map[string]string{
		"currencies.toml": "table = \"currencies\"\nkey = \"code\"\n[[row]]\ncode = \"EUR\"\ndigits = 2\n[[row]]\ncode = \"JPY\"\ndigits = 0\n",
		"countries.toml": "table = \"countries\"\nkey = \"code\"\non_remove = \"delete\"\n[refs]\ncurrency_id = \"currencies.code\"\n" +
			"[[row]]\ncode = \"DE\"\nname = \"Germany\"\npopulation = 84000000\narea = 357588.5\ncurrency_id = \"EUR\"\n" +
			"[[row]]\ncode = \"FR\"\nname = \"France\"\npopulation = 68000000\ncurrency_id = \"EUR\"\n" +
			"[[row]]\ncode = \"JP\"\nname = \"Japan\"\npopulation = 124000000\narea = 377975\ncurrency_id = \"JPY\"\nnote = \"Says \\\"hello\\\",\\nin two lines\"\n",
	}
	expect(t, f.agree(toml), "")

	if b.pg {
		return // the rest stores empty text in a REAL column, which only SQLite takes
	}
	// A change in the CSV, a rename, a dropped row and a custom delimiter
	// and null marker.
	files["countries.toml"] = strings.Replace(files["countries.toml"], `rows = "data/countries.csv"`, `rows = { file = "data/countries.csv", delimiter = ";", null = "\\N" }`, 1)
	files["data/countries.csv"] = "code;name;population;area;currency_id;note;_renamed_from\n" +
		"DEU;Germany;84100000;357588.5;EUR;\\N;DE\n" +
		"JP;Japan;124000000;377975;JPY;\"Says \"\"hello\"\",\nin two lines\";\n" +
		"ES;Spain;48000000;;EUR;;\n"
	p := f.agree(files)
	expect(t, p, "rename countries/DEU; update countries/DEU.population; insert countries/ES; delete countries/FR")
	// With null = "\N", an empty cell is the empty text, not NULL.
	if got := f.one("SELECT quote(area) || ' ' || quote(note) FROM countries WHERE code = 'ES'"); got != "'' ''" {
		t.Fatalf("ES: %s", got)
	}
}

func TestCSVSeedHashFollowsTheCSV(t *testing.T) {
	m := fstest.MapFS{
		"t.toml": {Data: []byte("table = \"t\"\nkey = \"k\"\nrows = \"t.csv\"\n")},
		"t.csv":  {Data: []byte("k\na\n")},
	}
	a, err := bs.LoadFS(m, ".")
	if err != nil {
		t.Fatal(err)
	}
	m["t.csv"] = &fstest.MapFile{Data: []byte("k\nb\n")}
	b, err := bs.LoadFS(m, ".")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash == b.Hash {
		t.Fatal("the seed hash didn't change with the CSV file")
	}
	if a.Tables[0].Rows[0].Where != "t.csv: line 2" {
		t.Fatalf("where: %q", a.Tables[0].Rows[0].Where)
	}
}

func TestCSVErrors(t *testing.T) {
	base := "table = \"t\"\nkey = \"k\"\n"
	cases := []struct{ toml, csv, want string }{
		{`rows = "t.csv"` + "\n[[row]]\nk = 1", "k\n1\n", "not both"},
		{`rows = 3`, "-", `rows must be "file.csv"`},
		{`rows = { file = "t.csv", sep = ";" }`, "k\n1\n", `unknown setting "sep"`},
		{`rows = { file = "t.csv", delimiter = ";;" }`, "k\n1\n", "delimiter must be one character"},
		{`rows = { delimiter = ";" }`, "-", `missing file`},
		{`rows = "../t.csv"`, "-", "inside the seed directory"},
		{`rows = "/t.csv"`, "-", "inside the seed directory"},
		{`rows = "missing.csv"`, "-", "missing.csv"},
		{`rows = "t.csv"`, "", "t.csv: empty"},
		{`rows = "t.csv"`, "k,,v\n1,2,3\n", "line 1, column 2 has no name"},
		{`rows = "t.csv"`, "k,v,v\n1,2,3\n", `names column "v" twice`},
		{`rows = "t.csv"`, "k,v\n1,2\n3\n", "t.csv: line 3 has 1 values, the header names 2 columns"},
		{`rows = "t.csv"`, "k,v\n1,2\n,4\n", `t.csv: line 3 has no value for key column "k"`},
		{`rows = "t.csv"`, "k,_why\n1,2\n", `t.csv: line 2: unknown setting "_why"`},
		{`rows = "t.csv"`, "k,v\n1,\"open\n", "t.csv"},
		{`rows = "t.csv"`, "k\n\xff\n", "not UTF-8"},
	}
	for _, c := range cases {
		m := fstest.MapFS{"t.toml": {Data: []byte(base + c.toml + "\n")}}
		if c.csv != "-" {
			m["t.csv"] = &fstest.MapFile{Data: []byte(c.csv)}
		}
		_, err := bs.LoadFS(m, ".")
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "t.toml: ") {
			t.Errorf("%s / %q: got %v, want an error with %q", c.toml, c.csv, err, c.want)
		}
	}
	// ParseTable alone can't read the CSV file and says what to use.
	if _, err := bs.ParseTable("t.toml", []byte(base+`rows = "t.csv"`)); err == nil || !strings.Contains(err.Error(), "use LoadDir or LoadFS") {
		t.Fatalf("ParseTable: %v", err)
	}
}

func TestCSVKeyRepeatedSaysWhere(t *testing.T) {
	f := fkFixture(t, countriesSchema)
	set := setFrom(t, map[string]string{
		"currencies.toml": "table = \"currencies\"\nkey = \"code\"\nrows = \"c.csv\"\n",
		"c.csv":           "code\nEUR\nUSD\nEUR\n",
	})
	if _, err := bs.Preview(f.ctx, f.db, set, nil); err == nil || err.Error() != `c.csv: line 4 repeats the key EUR` {
		t.Fatalf("got %v", err)
	}
}

// true and false in a CSV cell are TOML's true and false in a numeric
// column, so moving rows from TOML to CSV changes nothing.
func TestCSVBooleans(t *testing.T) {
	eachBackend(t, func(t *testing.T, b backend) {
		f := fixtureOn(t, b, `CREATE TABLE r (code TEXT PRIMARY KEY, active INTEGER, label TEXT)`)
		f.agree(map[string]string{"r.toml": "table = \"r\"\nkey = \"code\"\n[[row]]\ncode = \"a\"\nactive = true\nlabel = \"true\"\n[[row]]\ncode = \"b\"\nactive = false\n"})
		expect(t, f.agree(map[string]string{
			"r.toml": "table = \"r\"\nkey = \"code\"\nrows = \"r.csv\"\n",
			"r.csv":  "code,active,label\na,TRUE,true\nb,false,\n",
		}), "")
	})
}
