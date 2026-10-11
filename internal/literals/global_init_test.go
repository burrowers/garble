package literals

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"
	"testing"
)

func TestGlobalLiteralUnevaluatedInitDependency(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "global.go", `package globals
var early = len([1]string{late})
var late = "initialized through an unevaluated operand"
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("globals", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	file, inits := ObfuscateWithGlobalInits(rand.New(rand.NewSource(1)), file, info, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%x", base, r.Uint64()) })
	if len(inits) != 1 || inits[0].Global.Name != "late" {
		t.Fatalf("unexpected global certificates: %v", inits)
	}
	// len does not evaluate its operand, but Go still orders late before early.
	// Do not fold away that reference while obfuscating the string initializer.
	transformed := &types.Info{}
	if _, err := new(types.Config).Check("globals", fset, []*ast.File{file}, transformed); err != nil {
		t.Fatal(err)
	}
	if len(transformed.InitOrder) != 2 || transformed.InitOrder[0].Lhs[0].Name() != "late" || transformed.InitOrder[1].Lhs[0].Name() != "early" {
		t.Fatalf("unevaluated operand lost its init dependency: %v", transformed.InitOrder)
	}
}

func TestGlobalLiteralProxyOwnership(t *testing.T) {
	for _, obf := range Obfuscators {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "global.go", `package globals
var dead = "an independently removable string global"
var live = "another independently initialized string"
`, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
			if _, err := new(types.Config).Check("globals", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			old := testPkgToObfuscatorMap
			testPkgToObfuscatorMap = map[string]obfuscator{"globals": obf}
			defer func() { testPkgToObfuscatorMap = old }()
			file, inits := ObfuscateWithGlobalInits(rand.New(rand.NewSource(1)), file, info, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%x", base, r.Uint64()) })
			if len(inits) != 2 || inits[0].Factory == inits[1].Factory {
				t.Fatalf("globals lack exclusive factory certificates: %v", inits)
			}
			if _, err := new(types.Config).Check("globals", fset, []*ast.File{file}, nil); err != nil {
				t.Fatal(err)
			}
			vars := 0
			for _, decl := range file.Decls {
				if decl, ok := decl.(*ast.GenDecl); ok && decl.Tok == token.VAR {
					vars += len(decl.Specs)
				}
			}
			if vars != 2 {
				t.Fatalf("global initializers retain %d package variables; want only the two original globals", vars)
			}
			for _, decl := range file.Decls[:2] {
				init := decl.(*ast.GenDecl).Specs[0].(*ast.ValueSpec).Values[0].(*ast.CallExpr)
				if _, ok := init.Fun.(*ast.Ident); !ok {
					t.Fatalf("global initializer is not an isolated direct factory call: %T", init.Fun)
				}
			}
		})
	}
}
