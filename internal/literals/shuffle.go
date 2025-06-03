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

func (shuffle) obfuscate(rand *mathrand.Rand, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	key := make([]byte, len(data))
	rand.Read(key)

	const (
		minIdxKeySize = 2
		maxIdxKeySize = 16
	)

	idxKeySize := minIdxKeySize
	if tmp := rand.Intn(len(data)); tmp > idxKeySize {
		idxKeySize = tmp
	}
	if idxKeySize > maxIdxKeySize {
		idxKeySize = maxIdxKeySize
	}

	idxKey := make([]byte, idxKeySize)
	rand.Read(idxKey)

	fullData := make([]byte, len(data)+len(key))
	operators := make([]token.Token, len(fullData))
	for i := range operators {
		operators[i] = randOperator(rand)
	}

	for i, b := range key {
		fullData[i], fullData[i+len(data)] = evalOperator(operators[i], data[i], b), b
	}

	shuffledIdxs := rand.Perm(len(fullData))

	shuffledFullData := make([]byte, len(fullData))
	for i, b := range fullData {
		shuffledFullData[shuffledIdxs[i]] = b
	}

	args := []ast.Expr{ast.NewIdent("data")}
	for i := range data {
		keyIdx := rand.Intn(idxKeySize)
		k := int(idxKey[keyIdx])

		args = append(args, operatorToReversedBinaryExpr(
			operators[i],
			ah.IndexExpr("fullData", &ast.BinaryExpr{X: ah.IntLit(shuffledIdxs[i] ^ k), Op: token.XOR, Y: ah.CallExprByName("int", ah.IndexExpr("idxKey", ah.IntLit(keyIdx)))}),
			ah.IndexExpr("fullData", &ast.BinaryExpr{X: ah.IntLit(shuffledIdxs[len(data)+i] ^ k), Op: token.XOR, Y: ah.CallExprByName("int", ah.IndexExpr("idxKey", ah.IntLit(keyIdx)))}),
		))
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("fullData")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSliceWithExtKeys(rand, shuffledFullData, extKeys)},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("idxKey")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSliceWithExtKeys(rand, idxKey, extKeys)},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("data")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.CallExpr(ast.NewIdent("make"), &ast.ArrayType{Elt: ast.NewIdent("byte")}, ah.IntLit(0), ah.IntLit(len(data)+1))},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("data")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{ah.CallExpr(ast.NewIdent("append"), args...)},
		},
	)
}
