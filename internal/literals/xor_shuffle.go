package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type xorShuffle struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = xorShuffle{}

func (x xorShuffle) obfuscate(data []byte) *ast.BlockStmt {
	key := make([]byte, len(data))
	genRandBytes(key)

	fullData := make([]byte, len(data)+len(key))
	operators := make([]token.Token, len(fullData))
	for i := range operators {
		operators[i] = genRandOperator()
	}

	for i, b := range key {
		fullData[i], fullData[i+len(data)] = evalOperator(operators[i], data[i], b), b
	}

	shuffledIdxs := mathrand.Perm(len(fullData))

	shuffledFullData := make([]byte, len(fullData))
	for i, b := range fullData {
		shuffledFullData[shuffledIdxs[i]] = b
	}

	args := []ast.Expr{ah.Ident("data")}
	for i := range data {
		args = append(args, getReversedOperator(
			operators[i],
			ah.IndexExpr("fullData", ah.IntLit(shuffledIdxs[i])),
			ah.IndexExpr("fullData", ah.IntLit(shuffledIdxs[len(data)+i]))),
		)
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("fullData")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.DataToByteSlice(shuffledFullData)},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ah.Ident("data")},
					Type:  &ast.ArrayType{Elt: ah.Ident("byte")},
				}},
			},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("data")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{ah.CallExpr(ah.Ident("append"), args...)},
		},
	)
}
