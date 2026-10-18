// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package bootstrapping

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func scaleOf() int {
	if n, err := strconv.Atoi(os.Getenv("BS_SCALE")); err == nil && n > 0 {
		return n
	}
	return 1
}

func recordResult(t *testing.T, name string, v any) {
	dir := os.Getenv("BS_RESULTS")
	if dir == "" {
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
