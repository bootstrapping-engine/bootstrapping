// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Package bootstrapping keeps the reference data an application ships with
// (roles, permissions, plan tiers, workflow states, country lists) in step
// with its database, without undoing what admins changed.
//
// Seed files say what the rows should be. A ledger in the same database
// remembers what was applied last time. Each apply is a three-way merge per
// value: the new seed, the live row and the last-applied seed. An admin's
// edit is kept, a seed change to an untouched value lands, and a value
// changed on both sides, differently, is a conflict that waits for a
// decision instead of being overwritten.
//
//	set, err := bootstrapping.LoadDir("seeds")
//	plan, err := bootstrapping.Preview(ctx, db, set, nil)
//	plan, err = bootstrapping.Apply(ctx, db, set, nil)
//
// The package works with any database/sql driver for SQLite or PostgreSQL
// (12 or later) and tells the two apart on its own. It needs the
// application's tables to exist already; schema migrations are another
// tool's job.
package bootstrapping

// Version is the release, raised before a release goes out.
const Version = "0.2.1"
