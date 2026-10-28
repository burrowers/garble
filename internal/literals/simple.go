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

func (simple) obfuscate(rand *mathrand.Rand, names *generatedNames, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	key := make([]byte, len(data))
	rand.Read(key)

	op := randOperator(rand)
	for i, b := range key {
		data[i] = evalOperator(op, data[i], b)
	}

	return ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("key"), dataToByteSliceWithExtKeys(rand, names, key, extKeys)),
		ah.AssignDefineStmt(names.ident("data"), dataToByteSliceWithExtKeys(rand, names, data, extKeys)),
		&ast.RangeStmt{
			Key:   names.ident("i"),
			Value: names.ident("b"),
			Tok:   token.DEFINE,
			X:     names.ident("key"),
			Body: ah.BlockStmt(ah.AssignStmt(
				ah.IndexExpr(names.name("data"), names.ident("i")),
				operatorToReversedBinaryExpr(op, ah.IndexExpr(names.name("data"), names.ident("i")), names.ident("b")),
			)),
		},
	)
}
