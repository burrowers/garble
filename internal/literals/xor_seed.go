package literals

import (
	"go/ast"
	"go/token"
	"math"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type xorSeed struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = xorSeed{}

func (x xorSeed) obfuscate(data []byte) *ast.BlockStmt {
	seed := byte(mathrand.Intn(math.MaxUint8))
	originalSeed := seed

	var callExpr *ast.CallExpr

	for i, b := range data {
		encB := b ^ seed
		seed += seed ^ encB

		if i == 0 {
			callExpr = ah.CallExpr(ah.Ident("fnc"), ah.IntLit(int(encB)))
		} else {
			callExpr = ah.CallExpr(callExpr, ah.IntLit(int(encB)))
		}
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("seed")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.CallExpr(ah.Ident("byte"), ah.IntLit(int(originalSeed)))},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ah.Ident("data")},
					Type:  &ast.ArrayType{Elt: ah.Ident("byte")},
				}},
			},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.TYPE,
				Specs: []ast.Spec{&ast.TypeSpec{
					Name: ah.Ident("decFunc"),
					Type: &ast.FuncType{
						Params: &ast.FieldList{
							List: []*ast.Field{{
								Type: ah.Ident("byte"),
							}},
						},
						Results: &ast.FieldList{
							List: []*ast.Field{{
								Type: ah.Ident("decFunc"),
							}},
						},
					},
				}},
			},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ah.Ident("fnc")},
					Type:  ah.Ident("decFunc"),
				}},
			},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("fnc")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{
				&ast.FuncLit{
					Type: &ast.FuncType{
						Params: &ast.FieldList{
							List: []*ast.Field{{
								Names: []*ast.Ident{ah.Ident("x")},
								Type:  ah.Ident("byte"),
							}},
						},
						Results: &ast.FieldList{
							List: []*ast.Field{{
								Type: ah.Ident("decFunc"),
							}},
						},
					},
					Body: ah.BlockStmt(
						&ast.AssignStmt{
							Lhs: []ast.Expr{ah.Ident("data")},
							Tok: token.ASSIGN,
							Rhs: []ast.Expr{ah.CallExpr(ah.Ident("append"), ah.Ident("data"), &ast.BinaryExpr{
								X:  ah.Ident("x"),
								Op: token.XOR,
								Y:  ah.Ident("seed"),
							})},
						},
						&ast.AssignStmt{
							Lhs: []ast.Expr{ah.Ident("seed")},
							Tok: token.ADD_ASSIGN,
							Rhs: []ast.Expr{
								&ast.BinaryExpr{
									X:  ah.Ident("seed"),
									Op: token.XOR,
									Y:  ah.Ident("x"),
								},
							},
						},
						&ast.ReturnStmt{
							Results: []ast.Expr{ah.Ident("fnc")},
						},
					),
				},
			},
		},
		ah.ExprStmt(callExpr),
	)
}
