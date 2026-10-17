package literals

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"
	"strings"
	"testing"
)

func TestDecodersObfuscateGeneratedNames(t *testing.T) {
	const literal = "long_enough_string"
	cases := []struct {
		name  string
		build func(*obfRand)
	}{
		{"string", func(r *obfRand) { obfuscateString(r, literal) }},
		{"slice", func(r *obfRand) { obfuscateByteSlice(r, false, []byte(literal)) }},
		{"slice pointer", func(r *obfRand) { obfuscateByteSlice(r, true, []byte(literal)) }},
		{"array", func(r *obfRand) { obfuscateByteArray(r, false, []byte(literal), int64(len(literal))) }},
		{"array pointer", func(r *obfRand) { obfuscateByteArray(r, true, []byte(literal), int64(len(literal))) }},
	}
	for _, obf := range Obfuscators {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%T/%s", obf, tc.name), func(t *testing.T) {
				r := newObfRand(rand.New(rand.NewSource(1)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
				r.testObfuscator = obf
				tc.build(r)
				for _, decl := range r.liftedFuncs {
					ast.Inspect(decl, func(node ast.Node) bool {
						if id, ok := node.(*ast.Ident); ok {
							switch id.Name {
							case "data", "key", "fullData", "idxKey", "positions", "localKey", "seed", "decFunc", "fnc", "x", "b", "decryptKey", "counter", "i", "y", "newdata", "garbleStringCaster", "operations", "mask", "handlers", "dst", "value":
								t.Errorf("generated identifier %q was not obfuscated", id.Name)
							}
							if strings.HasPrefix(id.Name, "garbleExternalKey") {
								t.Errorf("external key %q was not obfuscated", id.Name)
							}
						}
						return true
					})
				}
			})
		}
	}
}

// Each original function must reach only its own generated proxy declarations,
// including decoders for nested closures, methods and generic functions.
func TestFunctionOwnedProxyDispatchers(t *testing.T) {
	const source = `package main
var global = "global literal secret"
var callback = func() string { return "global callback secret" }
var initialized = first()
type receiver struct{}
func first() string { return "first literal secret" + string([]byte("another first secret")) }
func second() string { return "second literal secret" + func() string { return "nested literal secret" }() }
func (receiver) method() string { return "method literal secret" }
func generic[T any](v T) string { return "generic literal secret" }
func init() { global += "init literal secret" }
`
	for _, obf := range Obfuscators {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "owner.go", source, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			previous := testPkgToObfuscatorMap
			testPkgToObfuscatorMap = map[string]obfuscator{"main": obf}
			defer func() { testPkgToObfuscatorMap = previous }()
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
			if _, err := new(types.Config).Check("owners", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			liftedNames := make(map[string]bool)
			originalCount := len(file.Decls)
			Obfuscate(rand.New(rand.NewSource(1)), file, info, nil, nil, func(r *rand.Rand, base string) string {
				if strings.HasPrefix(base, "literalDecoder") || strings.HasPrefix(base, "literalHelper") {
					if liftedNames[base] {
						t.Errorf("lifted function base name reused across owners: %s", base)
					}
					liftedNames[base] = true
				}
				return fmt.Sprintf("%s%d", base, r.Uint64())
			})
			// Recheck the generated AST, including uniqueness of lifted names and
			// the function types stored in the proxy trees.
			if _, err := new(types.Config).Check("owners", fset, []*ast.File{file}, nil); err != nil {
				t.Fatal(err)
			}
			decls := make(map[string]ast.Node)
			var owners []*ast.FuncDecl
			for i, decl := range file.Decls {
				switch decl := decl.(type) {
				case *ast.FuncDecl:
					if i < originalCount {
						owners = append(owners, decl)
					} else {
						decls[decl.Name.Name] = decl
					}
				case *ast.GenDecl:
					if i >= originalCount && decl.Tok == token.VAR {
						for _, spec := range decl.Specs {
							spec := spec.(*ast.ValueSpec)
							for _, name := range spec.Names {
								decls[name.Name] = spec
							}
						}
					}
				}
			}
			claimed := make(map[string]string)
			for _, fn := range owners {
				owner := fn.Name.Name
				seen := make(map[string]bool)
				var visit func(ast.Node)
				visit = func(node ast.Node) {
					ast.Inspect(node, func(node ast.Node) bool {
						id, ok := node.(*ast.Ident)
						if !ok || seen[id.Name] {
							return true
						}
						if decl := decls[id.Name]; decl != nil {
							seen[id.Name] = true
							if previous, ok := claimed[id.Name]; ok && previous != owner {
								t.Errorf("owners %s and %s share generated declaration %s", previous, owner, id.Name)
							}
							claimed[id.Name] = owner
							visit(decl)
						}
						return true
					})
				}
				visit(fn.Body)
				helpers := 0
				for name := range seen {
					if strings.HasPrefix(name, "literalHelper") {
						helpers++
					}
				}
				if helpers != 1 {
					t.Errorf("owner %s reaches %d string adapters, want 1", owner, helpers)
				}
				if len(seen) == 0 {
					t.Errorf("owner %s has no hidden decoder", owner)
				}
			}
		})
	}
}

func TestGeneratedLocalNames(t *testing.T) {
	r := newObfRand(rand.New(rand.NewSource(1)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
	names := newGeneratedNames(r)
	first := names.ident("data")
	if first.Name == "data" || names.ident("data").Name != first.Name {
		t.Fatalf("generated local was not consistently named: %q", first.Name)
	}
	if names.ident("other").Name == first.Name {
		t.Fatal("distinct generated locals have the same name")
	}
}
