// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/burrowers/garble/internal/literals"
)

const sharedSource = `package main

var first = "short-literal-1"
var second = "short-literal-2"
var repeated = "short-literal-1"

func main() {
	println(first)
	println(second)
	println(repeated)
	println("eight-by")
	println(string([]byte("byte-slice-literal")))
	values()
}
`

func TestSharedShortLiteralDecoder(t *testing.T) {
	var input, output strings.Builder
	input.WriteString(sharedSource + "\nfunc values() {\n")
	output.WriteString("short-literal-1\nshort-literal-2\nshort-literal-1\neight-by\nbyte-slice-literal\n")
	for i := range 60 {
		text := fmt.Sprintf("short-%02d%s", i, strings.Repeat("x", i%12))
		fmt.Fprintf(&input, "println(%q)\n", text)
		fmt.Fprintln(&output, text)
	}
	for i := range 40 {
		fmt.Fprintf(&input, "owner%d()\n", i)
		fmt.Fprint(&output, "eight-by\n", strings.Repeat("x", 24), "\neight-by\n")
	}
	input.WriteString("}\n")
	// Many independent owners exercise every randomly selected family. Each
	// owner mixes lengths and repeats a value inside its nested closure.
	for i := range 40 {
		fmt.Fprintf(&input, "func owner%d() { println(%q); println(%q); func() { println(%q) }() }\n", i, "eight-by", strings.Repeat("x", 24), "eight-by")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "shared.go", input.String(), parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Clone(file.Decls)
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	literals.Obfuscate(mathrand.New(mathrand.NewSource(7)), file, info, nil, nil, nil, func(r *mathrand.Rand, base string) string {
		return fmt.Sprintf("%s%d", base, r.Uint64())
	})

	owners := make(map[string]string)
	orig, generated := literals.SplitDecls(file, before)
	for _, decl := range orig {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "values" && !strings.HasPrefix(fn.Name.Name, "owner")) {
			continue
		}
		targets := make(map[string]bool)
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "println" {
				return true
			}
			decoder, ok := call.Args[0].(*ast.CallExpr)
			if !ok {
				t.Fatal("short literal was not decoded")
			}
			var target strings.Builder
			if err := printer.Fprint(&target, fset, decoder.Fun); err != nil {
				t.Fatal(err)
			}
			targets[target.String()] = true
			return true
		})
		if len(targets) != 1 {
			t.Fatalf("owner %s uses %d short decoders; want one", fn.Name.Name, len(targets))
		}
		for target := range targets {
			if owner := owners[target]; owner != "" {
				t.Fatalf("owners %s and %s share a decoder", owner, fn.Name.Name)
			}
			owners[target] = fn.Name.Name
		}
	}

	starts := make(map[string]bool)
	decoders := 0
	for _, decl := range generated {
		if fn, ok := decl.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "literalDecoder") {
			decoders++
			name := fn.Body.List[0].(*ast.AssignStmt).Lhs[0].(*ast.Ident).Name
			starts[strings.TrimRight(name, "0123456789")] = true
		}
	}
	// Three global factories, main, values and forty other owners.
	if decoders != 45 {
		t.Fatalf("got %d decoders; want 45", decoders)
	}

	var text strings.Builder
	if err := printer.Fprint(&text, fset, file); err != nil {
		t.Fatal(err)
	}
	source := text.String()
	// The standard families start with key (simple), data (swap), fullData
	// (shuffle), or payload (seed/split); offsets distinguishes split from seed.
	for _, name := range []string{"key", "data", "fullData", "payload"} {
		if !starts["literalLocal"+name] {
			t.Errorf("no standard decoder starting with %s was selected", name)
		}
	}
	if !strings.Contains(source, "literalLocaloffsets") {
		t.Error("the split decoder was not selected")
	}
	if !strings.Contains(source, "literalLocalseed") {
		t.Error("the seed decoder was not selected")
	}
	for _, plain := range []string{"short-literal-1", "short-literal-2", "eight-by", "byte-slice-literal"} {
		if strings.Contains(source, plain) {
			t.Fatalf("literal %q survived in the generated source", plain)
		}
	}

	dir := t.TempDir()
	srcPath, binPath := filepath.Join(dir, "main.go"), filepath.Join(dir, "main.exe")
	if err := os.WriteFile(srcPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-trimpath", "-o", binPath, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := output.String()
	if got := string(out); got != want {
		t.Fatalf("decoded output mismatch:\n got %q\nwant %q", got, want)
	}
}
