// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type shuffle struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = shuffle{}

func (shuffle) obfuscate(data []byte) *ast.BlockStmt {
	key := make([]byte, len(data))
	genRandBytes(key)

	fullData := make([]byte, len(data)+len(key))
	operators := make([]token.Token, len(fullData))
	for i := range operators {
		operators[i] = randOperator()
	}

	for i, b := range key {
		fullData[i], fullData[i+len(data)] = evalOperator(operators[i], data[i], b), b
	}

	shuffledIdxs := mathrand.Perm(len(fullData))

	shuffledFullData := make([]byte, len(fullData))
	for i, b := range fullData {
		shuffledFullData[shuffledIdxs[i]] = b
	}

	args := []ast.Expr{ast.NewIdent("data")}
	for i := range data {
		args = append(args, operatorToReversedBinaryExpr(
			operators[i],
			ah.IndexExpr("fullData", ah.IntLit(shuffledIdxs[i])),
			ah.IndexExpr("fullData", ah.IntLit(shuffledIdxs[len(data)+i])),
		))
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("fullData")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.DataToByteSlice(shuffledFullData)},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent("data")},
					Type:  &ast.ArrayType{Elt: ast.NewIdent("byte")},
				}},
			},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("data")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{ah.CallExpr(ast.NewIdent("append"), args...)},
		},
	)
}
