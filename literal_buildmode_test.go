// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Check both the global and its factory against the linker's reachability graph.
// Garble strips symbol tables, so normal binaries cannot be inspected with nm.
func checkLiteralInitReachability(t *testing.T, source string, output []byte, indices []int, retained func(int) bool) {
	t.Helper()
	symbols := make(map[string]bool)
	for line := range strings.SplitSeq(string(output), "\n") {
		if _, target, ok := strings.Cut(line, " -> "); ok {
			if fields := strings.Fields(target); len(fields) > 0 {
				// Package paths can contain dots; only the final component is a name.
				name := fields[0][strings.LastIndexByte(fields[0], '.')+1:]
				symbols[name] = true
			}
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var globals []*ast.ValueSpec
	for _, decl := range file.Decls {
		if decl, ok := decl.(*ast.GenDecl); ok && decl.Tok == token.VAR {
			for _, spec := range decl.Specs {
				globals = append(globals, spec.(*ast.ValueSpec))
			}
		}
	}
	for _, i := range indices {
		if i >= len(globals) {
			t.Fatalf("missing global %d in %s", i, source)
		}
		global := globals[i]
		if len(global.Names) != 1 || len(global.Values) != 1 {
			t.Fatalf("unexpected global shape: %v", global)
		}
		call, ok := global.Values[0].(*ast.CallExpr)
		if !ok {
			t.Fatalf("global initializer is not a factory call: %T", global.Values[0])
		}
		factory, ok := call.Fun.(*ast.Ident)
		if !ok {
			t.Fatalf("factory is not a direct call: %T", call.Fun)
		}
		for _, name := range []string{global.Names[0].Name, factory.Name} {
			if want := retained(i); symbols[name] != want {
				t.Errorf("%s: symbol %s retained = %v, want %v", source, name, symbols[name], want)
			}
		}
	}
}

func TestLiteralInitDarwinBuildModes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "dep"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"go.mod":     "module example.com/pic\n\ngo 1.27\n",
		"main.go":    "package main\nimport \"example.com/pic/dep\"\nvar Live = \"live main global string\"\nvar Dead = \"dead main global string\"\nfunc main() { println(Live, dep.Live) }\n",
		"dep/dep.go": "package dep\nvar Live = \"live imported global string\"\nvar Dead = \"dead imported global string\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Repeat PIE after the library build with the same imported package and cache.
	for _, mode := range []string{"default", "exe", "pie", "c-archive", "pie", "c-archive-goflags", "goflags-overridden"} {
		t.Run(mode, func(t *testing.T) {
			debugdir := filepath.Join(t.TempDir(), "debug")
			args := []string{"-literals", "-seed=AAAAAAAAAAA", "-debugdir=" + debugdir, "build", "-ldflags=-dumpdep", "-o", filepath.Join(dir, "out")}
			goflags := ""
			switch mode {
			case "c-archive-goflags":
				goflags = "-buildmode=c-archive"
			case "goflags-overridden":
				goflags = "-buildmode=c-archive"
				args = append(args, "-buildmode=pie")
			default:
				args = append(args, "-buildmode="+mode)
			}
			cmd := exec.Command(self, append(args, ".")...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "RUN_GARBLE_MAIN=true", "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0", "GARBLE_GO_SRC=", "GOFLAGS="+goflags)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Darwin arm64 %s build: %v\n%s", mode, err, output)
			}
			for _, name := range []string{"main.go", "dep/dep.go"} {
				checkLiteralInitReachability(t, filepath.Join(debugdir, "garbled", "example.com/pic", name), output, []int{0, 1}, func(i int) bool {
					return i == 0 || mode == "c-archive" || mode == "c-archive-goflags"
				})
			}
		})
	}
}
