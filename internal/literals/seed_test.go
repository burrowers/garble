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

func TestSeedPayloadLoop(t *testing.T) {
	r := newObfRand(rand.New(rand.NewSource(123)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
	names := newGeneratedNames(r)
	keys := randExtKeys(r.rnd, names)
	data := []byte("long_enough_string")
	block := (seed{}).obfuscate(r.rnd, names, data, keys)

	var appendCalls, indexedWrites, loops, payloadStrings int
	ast.Inspect(block, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "append" {
				appendCalls++
			}
			if _, ok := node.Fun.(*ast.CallExpr); ok {
				t.Fatal("decoder still builds a nested function-value chain")
			}
		case *ast.ForStmt:
			loops++
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				id, ok := idx.X.(*ast.Ident)
				if !ok || id.Name != names.name("data") {
					continue
				}
				indexedWrites++
				// The payload is the only static copy of the encrypted bytes.
				p, ok := idx.Index.(*ast.Ident)
				if !ok || p.Name != names.name("i") {
					t.Fatalf("decoded byte is not written at the loop index: %T", idx.Index)
				}
			}
			for i, lhs := range node.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == names.name("payload") {
					lit, ok := node.Rhs[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("payload must be a string literal, got %T", node.Rhs[i])
					}
					payloadStrings++
				}
			}
		}
		return true
	})
	if appendCalls != 0 || indexedWrites != 1 || loops != 1 {
		t.Fatalf("decoder must fill its buffer in one loop: append calls=%d, indexed writes=%d, loops=%d",
			appendCalls, indexedWrites, loops)
	}
	if payloadStrings != 1 {
		t.Fatalf("decoder must keep exactly one static payload literal, got %d", payloadStrings)
	}
	var refs int
	for _, key := range keys {
		refs += key.refs
	}
	if refs == 0 {
		t.Fatal("decoder lost external key dependencies")
	}
}
