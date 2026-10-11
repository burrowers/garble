package literals

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ah "mvdan.cc/garble/internal/asthelper"
)

func TestSeedRoundTrip(t *testing.T) {
	var source bytes.Buffer
	source.WriteString("package main\nfunc main() {\n")
	for _, randSeed := range []int64{1, 2, 3} {
		for _, size := range []int{1, 17, 257} {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i)
			}
			r := newObfRand(rand.New(rand.NewSource(randSeed)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
			names := newGeneratedNames(r)
			keys := randExtKeys(r.rnd, names)
			body := (seed{}).obfuscate(r.rnd, names, data, keys)
			body.List = append(body.List, ah.ReturnStmt(names.ident("data")))
			params := &ast.FieldList{}
			var args []ast.Expr
			for _, key := range keys {
				params.List = append(params.List, ah.Field(key.Type(), key.Name()))
				args = append(args, ah.UintLit(key.value))
			}
			source.WriteString("{ decode := func() []byte { return ")
			if err := printer.Fprint(&source, token.NewFileSet(), ah.LambdaCall(params, ah.ByteSliceType(), body, args)); err != nil {
				t.Fatal(err)
			}
			source.WriteString(" }; got := decode(); ")
			fmt.Fprintf(&source, "if len(got) != %d || cap(got) != %d { panic(\"capacity\") }; ", size, size)
			source.WriteString("for i, b := range got { if b != byte(i) { panic(\"contents\") } }; ")
			source.WriteString("ptr := &got; (*ptr)[0] = 42; fresh := decode(); if got[0] != 42 || fresh[0] != 0 { panic(\"aliasing\") } }\n")
		}
	}
	source.WriteString("}\n")
	dir := t.TempDir()
	srcPath, binPath := filepath.Join(dir, "main.go"), filepath.Join(dir, "probe.exe")
	if err := os.WriteFile(srcPath, source.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-p=1", "-o", binPath, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if out, err := exec.Command(binPath).CombinedOutput(); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
}

func TestSeedFixedOutputBuffer(t *testing.T) {
	r := newObfRand(rand.New(rand.NewSource(123)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
	names := newGeneratedNames(r)
	keys := randExtKeys(r.rnd, names)
	block := (seed{}).obfuscate(r.rnd, names, []byte("long_enough_string"), keys)
	var appendCalls, indexedWrites, indirectCalls int
	ast.Inspect(block, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "append" {
				appendCalls++
			}
			if _, ok := node.Fun.(*ast.CallExpr); ok {
				indirectCalls++
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if idx, ok := lhs.(*ast.IndexExpr); ok {
					if id, ok := idx.X.(*ast.Ident); ok && id.Name == names.name("data") {
						indexedWrites++
					}
				}
			}
		}
		return true
	})
	if appendCalls != 0 || indexedWrites != 1 {
		t.Fatalf("decoder must fill its fixed buffer: append calls=%d, indexed writes=%d", appendCalls, indexedWrites)
	}
	if indirectCalls != len("long_enough_string")-1 {
		t.Fatalf("recursive function-value chain lost: got %d nested calls", indirectCalls)
	}
	var refs int
	for _, key := range keys {
		refs += key.refs
	}
	if refs == 0 {
		t.Fatal("decoder lost external key dependencies")
	}
}
