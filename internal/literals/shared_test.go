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
	"strings"
	"testing"

	"mvdan.cc/garble/internal/literals"
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
}
`

func TestSharedShortLiteralDecoder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "shared.go", sharedSource, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	originalCount := len(file.Decls)
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	literals.Obfuscate(mathrand.New(mathrand.NewSource(7)), file, info, nil, nil, func(r *mathrand.Rand, base string) string {
		return fmt.Sprintf("%s%d", base, r.Uint64())
	})

	shared := 0
	for _, decl := range file.Decls[originalCount:] {
		if fn, ok := decl.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "literalSharedDecoder") {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("no shared decoder was generated for short literals")
	}
	if shared > len(literals.Obfuscators) {
		t.Fatalf("generated %d shared decoders, want at most one per operator (%d)", shared, len(literals.Obfuscators))
	}

	var text strings.Builder
	if err := printer.Fprint(&text, fset, file); err != nil {
		t.Fatal(err)
	}
	source := text.String()
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
	want := "short-literal-1\nshort-literal-2\nshort-literal-1\neight-by\nbyte-slice-literal\n"
	if got := string(out); got != want {
		t.Fatalf("decoded output mismatch:\n got %q\nwant %q", got, want)
	}
}
