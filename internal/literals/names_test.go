package literals

import (
	"fmt"
	"go/ast"
	"go/token"
	"math/rand"
	"testing"
)

func TestStringDecodersObfuscateGeneratedNames(t *testing.T) {
	for _, obf := range Obfuscators {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			r := newObfRand(rand.New(rand.NewSource(1)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
			r.testObfuscator = obf
			obfuscateString(r, "long_enough_string")
			for _, decl := range r.liftedFuncs {
				ast.Inspect(decl, func(node ast.Node) bool {
					if id, ok := node.(*ast.Ident); ok {
						switch id.Name {
						case "data", "fullData", "seed", "decFunc", "fnc", "x", "decryptKey", "i", "y", "newdata":
							t.Errorf("generated identifier %q was not obfuscated", id.Name)
						}
					}
					return true
				})
			}
		})
	}
}

func TestLiftCallObfuscatesGeneratedLocals(t *testing.T) {
	r := newObfRand(rand.New(rand.NewSource(1)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
	block := &ast.BlockStmt{List: []ast.Stmt{
		&ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent("fullData")}, Tok: token.DEFINE, Rhs: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: `"secret"`}}},
		&ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent("data")}, Tok: token.DEFINE, Rhs: []ast.Expr{ast.NewIdent("fullData")}},
		&ast.ReturnStmt{Results: []ast.Expr{ast.NewIdent("data")}},
	}}
	r.liftCall(nil, ast.NewIdent("string"), block, nil)
	ast.Inspect(r.liftedFuncs[0], func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "fullData" || id.Name == "data") {
			t.Errorf("generated local %q was not obfuscated", id.Name)
		}
		return true
	})
}
