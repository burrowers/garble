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
 `, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
	if _, err := new(types.Config).Check("constructors", fs, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	_, inits := ObfuscateWithGlobalInits(rand.New(rand.NewSource(1)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%x", base, r.Uint64()) })
	// TODO: certify the body-proven constructor and remove this retention check.
	if len(inits) != 0 {
		t.Fatalf("unexpected constructor certificates: %v", inits)
	}
}
