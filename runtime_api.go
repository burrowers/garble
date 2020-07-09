package main

import (
	"go/ast"
	"go/token"
)

// addRuntimeAPI exposes additional functions in the runtime
// package that may be helpful when hiding information
// during execution is required.
func addRuntimeAPI(filename string, file *ast.File) {
	switchPanicPrints := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}

		if id.Name == "print" {
			id.Name = "panicprint"
			return false
		}

		return true
	}

	switch filename {
	case "debug.go":
		// Add hideFatalErrors function and internal fatalErrorsHidden variable
		file.Decls = append(file.Decls, hideFatalErrorsDecls...)
	case "error.go":
		// Add a function panicprint, that does nothing if panics are
		// hidden, otherwise forwards arguments to printany to print them
		// as normal. Although we add an if statement to printany to do
		// nothing if panics are hidden, printany only takes one argument,
		// and both print and printany are used to print panic messages.
		// panicprint's entire purpose is to act as a replacement to print
		// that respects hideFatalErrors, and print is variadic, so print
		// must be replaced by a variadic function, hence panicprint.
		//
		// We will also add two statements to printany:
		// 1. An if statement that returns early if panics are hidden
		// 2. An additional case statement that handles printing runtime.hex
		//    values. Without this case statement, the default case will print
		//    the runtime.hex values in a way not consistent with normal panic
		//    outputs
		for _, decl := range file.Decls {
			decl, ok := decl.(*ast.FuncDecl)
			if !ok || decl.Name.Name != "printany" {
				continue
			}
			for _, stmt := range decl.Body.List {
				if stmt, ok := stmt.(*ast.TypeSwitchStmt); ok {
					stmt.Body.List = append(stmt.Body.List, printanyHexCase)
					break
				}
			}
			decl.Body.List = append([]ast.Stmt{fatalErrorsHiddenCheckStmt}, decl.Body.List...)
			break
		}

		file.Decls = append(file.Decls, panicprintDecl)
	default:
		// Change all calls to print, which we don't control, to
		// panicprint, which we do control and does the same thing.
		ast.Inspect(file, switchPanicPrints)
	}
}

var fatalErrorsHiddenCheckStmt = &ast.IfStmt{
	Cond: &ast.Ident{Name: "fatalErrorsHidden"},
	Body: &ast.BlockStmt{List: []ast.Stmt{
		&ast.ReturnStmt{},
	}},
}

var hideFatalErrorsDecls = []ast.Decl{
	&ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{
			Names: []*ast.Ident{{Name: "fatalErrorsHidden"}},
			Type:  &ast.Ident{Name: "bool"},
		}},
	},
	&ast.FuncDecl{
		Name: &ast.Ident{Name: "hideFatalErrors"},
		Type: &ast.FuncType{Params: &ast.FieldList{
			List: []*ast.Field{{
				Names: []*ast.Ident{{Name: "hide"}},
				Type:  &ast.Ident{Name: "bool"},
			}},
		}},
		Body: &ast.BlockStmt{List: []ast.Stmt{
			&ast.AssignStmt{
				Lhs: []ast.Expr{&ast.Ident{Name: "fatalErrorsHidden"}},
				Tok: token.ASSIGN,
				Rhs: []ast.Expr{&ast.Ident{Name: "hide"}},
			},
		}},
	},
}

var printanyHexCase = &ast.CaseClause{
	List: []ast.Expr{&ast.Ident{Name: "hex"}},
	Body: []ast.Stmt{
		&ast.ExprStmt{X: &ast.CallExpr{
			Fun:  &ast.Ident{Name: "print"},
			Args: []ast.Expr{&ast.Ident{Name: "v"}},
		}},
	},
}

var panicprintDecl = &ast.FuncDecl{
	Name: &ast.Ident{Name: "panicprint"},
	Type: &ast.FuncType{Params: &ast.FieldList{
		List: []*ast.Field{{
			Names: []*ast.Ident{{Name: "args"}},
			Type: &ast.Ellipsis{Elt: &ast.InterfaceType{
				Methods: &ast.FieldList{},
			}},
		}},
	}},
	Body: &ast.BlockStmt{List: []ast.Stmt{
		&ast.IfStmt{
			Cond: &ast.Ident{Name: "fatalErrorsHidden"},
			Body: &ast.BlockStmt{List: []ast.Stmt{
				&ast.ReturnStmt{},
			}},
		},
		&ast.RangeStmt{
			Key:   &ast.Ident{Name: "_"},
			Value: &ast.Ident{Name: "arg"},
			Tok:   token.DEFINE,
			X:     &ast.Ident{Name: "args"},
			Body: &ast.BlockStmt{List: []ast.Stmt{
				&ast.ExprStmt{X: &ast.CallExpr{
					Fun:  &ast.Ident{Name: "printany"},
					Args: []ast.Expr{&ast.Ident{Name: "arg"}},
				}},
			}},
		},
	}},
}
