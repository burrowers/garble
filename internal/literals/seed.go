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

func (seed) obfuscate(obfRand *mathrand.Rand, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	seed := byte(obfRand.Uint32())
	originalSeed := seed

	op := randOperator(obfRand)
	var callExpr *ast.CallExpr
	for i, b := range data {
		encB := evalOperator(op, b, seed)
		seed += encB

		if i == 0 {
			callExpr = ah.CallExpr(ast.NewIdent("fnc"), byteLitWithExtKey(obfRand, encB, extKeys, highProb))
			continue
		}

		callExpr = ah.CallExpr(callExpr, byteLitWithExtKey(obfRand, encB, extKeys, lowProb))
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("seed")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.CallExprByName("byte", byteLitWithExtKey(obfRand, originalSeed, extKeys, highProb))},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent("data")},
					Type:  &ast.ArrayType{Elt: ast.NewIdent("byte")},
				}},
			},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.TYPE,
				Specs: []ast.Spec{&ast.TypeSpec{
					Name: ast.NewIdent("decFunc"),
					Type: &ast.FuncType{
						Params: &ast.FieldList{List: []*ast.Field{
							{Type: ast.NewIdent("byte")},
						}},
						Results: &ast.FieldList{List: []*ast.Field{
							{Type: ast.NewIdent("decFunc")},
						}},
					},
				}},
			},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent("fnc")},
					Type:  ast.NewIdent("decFunc"),
				}},
			},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("fnc")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{
				&ast.FuncLit{
					Type: &ast.FuncType{
						Params: &ast.FieldList{
							List: []*ast.Field{{
								Names: []*ast.Ident{ast.NewIdent("x")},
								Type:  ast.NewIdent("byte"),
							}},
						},
						Results: &ast.FieldList{
							List: []*ast.Field{{
								Type: ast.NewIdent("decFunc"),
							}},
						},
					},
					Body: ah.BlockStmt(
						&ast.AssignStmt{
							Lhs: []ast.Expr{ast.NewIdent("data")},
							Tok: token.ASSIGN,
							Rhs: []ast.Expr{
								ah.CallExpr(ast.NewIdent("append"), ast.NewIdent("data"), operatorToReversedBinaryExpr(op, ast.NewIdent("x"), ast.NewIdent("seed"))),
							},
						},
						&ast.AssignStmt{
							Lhs: []ast.Expr{ast.NewIdent("seed")},
							Tok: token.ADD_ASSIGN,
							Rhs: []ast.Expr{ast.NewIdent("x")},
						},
						ah.ReturnStmt(ast.NewIdent("fnc")),
					),
				},
			},
		},
		ah.ExprStmt(callExpr),
	)
}
