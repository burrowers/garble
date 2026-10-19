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
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GOOS=") && !strings.HasPrefix(entry, "GOARCH=") && !strings.HasPrefix(entry, "CGO_ENABLED=") && !strings.HasPrefix(entry, "GARBLE_GO_SRC=") && !strings.HasPrefix(entry, "GOFLAGS=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "RUN_GARBLE_MAIN=true", "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0", "GARBLE_GO_SRC=", "GOFLAGS="+goflags)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Darwin arm64 %s build: %v\n%s", mode, err, output)
			}
			symbols := make(map[string]bool)
			for line := range strings.SplitSeq(string(output), "\n") {
				if _, target, ok := strings.Cut(line, " -> "); ok {
					if fields := strings.Fields(target); len(fields) > 0 {
						symbols[fields[0]] = true
					}
				}
			}
			for _, name := range []string{"main.go", "dep/dep.go"} {
				file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(debugdir, "garbled", "example.com/pic", name), nil, 0)
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
				if len(globals) < 2 {
					t.Fatalf("%s: missing original globals", name)
				}
				for i, global := range globals[:2] {
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
					for _, symbolName := range []string{global.Names[0].Name, factory.Name} {
						found := false
						for symbol := range symbols {
							if strings.HasSuffix(symbol, "."+symbolName) {
								found = true
							}
						}
						if want := i == 0 || mode == "c-archive" || mode == "c-archive-goflags"; found != want {
							t.Errorf("%s: symbol %s retained = %v, want %v", name, symbolName, found, want)
						}
					}
				}
			}
		})
	}
}
