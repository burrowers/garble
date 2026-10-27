// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type seed struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = seed{}

// The encrypted bytes are kept as one static payload which a single loop
// interprets. Building a call chain of one function value per byte made the
// generated code grow with the literal length: every byte became its own
// indirect call, which the linker keeps as text and per-function metadata.
func (seed) obfuscate(obfRand *mathrand.Rand, names *generatedNames, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	mask := byte(obfRand.Uint32())
	seed := byte(obfRand.Uint32())
	originalSeed := seed

	op := randOperator(obfRand)
	payload := make([]byte, len(data))
	for i, b := range data {
		encB := evalOperator(op, b, seed)
		seed += encB

		payload[i] = encB ^ mask
	}

	return ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("payload"), ah.StringLit(string(payload))),
		ah.AssignDefineStmt(names.ident("data"),
			ah.CallExprByName("make", ah.ByteSliceType(), ah.IntLit(len(data)))),
		ah.AssignDefineStmt(names.ident("mask"), ah.CallExprByName("byte", byteLitWithExtKey(obfRand, mask, extKeys, highProb))),
		ah.AssignDefineStmt(names.ident("seed"), ah.CallExprByName("byte", byteLitWithExtKey(obfRand, originalSeed, extKeys, highProb))),
		&ast.ForStmt{
			Init: ah.AssignDefineStmt(names.ident("i"), ah.IntLit(0)),
			Cond: ah.BinaryExpr(names.ident("i"), token.LSS, ah.IntLit(len(data))),
			Post: &ast.IncDecStmt{X: names.ident("i"), Tok: token.INC},
			Body: ah.BlockStmt(
				ah.AssignDefineStmt(names.ident("b"),
					ah.BinaryExpr(
						ah.IndexExpr(names.name("payload"), names.ident("i")),
						token.XOR,
						names.ident("mask"),
					),
				),
				ah.AssignStmt(
					ah.IndexExpr(names.name("data"), names.ident("i")),
					operatorToReversedBinaryExpr(op, names.ident("b"), names.ident("seed")),
				),
				&ast.AssignStmt{
					Lhs: []ast.Expr{names.ident("seed")},
					Tok: token.ADD_ASSIGN,
					Rhs: []ast.Expr{names.ident("b")},
				},
			),
		},
	)
}
