// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"mvdan.cc/garble/internal/patcher"
)

func TestLiteralInitLargePackage(t *testing.T) {
	if testing.Short() {
		t.Skip("large package compiler integration")
	}
	dir := t.TempDir()
	var source strings.Builder
	source.WriteString("package main\nimport \"example.com/large/imported\"\n")
	for i := range 6000 {
		fmt.Fprintf(&source, "var Global%06d = %q\n", i, fmt.Sprintf("large package constant value %06d", i))
	}
	source.WriteString("var AggregateLive = [2]string{\"live main aggregate string\"}\nvar AggregateDead = [2]string{\"dead main aggregate string\"}\n")
	source.WriteString("var MapLive = map[string]rune{\"live main map key\": 'x'}\nvar MapDead = map[string]rune{\"dead main map key\": 'y'}\n")
	source.WriteString("func main() { if AggregateLive[0] != \"live main aggregate string\" || imported.AggregateLive.Text != \"live imported aggregate string\" || MapLive[\"live main map key\"] != 'x' || imported.MapLive[\"live imported map key\"] != 'y' { panic(\"aggregate init removed\") }; println(Global000000); println(imported.Live) }\n")
	if err := os.Mkdir(filepath.Join(dir, "imported"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"go.mod":               "module example.com/large\n\ngo 1.27\n",
		"main.go":              source.String(),
		"imported/imported.go": "package imported\nvar Live = \"live imported string global\"\nvar Dead = \"dead imported string global\"\ntype record struct { Text string }\nvar AggregateLive = record{\"live imported aggregate string\"}\nvar AggregateDead = record{\"dead imported aggregate string\"}\nvar MapLive = map[string]rune{\"live imported map key\": 'y'}\nvar MapDead = map[string]rune{\"dead imported map key\": 'z'}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "program")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	debugdir := filepath.Join(dir, "debug")
	cmd := exec.Command(self, "-literals", "-seed=AAAAAAAAAAA", "-debugdir="+debugdir, "build", "-ldflags=-dumpdep", "-o", binary, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RUN_GARBLE_MAIN=true", "GARBLE_LITERAL_INITS=stale", "GARBLE_LITERAL_INITS_FILE=/missing/inherited-manifest", "GARBLE_GO_SRC=")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("large package build: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil || string(output) != "large package constant value 000000\nlive imported string global\n" {
		t.Fatalf("large package execution: %v\n%s", err, output)
	}
	// Garble strips symbol tables, so use the linker's actual reachability graph.
	symbols := make(map[string]bool)
	for line := range strings.SplitSeq(string(output), "\n") {
		if _, target, ok := strings.Cut(line, " -> "); ok {
			if fields := strings.Fields(target); len(fields) > 0 {
				symbols[fields[0]] = true
			}
		}
	}
	for _, tc := range []struct {
		file    string
		globals int
	}{
		{"main.go", 6004},
		{"imported/imported.go", 6},
	} {
		t.Run(tc.file, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(debugdir, "garbled", "example.com/large", tc.file), nil, 0)
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
			if len(globals) < tc.globals {
				t.Fatalf("transformed globals: got %d, want at least %d", len(globals), tc.globals)
			}
			// Both the scalar and aggregate pairs declare the live global first.
			for _, i := range []int{0, 1, tc.globals - 4, tc.globals - 3, tc.globals - 2, tc.globals - 1} {
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
					t.Fatalf("global factory is not a direct call: %T", call.Fun)
				}
				for _, name := range []string{global.Names[0].Name, factory.Name} {
					found := false
					for symbol := range symbols {
						if strings.HasSuffix(symbol, "."+name) {
							found = true
						}
					}
					if want := i%2 == 0; found != want {
						t.Errorf("symbol %s retained = %v, want %v", name, found, want)
					}
				}
			}
		})
	}
}

