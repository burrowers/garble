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
		// Add HidePanics function and internal hidePanics variable
		file.Decls = append(file.Decls, hidePanicsDecls...)
	case "error.go":
		// Add a function panicprint, that does nothing if panics are
		// hidden, otherwise forwards arguments to printany to print them
		// as normal.
		//
		// We will also add two statements to printany:
		// 1. An if statement that returns early if panics are hidden, as
		//    printany is called directly when printing panics
		// 2. An additional case statement that handles printing runtime.hex
		//    values. Without this case statement, the default case will print
		//    the runtime.hex values in a way not consistent with normal panic
		//    outputs
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if ok && funcDecl.Name.Name == "printany" {
				for _, stmt := range funcDecl.Body.List {
					switchStmt, ok := stmt.(*ast.TypeSwitchStmt)
					if !ok {
						continue
					}

					switchStmt.Body.List = append(switchStmt.Body.List, printanyHexCase)
					break
				}

				funcDecl.Body.List = append([]ast.Stmt{hidePanicsCheckStmt}, funcDecl.Body.List...)
				break
			}
		}

		file.Decls = append(file.Decls, panicprintDecl)
	case "panic.go", "traceback.go":
		// Change all calls to print, which we don't control, to
		// panicprint, which we do control and does the same thing.
		ast.Inspect(file, switchPanicPrints)
	}
}

var hidePanicsCheckStmt = &ast.IfStmt{
	Cond: &ast.Ident{
		Name: "hidePanics",
	},
	Body: &ast.BlockStmt{
		List: []ast.Stmt{
			&ast.ReturnStmt{},
		},
	},
}

var hidePanicsDecls = []ast.Decl{
	&ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{
				Names: []*ast.Ident{
					&ast.Ident{
						Name: "hidePanics",
					},
				},
				Type: &ast.Ident{
					Name: "bool",
				},
			},
		},
	},
	&ast.FuncDecl{
		Name: &ast.Ident{
			Name: "HidePanics",
		},
		Type: &ast.FuncType{
			Params: &ast.FieldList{
				List: []*ast.Field{
					&ast.Field{
						Names: []*ast.Ident{
							&ast.Ident{
								Name: "hide",
							},
						},
						Type: &ast.Ident{
							Name: "bool",
						},
					},
				},
			},
		},
		Body: &ast.BlockStmt{
			List: []ast.Stmt{
				&ast.AssignStmt{
					Lhs: []ast.Expr{
						&ast.Ident{
							Name: "hidePanics",
						},
					},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{
						&ast.Ident{
							Name: "hide",
						},
					},
				},
			},
		},
	},
}

var printanyHexCase = &ast.CaseClause{
	List: []ast.Expr{
		&ast.Ident{
			Name: "hex",
		},
	},
	Body: []ast.Stmt{
		&ast.ExprStmt{
			X: &ast.CallExpr{
				Fun: &ast.Ident{
					Name: "print",
				},
				Args: []ast.Expr{
					&ast.Ident{
						Name: "v",
					},
				},
			},
		},
	},
}

var panicprintDecl = &ast.FuncDecl{
	Name: &ast.Ident{
		Name: "panicprint",
	},
	Type: &ast.FuncType{
		Params: &ast.FieldList{
			List: []*ast.Field{
				&ast.Field{
					Names: []*ast.Ident{
						&ast.Ident{
							Name: "args",
						},
					},
					Type: &ast.Ellipsis{
						Elt: &ast.InterfaceType{
							Methods: &ast.FieldList{},
						},
					},
				},
			},
		},
	},
	Body: &ast.BlockStmt{
		List: []ast.Stmt{
			&ast.IfStmt{
				Cond: &ast.Ident{
					Name: "hidePanics",
				},
				Body: &ast.BlockStmt{
					List: []ast.Stmt{
						&ast.ReturnStmt{},
					},
				},
			},
			&ast.RangeStmt{
				Key: &ast.Ident{
					Name: "_",
				},
				Value: &ast.Ident{
					Name: "arg",
				},
				Tok: token.DEFINE,
				X: &ast.Ident{
					Name: "args",
				},
				Body: &ast.BlockStmt{
					List: []ast.Stmt{
						&ast.ExprStmt{
							X: &ast.CallExpr{
								Fun: &ast.Ident{
									Name: "printany",
								},
								Args: []ast.Expr{
									&ast.Ident{
										Name: "arg",
									},
								},
							},
						},
					},
				},
			},
		},
	},
}
