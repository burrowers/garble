// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestExportTargetsConstraint(t *testing.T) {
	t.Parallel()
	targets := map[exportTarget]bool{{"linux", "amd64"}: true, {"darwin", "arm64"}: true, {"solaris", "amd64"}: true}
	expr := exportTargetsConstraint(targets)
	for _, target := range []exportTarget{
		{"linux", "amd64"}, {"darwin", "arm64"}, {"solaris", "amd64"},
		{"android", "amd64"}, {"ios", "arm64"}, {"illumos", "amd64"},
		{"linux", "arm64"}, {"darwin", "amd64"},
	} {
		aliases := map[string]string{"android": "linux", "ios": "darwin", "illumos": "solaris"}
		got := expr.Eval(func(tag string) bool {
			return tag == target.GOOS || tag == target.GOARCH || tag == aliases[target.GOOS]
		})
		if got != targets[target] {
			t.Fatalf("%s/%s: constraint %s selected=%v", target.GOOS, target.GOARCH, expr, got)
		}
	}
}

func TestExportMergedConstraints(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, header string }{
		{"effective_legacy", "// +build export_alpha\n\n"},
		{"legacy_and", "// +build export_alpha\n// +build export_beta\n\n"},
		{"legacy_or", "// +build export_alpha export_beta\n\n"},
		{"package_doc", "// +build export_never_enabled\n"},
		{"package_doc_after_blank", "// Header.\n\n// +build export_never_enabled\n"},
		{"after_block", "/* Header. */\n// +build export_never_enabled\n"},
		{"after_block_blank", "/* Header. */\n\n// +build export_never_enabled\n\n"},
		{"before_block_no_blank", "// +build export_never_enabled\n/* Header. */\n\n"},
		{"before_block_blank", "// +build export_alpha\n\n/* Header. */\n"},
		{"inside_block", "/* Header.\n// +build export_never_enabled\n\n*/\n\n"},
		{"modern_package_doc", "//go:build export_alpha\n"},
		{"modern_after_block", "/* Header. */\n//go:build export_alpha\n"},
		{"modern_after_multiline_block", "/*\n//go:build export_never_enabled\n*/\n//go:build export_alpha\n"},
		{"modern_inline_after_block", "/* Header. */ //go:build export_never_enabled\n"},
		{"modern_precedence", "// +build export_beta\n//go:build export_alpha\n\n"},
		{"legacy_bom", "\ufeff// +build export_alpha\n\n"},
		{"modern_bom", "\ufeff//go:build export_alpha\n\n"},
		{"legacy_crlf", "// +build export_alpha\r\n 	\r\n"},
	} {
		for _, packageDoc := range []struct{ name, text string }{
			{"undocumented", ""},
			{"documented", "// Package placement documents the API.\n"},
		} {
			t.Run(test.name+"/"+packageDoc.name, func(t *testing.T) {
				root := t.TempDir()
				module := &packages.Module{Path: "example.com/placement", Dir: root}
				name := filepath.Join(root, "placement.go")
				source := test.header + packageDoc.text + "package placement\nfunc Public() {}\n"
				if err := os.WriteFile(name, []byte(source), 0o666); err != nil {
					t.Fatal(err)
				}
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, name, source, parser.ParseComments)
				if err != nil {
					t.Fatal(err)
				}
				docs, err := doc.NewFromFiles(fset, []*ast.File{file}, module.Path, doc.PreserveAST)
				if err != nil {
					t.Fatal(err)
				}
				pkg := &packages.Package{PkgPath: module.Path, Name: "placement", Dir: root, Module: module, Fset: fset, Syntax: []*ast.File{file}, CompiledGoFiles: []string{name}, TypesInfo: &types.Info{}}
				plan := &sourceExporter{module: module.Path, paths: map[string]string{module.Path: module.Path}}
				stage := t.TempDir()
				builds := []exportLoadedBuild{{exportBuild: exportBuild{targets: []exportTarget{{"linux", "amd64"}}}, packages: []*packages.Package{pkg}}}
				if err := writeExportBuilds(stage, module, builds, plan); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(stage, filepath.FromSlash(module.Path))
				outputSet := token.NewFileSet()
				output, err := parser.ParseFile(outputSet, filepath.Join(dir, "placement.go"), nil, parser.ParseComments)
				if err != nil {
					t.Fatal(err)
				}
				outputDocs, err := doc.NewFromFiles(outputSet, []*ast.File{output}, module.Path)
				if err != nil {
					t.Fatal(err)
				}
				if outputDocs.Doc != docs.Doc {
					t.Errorf("package godoc changed: original=%q exported=%q", docs.Doc, outputDocs.Doc)
				}
				for _, tags := range [][]string{nil, {"export_alpha"}, {"export_beta"}, {"export_alpha", "export_beta"}} {
					context := build.Default
					context.GOOS, context.GOARCH, context.BuildTags = "linux", "amd64", tags
					before, err := context.MatchFile(root, "placement.go")
					if err != nil {
						t.Fatal(err)
					}
					after, err := context.MatchFile(dir, "placement.go")
					if err != nil {
						t.Fatal(err)
					}
					if after != before {
						t.Errorf("tags %v: file selection changed: original=%v exported=%v", tags, before, after)
					}
				}
			})
		}
	}
}