func TestLiteralInitManifestCompiler(t *testing.T) {
	dir := t.TempDir()
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := patcher.PatchCompiler("", strings.TrimSpace(string(goroot)), strings.TrimSpace(string(version)), filepath.Join(cache, "garble"), dir)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "case.go")
	if err := os.WriteFile(source, []byte("package p\nvar value = factory()\n//go:noinline\nfunc factory() string { return \"value\" }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("inactive-stock-parity", func(t *testing.T) {
		toolDir, err := exec.Command("go", "env", "GOTOOLDIR").Output()
		if err != nil {
			t.Fatal(err)
		}
		object := filepath.Join(dir, "inactive.o")
		args := []string{"-p", "p", "-o", object, source}
		var stock []byte
		for _, tool := range []string{filepath.Join(strings.TrimSpace(string(toolDir)), "compile"), compiler} {
			if runtime.GOOS == "windows" && !strings.HasSuffix(tool, ".exe") {
				tool += ".exe"
			}
			cmd := exec.Command(tool, args...)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GARBLE_LITERAL_INITS_FILE=") && !strings.HasPrefix(entry, "GARBLE_PKGPATH_MAP=") && !strings.HasPrefix(entry, "GARBLE_SYMBOL_MAP=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("inactive compilation: %v\n%s", err, output)
			}
			data, err := os.ReadFile(object)
			if err != nil {
				t.Fatal(err)
			}
			if stock == nil {
				stock = data
			} else if !bytes.Equal(stock, data) {
				t.Fatal("inactive patched compiler differs from stock compiler")
			}
		}
	})
	certificates := map[string]string{"value": "factory"}
	for i := range 10000 {
		certificates[fmt.Sprintf("unused%06d", i)] = fmt.Sprintf("factory%06d", i)
	}
	data, err := json.Marshal(certificates)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 131072 {
		t.Fatal("manifest does not exceed Linux single-string limit")
	}
	t.Logf("manifest bytes: %d", len(data))
	// Match objabi.RelocType.IsDirectCall, never an indirect or weak call.
	eagerFactoryCall := regexp.MustCompile(`(?m)\bt=(?:R_CALL(?:ARM|ARM64|LOONG64|MIPS|POWER)?|R_RISCV_(?:CALL|JAL|JAL_TRAMP)) p\.factory\+0$`)
	for _, tc := range []struct{ name, data, diagnostic string }{
		{"aggregate-array", `{"value":"factory"}`, ""},
		{"aggregate-struct", `{"value":"factory"}`, ""},
		{"aggregate-map", `{"value":"factory"}`, ""},
		{"aggregate-interface", `{"value":"factory"}`, "invalid certified literal initializer"},
		{"aggregate-slice", `{"value":"factory"}`, "invalid certified literal initializer"},
		{"large", string(data), ""},
		{"large-moved", string(data), ""},
		{"darwin-arm64-pic", string(data), ""},
		{"dynlink", string(data), ""},
		{"linkshared", string(data), ""},
		{"linkname-darwin-arm64", string(data), ""},
		{"linkname", string(data), ""},
		{"linkname-single", string(data), ""},
		{"linknamestd", string(data), ""},
		{"linknamestd-single", string(data), ""},
		{"mismatched-factory", `{"value":"otherFactory"}`, "invalid certified literal factory"},
		{"invalid-assignment", `{"value":"factory"}`, "invalid certified literal initializer"},
		{"malformed", "{bad json", "invalid literal init certificate"},
		{"unreadable", "", "cannot read literal init certificate"},
		{"malformed-noinit", "{bad json", "invalid literal init certificate"},
		{"unreadable-noinit", "", "cannot read literal init certificate"},
		{"malformed-shared", "{bad json", "invalid literal init certificate"},
		{"malformed-disabled", "{bad json", "invalid literal init certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if !strings.HasPrefix(tc.name, "unreadable") {
				if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			input := source
			if strings.HasPrefix(tc.name, "aggregate-") {
				result := map[string]string{"aggregate-map": "map[string]rune", "aggregate-array": "[2]string", "aggregate-struct": "struct{ Text string }", "aggregate-interface": "any", "aggregate-slice": "[]string"}[tc.name]
				body := result + "{}"
				if result == "any" {
					body = `"value"`
				}
				input = filepath.Join(dir, tc.name+".go")
				text := "package p\nvar value = factory()\n//go:noinline\nfunc factory() " + result + " { return " + body + " }\n"
				if err := os.WriteFile(input, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			linked := strings.HasPrefix(tc.name, "linkname")
			if linked {
				directive := "//go:linkname value sharedAliasReview"
				if tc.name == "linkname-single" {
					directive = "//go:linkname value"
				} else if tc.name == "linknamestd" {
					directive = "//go:linknamestd value sharedAliasReview"
				} else if tc.name == "linknamestd-single" {
					directive = "//go:linknamestd value"
				}
				input = filepath.Join(dir, tc.name+".go")
				text := "package p\nimport _ \"unsafe\"\n" + directive + "\nvar value = factory()\n//go:noinline\nfunc factory() string { return \"value\" }\n"
				if err := os.WriteFile(input, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "invalid-assignment" {
				input = filepath.Join(dir, tc.name+".go")
				text := "package p\nvar value = factory(1)\n//go:noinline\nfunc factory(n int) string { return \"value\" }\n"
				if err := os.WriteFile(input, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(tc.name, "-noinit") {
				input = filepath.Join(dir, "noinit.go")
				if err := os.WriteFile(input, []byte("package p\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-S", "-p", "p", "-o", filepath.Join(dir, tc.name+".o")}
			if strings.HasSuffix(tc.name, "-shared") || tc.name == "darwin-arm64-pic" {
				args = append(args, "-shared")
			}
			if tc.name == "dynlink" || tc.name == "linkshared" {
				args = append(args, "-"+tc.name)
			}
			if strings.HasSuffix(tc.name, "-disabled") {
				args = append(args, "-d=wrapglobalmapctl=1")
			}
			cmd := exec.Command(compiler, append(args, input)...)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GARBLE_LITERAL_INITS=") && !strings.HasPrefix(entry, "GARBLE_LITERAL_INITS_FILE=") && !strings.HasPrefix(entry, "GARBLE_PKGPATH_MAP=") && !strings.HasPrefix(entry, "GARBLE_SYMBOL_MAP=") && !strings.HasPrefix(entry, "GOOS=") && !strings.HasPrefix(entry, "GOARCH=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			if strings.Contains(tc.name, "darwin-arm64") {
				cmd.Env = append(cmd.Env, "GOOS=darwin", "GOARCH=arm64")
			}
			cmd.Env = append(cmd.Env, "GARBLE_LITERAL_INITS_FILE="+path)
			output, err := cmd.CombinedOutput()
			if tc.diagnostic != "" {
				if err == nil || !strings.Contains(string(output), tc.diagnostic) {
					t.Fatalf("compiler did not fail closed: err=%v\n%s", err, output)
				}
			} else if linked || tc.name == "dynlink" || tc.name == "linkshared" {
				if err != nil || strings.Contains(string(output), "R_KEEP") || strings.Contains(string(output), "map.init.0") || !eagerFactoryCall.Match(output) {
					t.Fatalf("linknamed initializer was not kept eager: err=%v\n%s", err, output)
				}
			} else if err != nil || !strings.Contains(string(output), "R_KEEP") || !strings.Contains(string(output), "map.init.0") {
				t.Fatalf("file manifest did not certify initializer: err=%v\n%s", err, output)
			}
			if tc.name == "large-moved" {
				original, err := os.ReadFile(filepath.Join(dir, "large.o"))
				if err != nil {
					t.Fatal(err)
				}
				moved, err := os.ReadFile(filepath.Join(dir, "large-moved.o"))
				if err != nil {
					t.Fatal(err)
				}
				if string(original) != string(moved) {
					t.Fatal("compiler output depends on manifest path")
				}
			}
		})
	}
}
