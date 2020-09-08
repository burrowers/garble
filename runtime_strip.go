// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/token"
	"strings"

	ah "mvdan.cc/garble/internal/asthelper"
)

// stripRuntime removes unnecessary code from the runtime,
// such as panic and fatal error printing, and code that
// prints trace/debug info of the runtime.
func stripRuntime(filename string, file *ast.File) {
	stripPrints := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}

		switch id.Name {
		case "print", "println":
			id.Name = "hidePrint"
			return false
		default:
			return true
		}
	}

	for _, decl := range file.Decls {
		switch x := decl.(type) {
		case *ast.FuncDecl:
			switch filename {
			case "error.go":
				// only used in panics
				switch x.Name.Name {
				case "printany", "printanycustomtype":
					x.Body.List = nil
				}
			case "mprof.go":
				// remove all functions that print debug/tracing info
				// of the runtime
				if strings.HasPrefix(x.Name.Name, "trace") {
					x.Body.List = nil
				}
			case "print.go":
				// only used in tracebacks
				if x.Name.Name == "hexdumpWords" {
					x.Body.List = nil
					break
				}
			case "runtime1.go":
				switch x.Name.Name {
				case "parsedebugvars":
					// set defaults for GODEBUG cgocheck and
					// invalidptr, remove code that reads in
					// GODEBUG
					x.Body = parsedebugvarsStmts
				case "setTraceback":
					// tracebacks are completely hidden, no
					// sense keeping this function
					x.Body.List = nil
				}
			case "traceback.go":
				switch x.Name.Name {
				case "tracebackdefers", "goroutineheader", "tracebackHexdump":
					x.Body.List = nil
				case "printOneCgoTraceback":
					x.Body = ah.BlockStmt(ah.ReturnStmt(ah.IntLit(0)))
				default:
					if strings.HasPrefix(x.Name.Name, "print") {
						x.Body.List = nil
					}
				}
			default:
				break
			}
		case *ast.GenDecl:
			if filename != "print.go" || x.Tok != token.IMPORT {
				continue
			}

			for i, spec := range x.Specs {
				imp := spec.(*ast.ImportSpec)
				if imp.Path.Value == `"runtime/internal/sys"` {
					// remove 'runtime/internal/sys' import, as it was used
					// in hexdumpWords
					x.Specs = append(x.Specs[:i], x.Specs[i+1:]...)
					break
				}
			}
		}
	}

	if filename == "print.go" {
		file.Decls = append(file.Decls, hidePrintDecl)
	}

	// replace all 'print' and 'println' statements in
	// the runtime with an empty func, which will be
	// optimized out by the compiler
	ast.Inspect(file, stripPrints)
}

var hidePrintDecl = &ast.FuncDecl{
	Name: ah.Ident("hidePrint"),
	Type: &ast.FuncType{Params: &ast.FieldList{
		List: []*ast.Field{{
			Names: []*ast.Ident{{Name: "args"}},
			Type: &ast.Ellipsis{Elt: &ast.InterfaceType{
				Methods: &ast.FieldList{},
			}},
		}},
	}},
	Body: &ast.BlockStmt{},
}

var parsedebugvarsStmts = ah.BlockStmt(
	&ast.AssignStmt{
		Lhs: []ast.Expr{&ast.SelectorExpr{
			X:   ah.Ident("debug"),
			Sel: ah.Ident("cgocheck"),
		}},
		Tok: token.ASSIGN,
		Rhs: []ast.Expr{ah.IntLit(1)},
	},
	&ast.AssignStmt{
		Lhs: []ast.Expr{&ast.SelectorExpr{
			X:   ah.Ident("debug"),
			Sel: ah.Ident("invalidptr"),
		}},
		Tok: token.ASSIGN,
		Rhs: []ast.Expr{ah.IntLit(1)},
	},
)
