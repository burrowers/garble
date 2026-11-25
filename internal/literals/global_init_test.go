package literals

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"math/rand"
	"testing"
)

func TestPackageRootsPrecedeInitializers(t *testing.T) {
	// The first variable decodes a literal, the second only reads state the
	// first one writes. Package initialization runs the earliest declared
	// variable whose dependencies are ready, so a generated proxy root declared
	// after the second variable would silently put the second one first and it
	// would read the state before the first initializer wrote it.
	const source = `package order
var log []string
var first = pick("an obfuscated literal payload")
var second = len(log)
func pick(v string) string { return v + log[0] }
`
	for _, obf := range Obfuscators {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
			file, fset := CheckSource(t, "order", "order.go", source, 0, info)
			useObfuscator(t, file.Name.Name, obf)
			file = Obfuscate(rand.New(rand.NewSource(1)), file, info, nil, nil, nil, func(r *rand.Rand, base string) string {
				return fmt.Sprintf("hidden_%s_%x", base, r.Uint64())
			})

			transformed := &types.Info{}
			if _, err := new(types.Config).Check("order", fset, []*ast.File{file}, transformed); err != nil {
				t.Fatal(err)
			}
			var order []string
			for _, init := range transformed.InitOrder {
				name := init.Lhs[0].Name()
				if name == "first" || name == "second" {
					order = append(order, name)
				}
			}
			want := []string{"first", "second"}
			if fmt.Sprint(order) != fmt.Sprint(want) {
				t.Fatalf("initialization order = %v, want %v", order, want)
			}
		})
	}
}

func TestConstructorSummarySafety(t *testing.T) {
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
	file, _ := CheckSource(t, "safety", "safety.go", `package safety
 type Key string
 type Pair struct { Text string }
 var global = "observable global value"
 var array [1]string
 var callback = Good
 func Good(v string) Pair { return Pair{v} }
 func Nested(v string) Pair { return Good(v) }
 func Convert(v Key) Pair { return Pair{string(v)} }
 func Read(v string) Pair { return Pair{global} }
 func Write(v string) Pair { global = v; return Pair{v} }
 func Panic(v string) Pair { panic(v) }
 func Index(v string) Pair { return Pair{v[:1]} }
 func Indirect(v string) Pair { return callback(v) }
 func Rec(v string) Pair { return Rec(v) }
 func RecA(v string) Pair { return RecB(v) }
 func RecB(v string) Pair { return RecA(v) }
 func Loop(v string) Pair { for {} }
 func Dependency(v string) Pair { return Pair{string(len(array))} }
 func Pointer(v *Key) Pair { return v.String("constant argument") }
 func (k Key) String(v string) Pair { return Good(v) }
 func (k *Key) Other(v string) Pair { return Good(v) }
 type Embedded struct { *Key }
 func Promoted(v Embedded) Pair { return v.String("constant argument") }
 func MethodExpr(v Key) Pair { return Key.String(v, "constant argument") }
 func Slice(v string) []string { return []string{v} }
 func Divide(v int) int { return 1/v }
 func Assert(v any) string { return v.(string) }
 //go:linkname Linked
 func Linked(v string) Pair { return Pair{v} }
 `, parser.ParseComments, info)
	summaries := ConstructorSummaries([]*ast.File{file}, info, nil)
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			obj := info.Defs[fn.Name].(*types.Func)
			want := fn.Name.Name == "Good" || fn.Name.Name == "Nested" || fn.Name.Name == "Convert" || fn.Name.Name == "String"
			if got := summaries[obj.FullName()]; got != want {
				t.Errorf("%s: pure = %v, want %v", obj.FullName(), got, want)
			}
		}
	}
}

