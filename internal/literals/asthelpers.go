package literals

import (
	"go/ast"
	"go/token"
	"strconv"
)

func ident(name string) *ast.Ident {
	return &ast.Ident{Name: name}
}
func intLiteral(value string) *ast.BasicLit {
	return &ast.BasicLit{
		Kind:  token.INT,
		Value: value,
	}
}

// name[index]
func indexExpr(name string, index ast.Expr) *ast.IndexExpr {
	return &ast.IndexExpr{
		X:     ident(name),
		Index: index,
	}
}

// fun(arg)
func callExpr(fun ast.Expr, arg ast.Expr) *ast.CallExpr {
	var args []ast.Expr
	if arg != nil {
		args = []ast.Expr{arg}
	}

	return &ast.CallExpr{
		Fun:  fun,
		Args: args,
	}
}

// func() resultType {block}()
func lambdaCall(resultType ast.Expr, block *ast.BlockStmt) *ast.CallExpr {
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
	return callExpr(funcLit, nil)
}

// return result
func returnStmt(results ...ast.Expr) *ast.ReturnStmt {
	return &ast.ReturnStmt{
		Results: results,
	}
}

// _ = data[pos]
func boundsCheckData(pos int) *ast.AssignStmt {
	posStr := strconv.Itoa(pos)
	return &ast.AssignStmt{
		Lhs: []ast.Expr{ident("_")},
		Tok: token.ASSIGN,
		Rhs: []ast.Expr{indexExpr("data", intLiteral(posStr))},
	}
}
