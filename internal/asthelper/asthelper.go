// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package asthelper

import (
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
)

// StringLit returns an ast.BasicLit of kind STRING
func StringLit(value string) *ast.BasicLit {
	return &ast.BasicLit{
		Kind:  token.STRING,
		Value: fmt.Sprintf("%q", value),
	}
}

// IntLit returns an ast.BasicLit of kind INT
func IntLit(value int) *ast.BasicLit {
	return &ast.BasicLit{
		Kind:  token.INT,
		Value: strconv.Itoa(value),
	}
}

// IndexExpr "name[index]"
func IndexExpr(name string, index ast.Expr) *ast.IndexExpr {
	return &ast.IndexExpr{
		X:     ast.NewIdent(name),
		Index: index,
	}
}

// CallExpr "fun(arg)"
func CallExpr(fun ast.Expr, args ...ast.Expr) *ast.CallExpr {
	return &ast.CallExpr{
		Fun:  fun,
		Args: args,
	}
}

// LambdaCall "func() resultType {block}()"
func LambdaCall(resultType ast.Expr, block *ast.BlockStmt) *ast.CallExpr {
	funcLit := &ast.FuncLit{
		Type: &ast.FuncType{
			Params: &ast.FieldList{},
			Results: &ast.FieldList{
				List: []*ast.Field{
					{Type: resultType},
				},
			},
		},
		Body: block,
	}
	return CallExpr(funcLit)
}

// ReturnStmt "return result"
func ReturnStmt(results ...ast.Expr) *ast.ReturnStmt {
	return &ast.ReturnStmt{
		Results: results,
	}
}

// BlockStmt a block of multiple statments e.g. a function body
func BlockStmt(stmts ...ast.Stmt) *ast.BlockStmt {
	return &ast.BlockStmt{List: stmts}
}

// ExprStmt convert an ast.Expr to an ast.Stmt
func ExprStmt(expr ast.Expr) *ast.ExprStmt {
	return &ast.ExprStmt{X: expr}
}

// DataToByteSlice turns a byte slice like []byte{1, 2, 3} into an AST
// expression
func DataToByteSlice(data []byte) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.ArrayType{
			Elt: &ast.Ident{Name: "byte"},
		},
		Args: []ast.Expr{StringLit(string(data))},
	}
}
