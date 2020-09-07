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

	switch filename {
	case "error.go":
		for _, decl := range file.Decls {
			fun, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			// only used in panics
			switch fun.Name.Name {
			case "printany", "printanycustomtype":
				fun.Body.List = nil
			}
		}
	case "mprof.go":
		for _, decl := range file.Decls {
			fun, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			// remove all functions that print debug/tracing info
			// of the runtime
			switch {
			case strings.HasPrefix(fun.Name.Name, "trace"):
				fun.Body.List = nil
			}
		}
	case "print.go":
		for _, decl := range file.Decls {
			fun, ok := decl.(*ast.FuncDecl)
			if !ok {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.IMPORT {
					continue
				}

				for i, spec := range gen.Specs {
					imp := spec.(*ast.ImportSpec)
					if imp.Path.Value == `"runtime/internal/sys"` {
						// remove 'runtime/internal/sys' import, as it was used
						// in hexdumpWords
						gen.Specs = append(gen.Specs[:i], gen.Specs[i+1:]...)
						break
					}
				}
				continue
			}

			// only used in tracebacks
			if fun.Name.Name == "hexdumpWords" {
				fun.Body.List = nil
				break
			}
		}

		// add hidePrint declaration
		file.Decls = append(file.Decls, hidePrintDecl)
	case "runtime1.go":
		for _, decl := range file.Decls {
			fun, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			switch fun.Name.Name {
			case "parsedebugvars":
				// set defaults for GODEBUG cgocheck and
				// invalidptr, remove code that reads in
				// GODEBUG
				fun.Body = parsedebugvarsStmts
			case "setTraceback":
				// tracebacks are completely hidden, no
				// sense keeping this function
				fun.Body.List = nil
			}
		}
	default:
		// replace all 'print' and 'println' statements in
		// the runtime with an empty func, which will be
		// optimized out by the compiler
		ast.Inspect(file, stripPrints)
	}
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
