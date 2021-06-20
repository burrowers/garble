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
			case "mgcscavenge.go":
				// used in tracing the scavenger
				if x.Name.Name == "printScavTrace" {
					x.Body.List = nil
					break
				}
			case "mprof.go":
				// remove all functions that print debug/tracing info
				// of the runtime
				if strings.HasPrefix(x.Name.Name, "trace") {
					x.Body.List = nil
				}
			case "panic.go":
				// used for printing panics
				switch x.Name.Name {
				case "preprintpanics", "printpanics":
					x.Body.List = nil
				}
			case "print.go":
				// only used in tracebacks
				if x.Name.Name == "hexdumpWords" {
					x.Body.List = nil
					break
				}
			case "proc.go":
				// used in tracing the scheduler
				if x.Name.Name == "schedtrace" {
					x.Body.List = nil
					break
				}
			case "runtime1.go":
				usesEnv := func(node ast.Node) bool {
					seen := false
					ast.Inspect(node, func(node ast.Node) bool {
						ident, ok := node.(*ast.Ident)
						if ok && ident.Name == "gogetenv" {
							seen = true
							return false
						}
						return true
					})
					return seen
				}
			filenames:
				switch x.Name.Name {
				case "parsedebugvars":
					// keep defaults for GODEBUG cgocheck and invalidptr,
					// remove code that reads GODEBUG via gogetenv
					for i, stmt := range x.Body.List {
						if usesEnv(stmt) {
							x.Body.List = x.Body.List[:i]
							break filenames
						}
					}
					panic("did not see any gogetenv call in parsedebugvars")
				case "setTraceback":
					// tracebacks are completely hidden, no
					// sense keeping this function
					x.Body.List = nil
				}
			case "traceback.go":
				// only used for printing tracebacks
				switch x.Name.Name {
				case "tracebackdefers", "printcreatedby", "printcreatedby1", "traceback", "tracebacktrap", "traceback1", "printAncestorTraceback",
					"printAncestorTracebackFuncInfo", "goroutineheader", "tracebackothers", "tracebackHexdump", "printCgoTraceback":
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
			if x.Tok != token.IMPORT {
				continue
			}

			switch filename {
			case "print.go":
				// was used in hexdumpWords
				x.Specs = removeImport(`"runtime/internal/sys"`, x.Specs)
			case "traceback.go":
				// was used in traceback1
				x.Specs = removeImport(`"runtime/internal/atomic"`, x.Specs)
			}

		}
	}

	switch filename {
	case "runtime1.go":
		// On Go 1.16.x, the code above results in runtime1.go having an
		// unused import. Mark it as used via "var _ = pkg.Func".
		// If this is a recurring problem, we could go for a more
		// generic solution like x/tools/imports.
		for _, imp := range file.Imports {
			if imp.Path.Value == `"internal/bytealg"` {
				imp.Name = &ast.Ident{Name: "_"}
				break
			}
		}
	case "print.go":
		file.Decls = append(file.Decls, hidePrintDecl)
		return
	}

	// replace all 'print' and 'println' statements in
	// the runtime with an empty func, which will be
	// optimized out by the compiler
	ast.Inspect(file, stripPrints)
}

func removeImport(importPath string, specs []ast.Spec) []ast.Spec {
	for i, spec := range specs {
		imp := spec.(*ast.ImportSpec)
		if imp.Path.Value == importPath {
			specs = append(specs[:i], specs[i+1:]...)
			break
		}
	}

	return specs
}

var hidePrintDecl = &ast.FuncDecl{
	Name: ast.NewIdent("hidePrint"),
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
