package obfuscators

import (
	"go/ast"
	"go/token"
)

type xor struct{}

// check that the obfuscator interface is implemented
var _ Obfuscator = xor{}

func (x xor) Obfuscate(data []byte) *ast.BlockStmt {
	key := make([]byte, len(data))
	genRandBytes(key)

	for i, b := range key {
		data[i] = data[i] ^ b
	}

	return &ast.BlockStmt{
		List: []ast.Stmt{
			&ast.AssignStmt{
				Lhs: []ast.Expr{
					&ast.Ident{Name: "key"},
				},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{dataToByteSlice(key)},
			},
			&ast.AssignStmt{
				Lhs: []ast.Expr{
					&ast.Ident{Name: "data"},
				},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{dataToByteSlice(data)},
			},
			&ast.RangeStmt{
				Key:   &ast.Ident{Name: "i"},
				Value: &ast.Ident{Name: "b"},
				Tok:   token.DEFINE,
				X:     &ast.Ident{Name: "key"},
				Body: &ast.BlockStmt{
					List: []ast.Stmt{
						&ast.AssignStmt{
							Lhs: []ast.Expr{
								&ast.IndexExpr{
									X:     &ast.Ident{Name: "data"},
									Index: &ast.Ident{Name: "i"},
								},
							},
							Tok: token.ASSIGN,
							Rhs: []ast.Expr{
								&ast.BinaryExpr{
									X: &ast.IndexExpr{
										X:     &ast.Ident{Name: "data"},
										Index: &ast.Ident{Name: "i"},
									},
									Op: token.XOR,
									Y:  &ast.Ident{Name: "b"},
								},
							},
						},
					},
				},
			},
		},
	}

}
