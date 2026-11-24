// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"
	"testing"
)

// TestByteTypeParamCore covers type sets a composite literal cannot be written
// with, such as a union of two different byte types, or one that can only be
// reached through a nested constraint. Those shapes must stay unrewritten
// rather than receive a conversion the compiler rejects.
func TestByteTypeParamCore(t *testing.T) {
	const src = `package typeparam

type namedBytes []byte
type definedByte byte
type Nested interface{ ~[]byte }

type sliceCore[S ~[]byte] struct{}
type exactTerm[S []byte] struct{}
type arrayCore[S ~[16]byte] struct{}
type arrayUint8[S ~[16]uint8] struct{}
type namedTerm[S namedBytes] struct{}
type withMethod[S interface{ ~[]byte; M() }] struct{}

type runeCore[S ~[]rune] struct{}
type definedElem[S ~[]definedByte] struct{}
type unionTerms[S interface{ ~[]byte | ~[16]byte }] struct{}
type intersectionTerms[S interface{ ~[]byte; ~[16]byte }] struct{}
type nestedTerm[S Nested] struct{}
type nestedInterfaceTerm[S interface{ Nested }] struct{}
type mapCore[S ~map[string]byte] struct{}
type anyTerm[S any] struct{}
type comparableTerm[S comparable] struct{}
`
	fset := new(token.FileSet)
	file, err := parser.ParseFile(fset, "typeparam.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
		Types: make(map[ast.Expr]types.TypeAndValue),
	}
	pkg, err := new(types.Config).Check("typeparam", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		wantLen int64
		wantOK  bool
	}{
		{"sliceCore", 0, true},
		{"exactTerm", 0, true},
		{"arrayCore", 16, true},
		{"arrayUint8", 16, true},
		{"namedTerm", 0, true},
		{"withMethod", 0, true},
		{"runeCore", 0, false},
		{"definedElem", 0, false},
		{"unionTerms", 0, false},
		{"intersectionTerms", 0, false},
		{"nestedTerm", 0, true},
		{"nestedInterfaceTerm", 0, false},
		{"mapCore", 0, false},
		{"anyTerm", 0, false},
		{"comparableTerm", 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			named, ok := pkg.Scope().Lookup(test.name).Type().(*types.Named)
			if !ok || named.TypeParams().Len() != 1 {
				t.Fatalf("%s has no single type parameter", test.name)
			}
			tp := named.TypeParams().At(0)
			gotLen, gotOK := byteTypeParamCore(tp)
			if gotOK != test.wantOK || (gotOK && gotLen != test.wantLen) {
				t.Fatalf("byteTypeParamCore(%s) = (%d, %v), want (%d, %v)", test.name, gotLen, gotOK, test.wantLen, test.wantOK)
			}
		})
	}
}

// TestByteTypeParamShadowedSpelling covers the conversion's type spelling. A
// declaration with the parameter's name inside the same function resolves to a
// different object, so the conversion cannot name the type safely and the
// literal has to keep the form it was written in instead of producing code that
// does not compile.
func TestByteTypeParamShadowedSpelling(t *testing.T) {
	const src = `package typeparam

func plainBytes[S ~[8]byte]() S {
	return S{1, 2, 3, 4, 5, 6, 7, 8}
}

func shadowedBytes[S ~[8]byte]() S {
	if true {
		S := 7
		_ = S
	}
	return S{1, 2, 3, 4, 5, 6, 7, 8}
}
`
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	file, _ := CheckSource(t, "typeparam", "typeparam.go", src, parser.SkipObjectResolution, info)
	Obfuscate(rand.New(rand.NewSource(1)), file, info, nil, nil, nil, func(r *rand.Rand, base string) string { return base })
	decoded := make(map[string]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		for _, stmt := range fn.Body.List {
			ret, ok := stmt.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			_, isComposite := ret.Results[0].(*ast.CompositeLit)
			decoded[fn.Name.Name] = !isComposite
		}
	}
	if !decoded["plainBytes"] {
		t.Error("a byte composite of a parameter without a shadowing declaration was not decoded")
	}
	if decoded["shadowedBytes"] {
		t.Error("a byte composite of a shadowed parameter was decoded, so the conversion cannot name the type")
	}
}
