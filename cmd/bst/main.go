// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Command bst is bootstrapping under a shorter name.
package main

import (
	"os"

	"github.com/bootstrapping-engine/bootstrapping/internal/cli"
)

func main() {
	os.Exit(cli.Main("bst", os.Args[1:], os.Stdout, os.Stderr))
}
