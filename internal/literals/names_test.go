package literals

import (
	"go/ast"
	"go/token"
	"math/rand"
	"testing"
)

func TestLiftCallObfuscatesGeneratedLocals(t *testing.T) {
	r := newObfRand(rand.New(rand.NewSource(1)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
	block := &ast.BlockStmt{List: []ast.Stmt{
		&ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent("fullData")}, Tok: token.DEFINE, Rhs: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: `"secret"`}}},
		&ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent("data")}, Tok: token.DEFINE, Rhs: []ast.Expr{ast.NewIdent("fullData")}},
		&ast.ReturnStmt{Results: []ast.Expr{ast.NewIdent("data")}},
	}}
	r.liftCall(nil, ast.NewIdent("string"), block, nil)
	foundRawLocal := false
	ast.Inspect(r.liftedFuncs[0], func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "fullData" || id.Name == "data") {
			foundRawLocal = true
		}
		return true
	})
	if !foundRawLocal {
		t.Fatal("TODO: generated locals are no longer left unobfuscated")
	}
}
