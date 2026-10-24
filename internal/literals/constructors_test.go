// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

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

func TestGlobalConstructor(t *testing.T) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "constructors.go", `package constructors
 type Key string
 type Value struct { Text string }
 type Pair struct { Key Key; Value Value }
 const K1 Key = "enum key number 1"
 func StringValue(v string) Value { return Value{v} }
 func (k Key) String(v string) Pair { return Pair{k, StringValue(v)} }
 var V1 = K1.String("enum value number 1")
 var mutableKey Key
 var Reference = mutableKey.String("must retain a variable receiver")
 func effect(v string) Pair { println(v); return Pair{} }
 var Effect = effect("must retain constructor side effects")
 func read(v string) Pair { return Pair{mutableKey, StringValue(v)} }
 var Read = read("must retain a global-reading constructor")
 var Indirect = K1.String
 var Unknown = Indirect("must retain an indirect constructor")
 `, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
	if _, err := new(types.Config).Check("constructors", fs, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	_, inits := ObfuscateWithGlobalInits(rand.New(rand.NewSource(1)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%x", base, r.Uint64()) })
	if len(inits) != 1 || inits[0].Global.Name != "V1" {
		t.Fatalf("constructor global not certified: %v", inits)
	}
	if _, err := new(types.Config).Check("constructors", fs, []*ast.File{file}, nil); err != nil {
		t.Fatal(err)
	}
}
