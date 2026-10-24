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

func TestConstructorSummarySafety(t *testing.T) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "safety.go", `package safety
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
 `, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
	if _, err := new(types.Config).Check("safety", fs, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
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

func TestGlobalLiteralProxyOwnership(t *testing.T) {
	for _, obf := range Obfuscators {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			fset := token.NewFileSet()
			long := ""
			for range 100 {
				long += "an oversized concatenation operand "
			}
			long = long[:MaxSize]
			file, err := parser.ParseFile(fset, "global.go", fmt.Sprintf(`package globals
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
`, long, long), 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
			if _, err := new(types.Config).Check("globals", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			old := testPkgToObfuscatorMap
			testPkgToObfuscatorMap = map[string]obfuscator{"globals": obf}
			defer func() { testPkgToObfuscatorMap = old }()
			file, inits := ObfuscateWithGlobalInits(rand.New(rand.NewSource(1)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%x", base, r.Uint64()) })
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
			if len(transformed.InitOrder) < 2 || transformed.InitOrder[0].Lhs[0].Name() != "late" || transformed.InitOrder[1].Lhs[0].Name() != "early" {
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
