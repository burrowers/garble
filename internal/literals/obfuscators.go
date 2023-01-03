// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"fmt"
	"go/ast"
	"go/token"
	mathrand "math/rand"
)

// obfuscator takes a byte slice and converts it to a ast.BlockStmt
type obfuscator interface {
	obfuscate(obfRand *mathrand.Rand, data []byte) *ast.BlockStmt
}

// obfuscators contains all types which implement the obfuscator Interface
var obfuscators = []obfuscator{
	simple{},
	swap{},
	split{},
	shuffle{},
	// seed{}, TODO: re-enable once https://go.dev/issue/47631 is fixed in Go 1.20
}

func genRandIntSlice(obfRand *mathrand.Rand, max, count int) []int {
	indexes := make([]int, count)
	for i := 0; i < count; i++ {
		indexes[i] = obfRand.Intn(max)
	}
	return indexes
}

func randOperator(obfRand *mathrand.Rand) token.Token {
	operatorTokens := [...]token.Token{token.XOR, token.ADD, token.SUB}
	return operatorTokens[obfRand.Intn(len(operatorTokens))]
}

func evalOperator(t token.Token, x, y byte) byte {
	switch t {
	case token.XOR:
		return x ^ y
	case token.ADD:
		return x + y
	case token.SUB:
		return x - y
	default:
		panic(fmt.Sprintf("unknown operator: %s", t))
	}
}

func operatorToReversedBinaryExpr(t token.Token, x, y ast.Expr) *ast.BinaryExpr {
	expr := &ast.BinaryExpr{X: x, Y: y}

	switch t {
	case token.XOR:
		expr.Op = token.XOR
	case token.ADD:
		expr.Op = token.SUB
	case token.SUB:
		expr.Op = token.ADD
	default:
		panic(fmt.Sprintf("unknown operator: %s", t))
	}

	return expr
}
