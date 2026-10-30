// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"bytes"
	"go/ast"
	"math/rand"
	"testing"
)

func TestSplitRoundTrip(t *testing.T) {
	testDecoderRoundTrip(t, split{})
}

func TestSplitRecordDecoder(t *testing.T) {
	var block *ast.BlockStmt
	var names *generatedNames
	var keys []*externalKey
	for _, randSeed := range []int64{11, 12, 13, 14} {
		r := newObfRand(rand.New(rand.NewSource(randSeed)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
		names = newGeneratedNames(r)
		keys = randExtKeys(r.rnd, names)
		block = (split{}).obfuscate(r.rnd, names, bytes.Repeat([]byte("chunked data "), 8), keys)
	}

	var appends, switches, loops, indexedWrites int
	ast.Inspect(block, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "append" {
				appends++
			}
		case *ast.SwitchStmt, *ast.CaseClause:
			switches++
		case *ast.ForStmt:
			loops++
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
	if appends != 0 || switches != 0 {
		t.Fatalf("decoder still expands per-chunk code: append calls=%d, switch clauses=%d", appends, switches)
	}
	// One record loop and one chunk loop.
	if loops != 2 || indexedWrites != 1 {
		t.Fatalf("decoder must write chunks in two loops: loops=%d, indexed writes=%d", loops, indexedWrites)
	}
	var refs int
	for _, key := range keys {
		refs += key.refs
	}
	if refs == 0 {
		t.Fatal("decoder lost external key dependencies")
	}
}
