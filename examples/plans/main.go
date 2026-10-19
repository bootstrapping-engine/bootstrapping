// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Command plans is the case study of a SaaS product's plan tiers: prices
// each install sets for itself, a plan retired while customers still
// subscribe to it, and a new tier with its features.
//
//	go run ./examples/plans
package main

import (
	"embed"

	"github.com/bootstrapping-engine/bootstrapping/examples/internal/story"
)

//go:embed schema.sql seeds
var files embed.FS

func main() { story.Main(files, title, steps) }

const title = "Plan tiers: prices that belong to each install, and a plan retired under its customers."

func steps(s *story.Story) {
	s.Step("Release 1 ships three plans and the features each one includes.")
	s.Apply("v1")
	s.Want("SELECT count(*) FROM plan_features", "6")

	s.Step("The customer signs people up, sets their own price for Team,",
		"and renames Business for their market.")
	s.Exec("INSERT INTO subscriptions (customer, plan_id) SELECT 'Northwind', id FROM plans WHERE code = 'free'")
	s.Exec("UPDATE plans SET price_cents = 999 WHERE code = 'team'")
	s.Exec("UPDATE plans SET name = 'Business Plus' WHERE code = 'business'")

	s.Step("Release 2 drops the free plan, gives Team 15 seats and a new list",
		"price, and adds an Enterprise tier and single sign-on. The plan:")
	s.Plan("v2")
	s.Apply("v2")

	s.Say("Team's price is the install's own: price_cents is a local column.")
	s.Want("SELECT price_cents FROM plans WHERE code = 'team'", "999")
	s.Say("The new seat count landed; nobody had changed it.")
	s.Want("SELECT seats FROM plans WHERE code = 'team'", "15")
	s.Say("The admin's name for Business stays.")
	s.Want("SELECT name FROM plans WHERE code = 'business'", "Business Plus")
	s.Say("Free is retired, not deleted, and Northwind's subscription still works.")
	s.Want("SELECT retired_at FROM plans WHERE code = 'free'", "2026-11-02T08:00:00Z")
	s.Want("SELECT p.code FROM subscriptions s JOIN plans p ON p.id = s.plan_id WHERE s.customer = 'Northwind'", "free")
	s.Say("Its feature links went with it, and Enterprise has all four features.")
	s.Want("SELECT count(*) FROM plan_features pf JOIN plans p ON p.id = pf.plan_id WHERE p.code = 'free'", "0")
	s.Want("SELECT count(*) FROM plan_features pf JOIN plans p ON p.id = pf.plan_id WHERE p.code = 'enterprise'", "4")
	s.InStep("v2")
}
