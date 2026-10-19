// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Command countries is the case study of a country and currency list kept
// in CSV files: a country changing its currency, a currency withdrawn while
// addresses still name the country, renames from the seed and from an
// admin.
//
//	go run ./examples/countries
package main

import (
	"embed"

	"github.com/bootstrapping-engine/bootstrapping/examples/internal/story"
)

//go:embed schema.sql seeds
var files embed.FS

func main() { story.Main(files, title, steps) }

const title = "Countries and currencies from CSV: Croatia moves to the euro, and two countries get new names."

func steps(s *story.Story) {
	s.Step("Release 1 ships six currencies and six countries, each list a CSV file.")
	s.Apply("v1")
	s.Say("The numbers in the CSV are stored as numbers.")
	s.Want("SELECT typeof(minor_units) || ' ' || minor_units FROM currencies WHERE code = 'JPY'", "integer 0")

	s.Step("Customers enter addresses, and an admin shortens Czech Republic to Czechia.")
	s.Exec("INSERT INTO addresses (line, country_id) SELECT 'Ilica 1, Zagreb', id FROM countries WHERE code = 'HR'")
	s.Exec("UPDATE countries SET name = 'Czechia' WHERE code = 'CZ'")

	s.Step("Release 2: Croatia uses the euro and the kuna is gone, Turkey is",
		"Türkiye, Czech Republic is Czechia, and Singapore is new. The plan:")
	s.Plan("v2")
	s.Apply("v2")

	s.Say("Croatia points at the euro, and its addresses are untouched.")
	s.Want("SELECT c.code FROM countries n JOIN currencies c ON c.id = n.currency_id WHERE n.code = 'HR'", "EUR")
	s.Want("SELECT n.code FROM addresses a JOIN countries n ON n.id = a.country_id", "HR")
	s.Say("The kuna was deleted after Croatia stopped pointing at it.")
	s.Want("SELECT count(*) FROM currencies WHERE code = 'HRK'", "0")
	s.Say("Türkiye's new name landed; Czechia was already right, so nothing changed.")
	s.Want("SELECT name FROM countries WHERE code = 'TR'", "Türkiye")
	s.Want("SELECT name FROM countries WHERE code = 'CZ'", "Czechia")
	s.Want("SELECT c.code FROM countries n JOIN currencies c ON c.id = n.currency_id WHERE n.code = 'SG'", "SGD")
	s.InStep("v2")
}
