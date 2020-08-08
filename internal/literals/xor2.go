package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type xor2 struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = xor2{}

func (x xor2) obfuscate(data []byte) *ast.BlockStmt {
	key := make([]byte, len(data))
	genRandBytes(key)

	fullData := make([]byte, len(data))
	for i, b := range key {
		fullData[i] = data[i] ^ b
	}
	fullData = append(fullData, key...)

	shuffledIdxs := mathrand.Perm(len(fullData))

	shuffledFullData := make([]byte, len(fullData))
	for i := range fullData {
		shuffledFullData[shuffledIdxs[i]] = fullData[i]
	}

	args := []ast.Expr{ah.Ident("data")}
	for i := range data {
		args = append(args, &ast.BinaryExpr{
			X:  ah.IndexExpr("fullData", ah.IntLit(shuffledIdxs[i])),
			Op: token.XOR,
			Y:  ah.IndexExpr("fullData", ah.IntLit(shuffledIdxs[len(data)+i])),
		})
	}

	return &ast.BlockStmt{List: []ast.Stmt{
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
	}}
}
