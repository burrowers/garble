package literals

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"math/rand"
	"strings"
	"testing"
)

func TestRepeatedStringDecoders(t *testing.T) {
	const source = `package main
var global = "repeated literal secret"
func first() {
 _ = "repeated literal secret"
 _ = "repeated " + "literal secret"
 _ = func() string { return "repeated literal secret" }
 _ = []byte("repeated literal secret")
 type bytes []byte
 _ = bytes("repeated literal secret")
 _ = []rune("repeated literal secret")
 _ = "different literal secret"
}
func second() string { return "repeated literal secret" }
func init() { _ = "repeated literal secret" }
var callback = func() string { return "repeated literal secret" }
`
	for _, obf := range Obfuscators {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "repeat.go", source, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			previous := testPkgToObfuscatorMap
			testPkgToObfuscatorMap = map[string]obfuscator{"main": obf}
			defer func() { testPkgToObfuscatorMap = previous }()
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object)}
			if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			originalCount := len(file.Decls)
			Obfuscate(rand.New(rand.NewSource(1)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("%s%d", base, r.Uint64()) })
			decoders, helpers := 0, 0
			for _, decl := range file.Decls[originalCount:] {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					if strings.HasPrefix(fn.Name.Name, "literalDecoder") {
						decoders++
					} else if strings.HasPrefix(fn.Name.Name, "literalHelper") {
						helpers++
					}
				}
			}
			if decoders != 6 || helpers != 4 {
				t.Fatalf("got %d decoders and %d casters, want 6 and 4", decoders, helpers)
			}
			checked := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
			if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, checked); err != nil {
				t.Fatal(err)
			}
			claimed := make(map[string]string)
			for _, decl := range file.Decls[:originalCount] {
				owner := "package"
				if fn, ok := decl.(*ast.FuncDecl); ok {
					owner = fn.Name.Name
				}
				var calls []string
				nodes := make(map[ast.Node]bool)
				ast.Inspect(decl, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if _, ok := call.Fun.(*ast.SelectorExpr); !ok || checked.TypeOf(call) != types.Typ[types.String] {
						return true
					}
					var text strings.Builder
					if err := printer.Fprint(&text, fset, call); err != nil {
						t.Fatal(err)
					}
					key := text.String()
					calls = append(calls, key)
					if previous, ok := claimed[key]; ok && (previous != owner || owner == "package") {
						t.Errorf("decoder shared between %s and %s", previous, owner)
					}
					claimed[key] = owner
					for n := range ast.Preorder(call) {
						if nodes[n] {
							t.Error("repeated calls share mutable AST nodes")
						}
						nodes[n] = true
					}
					return false
				})
				if owner == "first" {
					if len(calls) != 7 {
						t.Fatalf("got %d decoder calls, want 7", len(calls))
					}
					for _, call := range calls[1:6] {
						if call != calls[0] {
							t.Error("equal strings in one original function do not share a decoder and proxy arguments")
						}
					}
					if calls[6] == calls[0] {
						t.Error("different strings share a decoder")
					}
				} else if len(calls) != 1 {
					t.Errorf("owner %s has %d calls, want 1", owner, len(calls))
				}
			}
		})
	}
}
