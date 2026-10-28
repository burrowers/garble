package ssa2ast

import (
	"go/ast"
	"go/token"
	"go/types"

	ah "mvdan.cc/garble/internal/asthelper"
)

func makeMapIteratorPolyfill(tc *TypeConverter, mapType *types.Map) (ast.Expr, types.Type, error) {
	keyTypeExpr, err := tc.Convert(mapType.Key())
	if err != nil {
		return nil, nil, err
	}
	valueTypeExpr, err := tc.Convert(mapType.Elem())
	if err != nil {
		return nil, nil, err
	}

	nextType := types.NewSignatureType(nil, nil, nil, nil, types.NewTuple(
		types.NewVar(token.NoPos, nil, "", types.Typ[types.Bool]),
		types.NewVar(token.NoPos, nil, "", mapType.Key()),
		types.NewVar(token.NoPos, nil, "", mapType.Elem()),
	), false)

	// Generated using https://github.com/lu4p/astextract from snippet:
	/*
		func(m map[<key type>]<value type>) func() (bool, <key type>, <value type>) {
			keys := make([]<key type>, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			i := 0
			return func() (ok bool, k <key type>, r <value type>) {
				if i < len(keys) {
					k = keys[i]
					ok, r = true, m[k]
					i++
				}
				return
			}
		}
	*/
	return &ast.FuncLit{
		Type: &ast.FuncType{
			Params: &ast.FieldList{List: []*ast.Field{
				ah.Field(&ast.MapType{Key: keyTypeExpr, Value: valueTypeExpr}, ast.NewIdent("m")),
			}},
			Results: &ast.FieldList{List: []*ast.Field{{
				Type: &ast.FuncType{
					Params: &ast.FieldList{},
					Results: &ast.FieldList{List: []*ast.Field{
						{Type: ast.NewIdent("bool")},
						{Type: keyTypeExpr},
						{Type: valueTypeExpr},
					}},
				},
			}}},
		},
		Body: ah.BlockStmt(
			ah.AssignDefineStmt(ast.NewIdent("keys"), ah.CallExprByName("make",
				&ast.ArrayType{Elt: keyTypeExpr}, ah.IntLit(0), ah.CallExprByName("len", ast.NewIdent("m")))),
			&ast.RangeStmt{
				Key: ast.NewIdent("k"),
				Tok: token.DEFINE,
				X:   ast.NewIdent("m"),
				Body: ah.BlockStmt(ah.AssignStmt(ast.NewIdent("keys"),
					ah.CallExprByName("append", ast.NewIdent("keys"), ast.NewIdent("k")))),
			},
			ah.AssignDefineStmt(ast.NewIdent("i"), ah.IntLit(0)),
			ah.ReturnStmt(&ast.FuncLit{
				Type: &ast.FuncType{
					Params: &ast.FieldList{},
					Results: &ast.FieldList{List: []*ast.Field{
						ah.Field(ast.NewIdent("bool"), ast.NewIdent("ok")),
						ah.Field(keyTypeExpr, ast.NewIdent("k")),
						ah.Field(valueTypeExpr, ast.NewIdent("r")),
					}},
				},
				Body: ah.BlockStmt(
					&ast.IfStmt{
						Cond: ah.BinaryExpr(ast.NewIdent("i"), token.LSS, ah.CallExprByName("len", ast.NewIdent("keys"))),
						Body: ah.BlockStmt(
							ah.AssignStmt(ast.NewIdent("k"), ah.IndexExpr("keys", ast.NewIdent("i"))),
							&ast.AssignStmt{
								Lhs: []ast.Expr{ast.NewIdent("ok"), ast.NewIdent("r")},
								Tok: token.ASSIGN,
								Rhs: []ast.Expr{ast.NewIdent("true"), ah.IndexExpr("m", ast.NewIdent("k"))},
							},
							&ast.IncDecStmt{X: ast.NewIdent("i"), Tok: token.INC},
						),
					},
					ah.ReturnStmt(),
				),
			}),
		),
	}, nextType, nil
}
