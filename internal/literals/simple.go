// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type simple struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = simple{}

func (simple) obfuscate(rand *mathrand.Rand, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	key := make([]byte, len(data))
	rand.Read(key)

	op := randOperator(rand)
	for i, b := range key {
		data[i] = evalOperator(op, data[i], b)
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("key")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSliceWithExtKeys(rand, key, extKeys)},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("data")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSliceWithExtKeys(rand, data, extKeys)},
		},
		&ast.RangeStmt{
			Key:   ast.NewIdent("i"),
			Value: ast.NewIdent("b"),
			Tok:   token.DEFINE,
			X:     ast.NewIdent("key"),
			Body: &ast.BlockStmt{List: []ast.Stmt{
				&ast.AssignStmt{
					Lhs: []ast.Expr{ah.IndexExpr("data", ast.NewIdent("i"))},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{operatorToReversedBinaryExpr(op, ah.IndexExpr("data", ast.NewIdent("i")), ast.NewIdent("b"))},
				},
			}},
		},
	)
}