func TestConstructorResultTypes(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "results.go", `package results
	type String string
	type Array [1]string
	type Struct struct { Text string }
	type Map map[string]string
	type Interface interface { Apply() }
	type EmptyInterface interface{}
	type Slice []string
	type Pointer *string
	type Func func() string
	type Chan chan string
	type Bool bool
	type Int int
	type Float float64
	type Complex complex128
	type InterfaceAlias = Interface
	type StringAlias = String
	type Generic[T any] string
	`, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
	pkg, err := new(types.Config).Check("results", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("Instantiation", func(t *testing.T) {
		// A factory's result type has to match the compiler's certified
		// initializer contract, which an instantiation cannot spell.
		inst, err := types.Instantiate(nil, pkg.Scope().Lookup("Generic").Type(), []types.Type{types.Typ[types.Int]}, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := constructorResult(file, info, inst); got != nil {
			t.Fatalf("constructor result of an instantiation: got %v, want nil", got)
		}
	})
	for _, name := range pkg.Scope().Names() {
		t.Run(name, func(t *testing.T) {
			want := name == "String" || name == "StringAlias" || name == "Array" || name == "Struct" || name == "Map"
			if got := constructorResult(file, info, pkg.Scope().Lookup(name).Type()); (got != nil) != want {
				t.Fatalf("constructor result %s: got %v, supported = %v", name, got, want)
			}
		})
	}
}

// TestNamedTypeExprSpellings covers the type forms the ordinary named-string
// conversion can be written with. Every identifier it emits has to carry its
// object, because the renaming pass rewrites names through info.Uses; a form it
// cannot bind is not emitted at all, which leaves that literal in plaintext.
func TestNamedTypeExprSpellings(t *testing.T) {
	const source = `package spell
type Named[T ~string] string
type Any2[T any] string
type Pair[A, B any] string
func generic[S ~string]() {}
`
	info := &types.Info{Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
	file, fset := CheckSource(t, "spell", "spell.go", source, 0, info)
	named := map[string]types.Type{}
	var param *types.TypeParam
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					named[ts.Name.Name] = info.Defs[ts.Name].Type()
				}
			}
		case *ast.FuncDecl:
			param = info.Defs[decl.Name].(*types.Func).Signature().TypeParams().At(0)
		}
	}
	inst := func(t *testing.T, orig types.Type, args ...types.Type) types.Type {
		t.Helper()
		typ, err := types.Instantiate(nil, orig, args, false)
		if err != nil {
			t.Fatal(err)
		}
		return typ
	}
	intType := types.Typ[types.Int]
	stringType := types.Typ[types.String]
	namedString := inst(t, named["Named"], stringType)
	tests := []struct {
		name string
		typ  types.Type
		want string // empty means the spelling is declined
	}{
		{"plain", named["Named"], "Named"},
		{"basic argument", namedString, "Named[string]"},
		{"nested argument", inst(t, named["Named"], namedString), "Named[Named[string]]"},
		{"type parameter argument", inst(t, named["Named"], param), "Named[S]"},
		{"two arguments", inst(t, named["Pair"], intType, stringType), "Pair[int, string]"},
		{"slice argument", inst(t, named["Any2"], types.NewSlice(intType)), "Any2[[]int]"},
		{"map argument", inst(t, named["Any2"], types.NewMap(stringType, intType)), "Any2[map[string]int]"},
		{"chan argument", inst(t, named["Any2"], types.NewChan(types.SendOnly, intType)), "Any2[chan<- int]"},
		{"array argument", inst(t, named["Any2"], types.NewArray(intType, 3)), "Any2[[3]int]"},
		{"pointer argument", inst(t, named["Any2"], types.NewPointer(intType)), "Any2[*int]"},
		// A struct or unsafe argument has no spelling here, so the literal keeps
		// its source text rather than receiving an unresolvable name.
		{"struct argument", inst(t, named["Any2"], types.NewStruct(nil, nil)), ""},
		{"unsafe argument", inst(t, named["Any2"], types.Typ[types.UnsafePointer]), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expr := namedTypeExpr(file, info, tc.typ)
			if tc.want == "" {
				if expr != nil {
					t.Fatalf("got %s, want no spelling", spell(t, fset, expr))
				}
				return
			}
			if expr == nil {
				t.Fatalf("no spelling, want %s", tc.want)
			}
			if got := spell(t, fset, expr); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			ast.Inspect(expr, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || info.Uses[id] != nil || types.Universe.Lookup(id.Name) != nil {
					return true
				}
				t.Errorf("%s is not bound to an object", id.Name)
				return true
			})
		})
	}
}

