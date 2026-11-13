// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "github.com/burrowers/garble/internal/asthelper"
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

	op := randOperator(names.codeRand(obfRand))
	payload := encryptSeed(data, op, mask, seed)
	return seedBody(names, op, names.input(ah.StringLit(string(payload)), ast.NewIdent("string")),
		ah.CallExprByName("byte", names.byteLit(obfRand, mask, extKeys, highProb)),
		ah.CallExprByName("byte", names.byteLit(obfRand, seed, extKeys, highProb)))
}

func encryptSeed(data []byte, op token.Token, mask, seed byte) []byte {
	payload := make([]byte, len(data))
	for i, b := range data {
		encB := evalOperator(op, b, seed)
		seed += encB
		payload[i] = encB ^ mask
	}
	return payload
}

// seedBody serves both per-literal decoders and owner-shared short decoders.
// Its inputs can be constants or parameters without changing the algorithm.
func seedBody(names *generatedNames, op token.Token, payload, mask, seed ast.Expr) *ast.BlockStmt {
	return ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("payload"), payload),
		ah.AssignDefineStmt(names.ident("data"), ah.CallExprByName("make", ah.ByteSliceType(), ah.CallExprByName("len", names.ident("payload")))),
		ah.AssignDefineStmt(names.ident("mask"), mask),
		ah.AssignDefineStmt(names.ident("seed"), seed),
		&ast.ForStmt{
			Init: ah.AssignDefineStmt(names.ident("i"), ah.IntLit(0)),
			Cond: ah.BinaryExpr(names.ident("i"), token.LSS, ah.CallExprByName("len", names.ident("payload"))),
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
