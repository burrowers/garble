// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	"math"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type swap struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = swap{}

func getIndexType(dataLen int64) string {
	switch {
	case dataLen <= math.MaxUint8:
		return "byte"
	case dataLen <= math.MaxUint16:
		return "uint16"
	case dataLen <= math.MaxUint32:
		return "uint32"
	default:
		return "uint64"
	}
}

func positionsToSlice(data []int) *ast.CompositeLit {
	arr := &ast.CompositeLit{
		Type: &ast.ArrayType{
			Len: &ast.Ellipsis{}, // Performance optimization
			Elt: ast.NewIdent(getIndexType(int64(len(data)))),
		},
		Elts: []ast.Expr{},
	}
	for _, data := range data {
		arr.Elts = append(arr.Elts, ah.IntLit(data))
	}
	return arr
}

// Generates a random even swap count based on the length of data
func generateSwapCount(obfRand *mathrand.Rand, dataLen int) int {
	swapCount := dataLen

	maxExtraPositions := dataLen / 2 // Limit the number of extra positions to half the data length
	if maxExtraPositions > 1 {
		swapCount += obfRand.Intn(maxExtraPositions)
	}
	if swapCount%2 != 0 { // Swap count must be even
		swapCount++
	}
	return swapCount
}

func (swap) obfuscate(rand *mathrand.Rand, names *generatedNames, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	swapCount := generateSwapCount(rand, len(data))
	shiftKey := byte(rand.Uint32())

	op := randOperator(rand)

	positions := genRandIntSlice(rand, len(data), swapCount)
	for i := len(positions) - 2; i >= 0; i -= 2 {
		// Generate local key for xor based on random key and byte position
		localKey := byte(i) + byte(positions[i]^positions[i+1]) + shiftKey
		// Swap bytes from i+1 to i and encrypt using operator and local key
		data[positions[i]], data[positions[i+1]] = evalOperator(op, data[positions[i+1]], localKey), evalOperator(op, data[positions[i]], localKey)
	}

	return ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("data"), dataToByteSliceWithExtKeys(rand, names, data, extKeys)),
		ah.AssignDefineStmt(names.ident("positions"), positionsToSlice(positions)),
		&ast.ForStmt{
			Init: ah.AssignDefineStmt(names.ident("i"), ah.IntLit(0)),
			Cond: ah.BinaryExpr(names.ident("i"), token.LSS, ah.IntLit(len(positions))),
			Post: &ast.AssignStmt{
				Lhs: []ast.Expr{names.ident("i")},
				Tok: token.ADD_ASSIGN,
				Rhs: []ast.Expr{ah.IntLit(2)},
			},
			Body: ah.BlockStmt(
				ah.AssignDefineStmt(
					names.ident("localKey"),
					ah.BinaryExpr(
						ah.BinaryExpr(
							ah.CallExpr(ast.NewIdent("byte"), names.ident("i")),
							token.ADD,
							ah.CallExpr(ast.NewIdent("byte"), ah.BinaryExpr(
								ah.IndexExpr(names.name("positions"), names.ident("i")),
								token.XOR,
								ah.IndexExpr(names.name("positions"), ah.BinaryExpr(names.ident("i"), token.ADD, ah.IntLit(1))),
							)),
						),
						token.ADD,
						byteLitWithExtKey(rand, shiftKey, extKeys, highProb),
					),
				),
				&ast.AssignStmt{
					Lhs: []ast.Expr{
						ah.IndexExpr(names.name("data"), ah.IndexExpr(names.name("positions"), names.ident("i"))),
						ah.IndexExpr(names.name("data"), ah.IndexExpr(names.name("positions"), ah.BinaryExpr(names.ident("i"), token.ADD, ah.IntLit(1)))),
					},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{
						operatorToReversedBinaryExpr(
							op,
							ah.IndexExpr(names.name("data"),
								ah.IndexExpr(names.name("positions"), ah.BinaryExpr(names.ident("i"), token.ADD, ah.IntLit(1))),
							),
							names.ident("localKey"),
						),
						operatorToReversedBinaryExpr(
							op,
							ah.IndexExpr(names.name("data"), ah.IndexExpr(names.name("positions"), names.ident("i"))),
							names.ident("localKey"),
						),
					},
				},
			),
		},
	)
}