// spell prints a generated type expression without line breaks.
func spell(t *testing.T, fset *token.FileSet, expr ast.Expr) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestGlobalLiteralProxyOwnership(t *testing.T) {
	for _, obf := range Obfuscators {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			long := ""
			for range 100 {
				long += "an oversized concatenation operand "
			}
			long = long[:MaxSize]
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
			file, fset := CheckSource(t, "globals", "global.go", fmt.Sprintf(`package globals
var early = len([1]string{late})
var late = "initialized through an unevaluated operand"
type record struct { Text string; Count int; Nested [2]string }
var scalarDead = "an independently removable string global"
var scalarLive = "another independently initialized string"
var dead = [...]record{{Text: "an independently removable aggregate", Count: 3, Nested: [2]string{1: "a nested string constant"}}}
var live = record{"another eagerly initialized aggregate", 4, [2]string{"nested live string constant", "short"}}
var side = record{Text: effect()}
var reference = [1]string{live.Text}
var slice = []string{"not a certified slice global"}
var pointer = &record{Text: "not a certified pointer global"}
type namedString string
var named = struct{ Text string; Named namedString }{"an aggregate with a named conversion", namedString("a named conversion operand must stay obfuscated")}
var dependency = [1]string{string(make([]byte, len(reference)))}
var unevaluated = [1]int{len([1]string{live.Text})}
var oversized = [2]string{"an eligible small string", string(%q) + string(%q)}
var deadMap = map[string]rune{"an independently removable map key": 'x'}
var liveMap = map[string]rune{"an eagerly initialized map key": 'y'}
var sideMap = map[string]string{"a map with a user call": effect()}
var refMap = map[string]string{"a map with a variable": live.Text}
var keyRefMap = map[string]string{live.Text: "a constant value with a variable key"}
var keyCallMap = map[string]string{effect(): "a constant value with a user call key"}
type Key string
type Value struct { Text string }
type Pair struct { Key Key; Value Value }
const K1 Key = "enum key number 1"
func StringValue(v string) Value { return Value{v} }
func (k Key) String(v string) Pair { return Pair{k, StringValue(v)} }
var constructor = K1.String("enum value number 1")
var mutableKey Key
var receiver = mutableKey.String("must retain a variable receiver")
func sideEffect(v string) Pair { println(v); return Pair{} }
var constructorEffect = sideEffect("must retain constructor side effects")
func read(v string) Pair { return Pair{mutableKey, StringValue(v)} }
var constructorRead = read("must retain a global-reading constructor")
var indirect = K1.String
var unknown = indirect("must retain an indirect constructor")
func effect() string { return "a user call is not pure" }
`, long, long), 0, info)
			useObfuscator(t, file.Name.Name, obf)
			file, inits := ObfuscateWithGlobalInits(rand.New(rand.NewSource(1)), file, info, nil, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%x", base, r.Uint64()) })
			want := []string{"late", "scalarDead", "scalarLive", "dead", "live", "oversized", "deadMap", "liveMap", "constructor"}
			if len(inits) != len(want) {
				t.Fatalf("unexpected certificates: %v", inits)
			}
			factories := make(map[string]bool)
			for i, init := range inits {
				if init.Global.Name != want[i] || factories[init.Factory] {
					t.Fatalf("globals lack exclusive certificates: %v", inits)
				}
				factories[init.Factory] = true
			}
			vars := 0
			for _, decl := range file.Decls {
				if decl, ok := decl.(*ast.GenDecl); ok && decl.Tok == token.VAR {
					vars += len(decl.Specs)
				}
			}
			// Only the uncertified initializers retain package-level proxy roots.
			if vars != 29 {
				t.Fatalf("got %d package variables; want 27 globals and two shared roots", vars)
			}
			transformed := &types.Info{}
			if _, err := new(types.Config).Check("globals", fset, []*ast.File{file}, transformed); err != nil {
				t.Fatal(err)
			}
			// len does not evaluate its operand, but late must still precede early.
			var userOrder []string
			for _, init := range transformed.InitOrder {
				name := init.Lhs[0].Name()
				if name == "late" || name == "early" {
					userOrder = append(userOrder, name)
				}
			}
			if len(userOrder) != 2 || userOrder[0] != "late" || userOrder[1] != "early" {
				t.Fatalf("unevaluated operand lost its init dependency: %v", transformed.InitOrder)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == fmt.Sprintf("%q", long) {
					t.Error("oversized concatenation operands lost their obfuscation")
				}
				return true
			})
			for _, init := range inits {
				for _, decl := range file.Decls {
					if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == init.Factory {
						if len(fn.Body.List) < 2 {
							t.Fatal("aggregate factory lost its local proxy roots")
						}
					}
				}
			}
		})
	}
}
