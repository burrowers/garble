// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportLicenseCollision(t *testing.T) {
	t.Parallel()
	from, to := t.TempDir(), t.TempDir()
	for name, content := range map[string]string{
		filepath.Join(from, "LICENSE"): "license document",
		filepath.Join(to, "LICENSE"):   "existing asset",
	} {
		if err := os.WriteFile(name, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if err := exportLicenses(from, to); err == nil || !strings.Contains(err.Error(), "license destination collision") {
		t.Fatalf("expected license destination collision, got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(to, "LICENSE"))
	if err != nil || string(data) != "existing asset" {
		t.Fatalf("collision changed destination: %q, %v", data, err)
	}
}
