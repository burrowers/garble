package literals

import (
	"go/ast"
	"go/token"
	"math/rand"
	"testing"
)

func TestSeedRoundTrip(t *testing.T) {
	testDecoderRoundTrip(t, seed{})
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
