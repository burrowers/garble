package literals

import (
	"go/ast"
	"go/token"

	ah "mvdan.cc/garble/internal/asthelper"
)

type xor struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = xor{}

func (x xor) obfuscate(data []byte) *ast.BlockStmt {
	key := make([]byte, len(data))
	genRandBytes(key)

	op := genRandOperator()
	for i, b := range key {
		data[i] = evalOperator(op, data[i], b)
	}

	return &ast.BlockStmt{List: []ast.Stmt{
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("key")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.DataToByteSlice(key)},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("data")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.DataToByteSlice(data)},
		},
		&ast.RangeStmt{
			Key:   ah.Ident("i"),
			Value: ah.Ident("b"),
			Tok:   token.DEFINE,
			X:     ah.Ident("key"),
			Body: &ast.BlockStmt{List: []ast.Stmt{
				&ast.AssignStmt{
					Lhs: []ast.Expr{ah.IndexExpr("data", ah.Ident("i"))},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{getReversedOperator(op, ah.IndexExpr("data", ah.Ident("i")), ah.Ident("b"))},
				},
			}},
		},
	}}
}
