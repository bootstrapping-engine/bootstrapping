// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Command workflow is the case study of a helpdesk's ticket states: a state
// renamed while tickets sit in it, a new state slotted into the chain each
// state keeps to the next, and a label the admin chose.
//
//	go run ./examples/workflow
package main

import (
	"embed"

	"github.com/bootstrapping-engine/bootstrapping/examples/internal/story"
)

//go:embed schema.sql seeds
var files embed.FS

func main() { story.Main(files, title, steps) }

const title = "Workflow states: a state renamed under open tickets, and a new step in the chain."

const chain = `WITH RECURSIVE walk (id, code, n) AS (
		SELECT id, code, 0 FROM states WHERE code = 'new'
		UNION ALL
		SELECT s.id, s.code, walk.n + 1 FROM states s JOIN walk ON s.id = (SELECT next_id FROM states WHERE id = walk.id)
	) SELECT group_concat(code, ' > ') FROM (SELECT code FROM walk ORDER BY n)`

func steps(s *story.Story) {
	s.Step("Release 1 ships four states, each naming the next.")
	s.Apply("v1")
	s.Want(chain, "new > open > resolved > closed")

	s.Step("Tickets come in, and the admin relabels Open as \"In progress\".")
	s.Exec("INSERT INTO tickets (title, state_id) SELECT 'Printer on fire', id FROM states WHERE code = 'resolved'")
	s.Exec("INSERT INTO tickets (title, state_id) SELECT 'VPN drops at noon', id FROM states WHERE code = 'open'")
	s.Exec("UPDATE states SET label = 'In progress' WHERE code = 'open'")
	s.Exec("CREATE TABLE before AS SELECT title, state_id FROM tickets")

	s.Step("Release 2 renames resolved to done, and puts a new state, waiting,",
		"between open and done. The plan:")
	s.Plan("v2")
	s.Apply("v2")

	s.Say("The renamed state kept its id, so its ticket didn't move.")
	s.Want("SELECT count(*) FROM tickets t JOIN before b ON b.title = t.title AND b.state_id = t.state_id", "2")
	s.Want("SELECT s.code FROM tickets t JOIN states s ON s.id = t.state_id WHERE t.title = 'Printer on fire'", "done")
	s.Say("The chain runs through the new state.")
	s.Want(chain, "new > open > waiting > done > closed")
	s.Say("The admin's label stays; the renamed state took the seed's.")
	s.Want("SELECT label FROM states WHERE code = 'open'", "In progress")
	s.Want("SELECT label FROM states WHERE code = 'done'", "Done")
	s.InStep("v2")
}
