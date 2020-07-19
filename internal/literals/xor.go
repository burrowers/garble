package literals

import (
	"go/ast"
	"go/token"
)

type xor struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = xor{}

func (x xor) obfuscate(data []byte) *ast.BlockStmt {
	key := make([]byte, len(data))
	genRandBytes(key)

	for i, b := range key {
		data[i] = data[i] ^ b
	}

	return &ast.BlockStmt{List: []ast.Stmt{
		&ast.AssignStmt{
			Lhs: []ast.Expr{ident("key")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSlice(key)},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ident("data")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSlice(data)},
		},
		&ast.RangeStmt{
			Key:   ident("i"),
			Value: ident("b"),
			Tok:   token.DEFINE,
			X:     ident("key"),
			Body: &ast.BlockStmt{List: []ast.Stmt{
				&ast.AssignStmt{
					Lhs: []ast.Expr{indexExpr("data", ident("i"))},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{&ast.BinaryExpr{
						X:  indexExpr("data", ident("i")),
						Op: token.XOR,
						Y:  ident("b"),
					}},
				},
			}},
		},
	}}
}
