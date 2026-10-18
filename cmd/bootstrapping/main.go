// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Command bootstrapping keeps an application's reference data in step with
// its SQLite database. Run it with no arguments for help.
package main

import (
	"os"

	"github.com/bootstrapping-engine/bootstrapping/internal/cli"
)

func main() {
	os.Exit(cli.Main("bootstrapping", os.Args[1:], os.Stdout, os.Stderr))
}
