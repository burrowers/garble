// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"

	ah "mvdan.cc/garble/internal/asthelper"
)

type seed struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = seed{}

func (seed) obfuscate(data []byte) *ast.BlockStmt {
	seed := genRandByte()
	originalSeed := seed

	op := randOperator()

	var callExpr *ast.CallExpr
	for i, b := range data {
		encB := evalOperator(op, b, seed)
		seed += encB

		if i == 0 {
			callExpr = ah.CallExpr(ah.Ident("fnc"), ah.IntLit(int(encB)))
			continue
		}

		callExpr = ah.CallExpr(callExpr, ah.IntLit(int(encB)))
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
						Params: &ast.FieldList{List: []*ast.Field{
							{Type: ah.Ident("byte")},
						}},
						Results: &ast.FieldList{List: []*ast.Field{
							{Type: ah.Ident("decFunc")},
						}},
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
							Rhs: []ast.Expr{
								ah.CallExpr(ah.Ident("append"), ah.Ident("data"), operatorToReversedBinaryExpr(op, ah.Ident("x"), ah.Ident("seed"))),
							},
						},
						&ast.AssignStmt{
							Lhs: []ast.Expr{ah.Ident("seed")},
							Tok: token.ADD_ASSIGN,
							Rhs: []ast.Expr{ah.Ident("x")},
						},
						ah.ReturnStmt(ah.Ident("fnc")),
					),
				},
			},
		},
		ah.ExprStmt(callExpr),
	)
}
