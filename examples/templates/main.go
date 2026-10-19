// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Command templates is the case study of email templates a customer's
// admin rewords and switches off: a conflict when the product changes a
// subject the admin also changed, the plan as a pull request comment, and
// legal text the product always owns.
//
//	go run ./examples/templates
package main

import (
	"embed"

	bs "github.com/bootstrapping-engine/bootstrapping"
	"github.com/bootstrapping-engine/bootstrapping/examples/internal/story"
)

//go:embed schema.sql seeds
var files embed.FS

func main() { story.Main(files, title, steps) }

const title = "Email templates: a conflict over a subject line, settled once, and legal text the product owns."

const footer = "You get this email because you have an account with us. Manage email settings in your profile."

func steps(s *story.Story) {
	s.Step("Release 1 ships three email templates.")
	s.Apply("v1")

	s.Step("A customer's admin brands the welcome subject, turns the invoice",
		"email off, and edits the footer of the password email.")
	s.Exec("UPDATE templates SET subject = 'Welcome to the Riverside Clinic portal' WHERE code = 'welcome'")
	s.Exec("UPDATE templates SET enabled = 0 WHERE code = 'invoice'")
	s.Exec("UPDATE templates SET footer = 'Questions? Call reception.' WHERE code = 'password_reset'")

	s.Step("Release 2 is a pull request. CI applies release 1 to a scratch",
		"database and posts this plan on it with bst plan -markdown (here, against",
		"the customer's copy, so it shows their edits too):")
	s.Markdown("v2")

	s.Step("Applied to the customer's database, the welcome subject is a conflict:",
		"both sides changed it. Nothing is written.")
	s.Blocked("v2")
	s.Want("SELECT count(*) FROM templates", "3")

	s.Step("The customer keeps their subject, for this one value.")
	s.Apply("v2", bs.Resolution{Table: "templates", Key: "welcome", Column: "subject", Take: bs.TakeLocal})

	s.Want("SELECT subject FROM templates WHERE code = 'welcome'", "Welcome to the Riverside Clinic portal")
	s.Say("The invoice email stays off: enabled is the customer's column.")
	s.Want("SELECT enabled FROM templates WHERE code = 'invoice'", "0")
	s.Say("The footer is the product's: the new legal text replaced the admin's edit.")
	s.Want("SELECT count(*) FROM templates WHERE footer = '"+footer+"'", "4")
	s.Say("The password email's new body landed, and the login alert is new.")
	s.Want("SELECT body FROM templates WHERE code = 'password_reset'", "Use the link below to choose a new password. It works for one hour.")
	s.Want("SELECT enabled FROM templates WHERE code = 'login_alert'", "1")
	s.Say("And the conflict doesn't come back on the next plan.")
	s.InStep("v2")
}