func TestExportMergedFileOrder(t *testing.T) {
	root := t.TempDir()
	module := &packages.Module{Path: "example.com/order", Dir: root}
	var builds []exportLoadedBuild
	for _, goos := range []string{"linux", "windows"} {
		sourceDir := filepath.Join(root, goos)
		if err := os.Mkdir(sourceDir, 0o777); err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		var files []*ast.File
		var names []string
		for _, item := range []struct{ name, value string }{{"a.go", goos}, {"a0.go", "second"}} {
			name := filepath.Join(sourceDir, item.name)
			source := "// Package order keeps file initialization order.\npackage order\nfunc init() { _ = \"" + item.value + "\" }\n"
			if err := os.WriteFile(name, []byte(source), 0o666); err != nil {
				t.Fatal(err)
			}
			file, err := parser.ParseFile(fset, name, source, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			files, names = append(files, file), append(names, name)
		}
		pkg := &packages.Package{PkgPath: module.Path, Name: "order", Dir: sourceDir, Module: module, Fset: fset, Syntax: files, CompiledGoFiles: names, TypesInfo: &types.Info{}}
		builds = append(builds, exportLoadedBuild{exportBuild: exportBuild{targets: []exportTarget{{goos, "amd64"}}}, packages: []*packages.Package{pkg}})
	}
	stage := t.TempDir()
	plan := &sourceExporter{module: module.Path, paths: map[string]string{module.Path: module.Path}}
	if err := writeExportBuilds(stage, module, builds, plan); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stage, filepath.FromSlash(module.Path))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, goos := range []string{"linux", "windows"} {
		context := build.Default
		context.GOOS, context.GOARCH = goos, "amd64"
		var selected []string
		for _, entry := range entries {
			match, err := context.MatchFile(dir, entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if match {
				selected = append(selected, entry.Name())
			}
		}
		if len(selected) != 2 || !strings.HasPrefix(selected[0], "f000000_") || !strings.HasPrefix(selected[1], "f000001_") {
			t.Fatalf("%s: initialization order changed: %v", goos, selected)
		}
		data, err := os.ReadFile(filepath.Join(dir, selected[0]))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "\""+goos+"\"") {
			t.Fatalf("%s selected wrong body:\n%s", goos, data)
		}
		file, err := parser.ParseFile(token.NewFileSet(), selected[0], data, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if file.Doc == nil || file.Doc.Text() != "Package order keeps file initialization order.\n" {
			t.Fatalf("%s: variant constraints changed package documentation: %#v", goos, file.Doc)
		}
	}
}
