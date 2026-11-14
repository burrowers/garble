// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportHeaderDocs(t *testing.T) {
	t.Parallel()
	for name, prefix := range map[string]string{
		"undocumented": "",
		"package":      "// Package library provides an API.\n// Its package documentation stays intact.\n",
		"license":      "// SPDX-License-Identifier: MIT\n\n// Package library provides an API.\n",
		"block":        "/* Package library provides an API. */\n",
	} {
		t.Run(name, func(t *testing.T) {
			source := prefix + "package library\n\n// Public provides an API.\nfunc Public() {}\n"
			readDocs := func(source []byte) *doc.Package {
				t.Helper()
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, "library.go", source, parser.ParseComments)
				if err != nil {
					t.Fatal(err)
				}
				pkg, err := doc.NewFromFiles(fset, []*ast.File{file}, "example.com/library")
				if err != nil {
					t.Fatal(err)
				}
				return pkg
			}
			before := readDocs([]byte(source))
			output, err := compactExportSource([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			after := readDocs(output)
			if after.Doc != before.Doc || len(after.Funcs) != 1 || after.Funcs[0].Doc != before.Funcs[0].Doc {
				t.Fatalf("export header changed godoc: package %q -> %q, functions %#v -> %#v", before.Doc, after.Doc, before.Funcs, after.Funcs)
			}
			const marker = "// DO NOT EDIT: code obfuscated by garble"
			if !strings.HasPrefix(string(output), marker+"\n\n") || strings.Count(string(output), marker) != 1 {
				t.Fatalf("missing standalone export header:\n%s", output)
			}
		})
	}
}

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
