// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
)

// The demo kills a copy of itself; under go test that copy is the test
// binary, started with -crash-child.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "-crash-child" {
		crashChild(os.Args[2])
		return
	}
	os.Exit(m.Run())
}

func TestDemo(t *testing.T) {
	if err := run(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
