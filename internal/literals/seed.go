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

func (seed) obfuscate(obfRand *mathrand.Rand, names *generatedNames, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	seed := byte(obfRand.Uint32())
	originalSeed := seed

	op := randOperator(obfRand)
	var callExpr *ast.CallExpr
	for i, b := range data {
		encB := evalOperator(op, b, seed)
		seed += encB

		if i == 0 {
			callExpr = ah.CallExpr(names.ident("fnc"), byteLitWithExtKey(obfRand, encB, extKeys, highProb))
			continue
		}

		callExpr = ah.CallExpr(callExpr, byteLitWithExtKey(obfRand, encB, extKeys, lowProb))
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{names.ident("seed")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.CallExprByName("byte", byteLitWithExtKey(obfRand, originalSeed, extKeys, highProb))},
		},
		makeDataStmt(names, len(data)),
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.TYPE,
				Specs: []ast.Spec{&ast.TypeSpec{
					Name: names.ident("decFunc"),
					Type: &ast.FuncType{
						Params: &ast.FieldList{List: []*ast.Field{
							{Type: ast.NewIdent("byte")},
						}},
						Results: &ast.FieldList{List: []*ast.Field{
							{Type: names.ident("decFunc")},
						}},
					},
				}},
			},
		},
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{names.ident("fnc")},
					Type:  names.ident("decFunc"),
				}},
			},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{names.ident("fnc")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{
				&ast.FuncLit{
					Type: &ast.FuncType{
						Params: &ast.FieldList{
							List: []*ast.Field{{
								Names: []*ast.Ident{names.ident("x")},
								Type:  ast.NewIdent("byte"),
							}},
						},
						Results: &ast.FieldList{
							List: []*ast.Field{{
								Type: names.ident("decFunc"),
							}},
						},
					},
					Body: ah.BlockStmt(
						&ast.AssignStmt{
							Lhs: []ast.Expr{names.ident("data")},
							Tok: token.ASSIGN,
							Rhs: []ast.Expr{
								ah.CallExpr(ast.NewIdent("append"), names.ident("data"), operatorToReversedBinaryExpr(op, names.ident("x"), names.ident("seed"))),
							},
						},
						&ast.AssignStmt{
							Lhs: []ast.Expr{names.ident("seed")},
							Tok: token.ADD_ASSIGN,
							Rhs: []ast.Expr{names.ident("x")},
						},
						ah.ReturnStmt(names.ident("fnc")),
					),
				},
			},
		},
		ah.ExprStmt(callExpr),
	)
}
