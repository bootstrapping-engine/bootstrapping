// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"

	"github.com/bootstrapping-engine/bootstrapping/examples/internal/story"
)

func TestWorkflow(t *testing.T) {
	if err := story.Run(files, t.TempDir(), io.Discard, title, steps); err != nil {
		t.Fatal(err)
	}
}
