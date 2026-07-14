// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	ah "mvdan.cc/garble/internal/asthelper"
)

// updateMagicValue updates the global constant
// `Go120PCLnTabMagic PCLnTabMagic = 0xfffffff1`
// to use the provided magic value integer.
// This is the latest magic value in use as of Go 1.26.
func updateMagicValue(file *ast.File, magicValue uint32) {
	magicUpdated := false

	for _, decl := range file.Decls {
		decl, ok := decl.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			continue
		}
		for _, spec := range decl.Specs {
			spec, ok := spec.(*ast.ValueSpec)
			if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
				continue
			}
			if spec.Names[0].Name == "Go120PCLnTabMagic" {
				spec.Values[0] = &ast.BasicLit{
					Kind:  token.INT,
					Value: strconv.FormatUint(uint64(magicValue), 10),
				}
				magicUpdated = true
			}
		}
	}

	if !magicUpdated {
		panic("magic value not updated")
	}
}

// updateEntryOffset adds xor encryption for funcInfo.entryoff
// Encryption algorithm contains 1 xor and 1 multiply operations and is not cryptographically strong.
// Its goal, without slowing down program performance (reflection, stacktrace),
// is to make it difficult to determine relations between function metadata and function itself in a binary file.
// Difficulty of decryption is based on the difficulty of finding a small (probably inlined) entry() function without obvious patterns.
func updateEntryOffset(file *ast.File, entryOffKey uint32) {
	// Note that this field could be renamed in future Go versions.
	const nameOffField = "nameOff"
	entryOffUpdated := false

	// During linker stage we encrypt funcInfo.entryoff using a random number and funcInfo.nameOff,
	// for correct program functioning we must decrypt funcInfo.entryoff at any access to it.
	// In runtime package all references to funcInfo.entryOff are made through one method entry():
	// func (f funcInfo) entry() uintptr {
	//	return f.datap.textAddr(f.entryoff)
	// }
	// It is enough to inject decryption into entry() method for program to start working transparently with encrypted value of funcInfo.entryOff:
	// func (f funcInfo) entry() uintptr {
	//	return f.datap.textAddr(f.entryoff ^ (uint32(f.nameOff) * <random int>))
	// }
	updateEntryOff := func(node ast.Node) bool {
		callExpr, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		textSelExpr, ok := callExpr.Fun.(*ast.SelectorExpr)
		if !ok || textSelExpr.Sel.Name != "textAddr" {
			return true
		}

		selExpr, ok := callExpr.Args[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}

		callExpr.Args[0] = &ast.BinaryExpr{
			X:  selExpr,
			Op: token.XOR,
			Y: &ast.ParenExpr{X: &ast.BinaryExpr{
				X: ah.CallExpr(ast.NewIdent("uint32"), &ast.SelectorExpr{
					X:   selExpr.X,
					Sel: ast.NewIdent(nameOffField),
				}),
				Op: token.MUL,
				Y: &ast.BasicLit{
					Kind:  token.INT,
					Value: strconv.FormatUint(uint64(entryOffKey), 10),
				},
			}},
		}
		entryOffUpdated = true
		return false
	}

	var entryFunc *ast.FuncDecl
	for _, decl := range file.Decls {
		decl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if decl.Name.Name == "entry" {
			entryFunc = decl
			break
		}
	}
	if entryFunc == nil {
		panic("entry function not found")
	}

	ast.Inspect(entryFunc, updateEntryOff)
	if !entryOffUpdated {
		panic("entryOff not found")
	}
}

func stripFatalStringFragments(expr ast.Expr) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind == token.STRING {
			expr.Value = `""`
		}
	case *ast.ParenExpr:
		stripFatalStringFragments(expr.X)
	case *ast.BinaryExpr:
		if expr.Op == token.ADD {
			stripFatalStringFragments(expr.X)
			stripFatalStringFragments(expr.Y)
		}
	}
}

// stripFatalMessageText blanks messages passed to the named package-level
// functions. Go 1.26 routes a number of standard-library fatal paths into
// runtime.throw/runtime.fatal via linkname, so limiting this operation to the
// runtime package leaves those diagnostics in the final binary.
//
// Computed expressions are still evaluated to preserve local-variable uses and
// side effects. Only a zero-length view is passed to the fatal function.
func stripFatalMessageText(basename string, file *ast.File, info *types.Info, callNames ...string) int {
	// Package tests can declare local helpers with the same short names. Tiny
	// mode should only rewrite calls in production source files.
	if strings.HasSuffix(basename, "_test.go") {
		return 0
	}

	matchesName := func(name string) bool {
		for _, callName := range callNames {
			if name == callName {
				return true
			}
		}
		return false
	}

	stripped := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || !matchesName(id.Name) {
			return true
		}
		if info != nil {
			obj := info.Uses[id]
			if obj == nil || obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope() {
				// A local shadow with the same short name is unrelated to the
				// package-level function or variable wired into the runtime.
				return true
			}
		}
		message := call.Args[0]
		stripFatalStringFragments(message)
		if literal, ok := message.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			literal.Value = `""`
		} else {
			call.Args[0] = &ast.SliceExpr{
				X:    &ast.ParenExpr{X: message},
				High: &ast.BasicLit{Kind: token.INT, Value: "0"},
			}
		}
		stripped++
		return false
	})
	return stripped
}

type runtimeDependencyStrips struct {
	fatalMessages int
	cgroupPrints  int
}

// These functions are either linknamed to runtime.throw/runtime.fatal or, in
// exithook's case, populated with runtime.throw during runtime initialization.
// Keep the allowlist exact: rewriting an arbitrary function named fatal in a
// normal package would be an unacceptable semantic change.
var runtimeFatalCallNames = map[string][]string{
	"crypto/internal/fips140":   {"fatal"},
	"crypto/internal/sysrand":   {"fatal"},
	"crypto/rand":               {"fatal"},
	"internal/runtime/cgroup":   {"throw"},
	"internal/runtime/exithook": {"Throw"},
	"internal/runtime/maps":     {"fatal"},
	"internal/sync":             {"throw", "fatal"},
	"sync":                      {"throw", "fatal"},
}

var requiredRuntimeFatalMessageCounts = map[string]int{
	"crypto/internal/fips140":   2,
	"crypto/internal/sysrand":   1,
	"crypto/rand":               1,
	"internal/runtime/cgroup":   6,
	"internal/runtime/exithook": 2,
	"internal/runtime/maps":     42,
	"internal/sync":             3,
	"sync":                      5,
}

// stripCgroupFatalPrint removes the one Linux cgroup diagnostic which writes
// directly through the print builtin immediately before calling runtime.throw.
// Its replacement has the same concrete parameter types, so all arguments are
// still evaluated without adding interface conversions or a package-level
// declaration. The exact shape deliberately fails closed if Go changes it.
func stripCgroupFatalPrint(basename string, file *ast.File) int {
	if basename != "cgroup_linux.go" {
		return 0
	}
	stripped := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 4 {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "println" {
			return true
		}
		first, ok := call.Args[0].(*ast.BasicLit)
		if !ok || first.Kind != token.STRING || first.Value != `"runtime: cgroup buffer length"` {
			return true
		}
		third, ok := call.Args[2].(*ast.BasicLit)
		if !ok || third.Kind != token.STRING || third.Value != `"want"` {
			return true
		}
		first.Value = `""`
		third.Value = `""`
		call.Fun = &ast.FuncLit{
			Type: &ast.FuncType{Params: &ast.FieldList{List: []*ast.Field{
				{Type: ast.NewIdent("string")},
				{Type: ast.NewIdent("int")},
				{Type: ast.NewIdent("string")},
				{Type: ast.NewIdent("int")},
			}}},
			Body: &ast.BlockStmt{},
		}
		stripped++
		return false
	})
	return stripped
}

func stripRuntimeDependency(importPath, basename string, file *ast.File, info *types.Info) runtimeDependencyStrips {
	var result runtimeDependencyStrips
	result.fatalMessages = stripFatalMessageText(basename, file, info, runtimeFatalCallNames[importPath]...)
	if importPath == "internal/runtime/cgroup" {
		result.cgroupPrints = stripCgroupFatalPrint(basename, file)
	}
	return result
}

func validateRuntimeDependencyStripping(importPath string, strippedByFile map[string]runtimeDependencyStrips) {
	var totals runtimeDependencyStrips
	for _, result := range strippedByFile {
		totals.fatalMessages += result.fatalMessages
		totals.cgroupPrints += result.cgroupPrints
	}
	want := requiredRuntimeFatalMessageCounts[importPath]
	if totals.fatalMessages != want {
		panic(fmt.Sprintf("runtime-linked fatal stripping matched %d calls in %s, want %d", totals.fatalMessages, importPath, want))
	}
	if importPath == "internal/runtime/cgroup" && totals.cgroupPrints != 1 {
		panic(fmt.Sprintf("runtime cgroup print stripping matched %d calls, want 1", totals.cgroupPrints))
	}
}

// stripRuntime removes unnecessary code from the runtime,
// such as panic and fatal error printing, and code that
// prints trace/debug info of the runtime.
func stripRuntime(basename string, file *ast.File, info *types.Info) {
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
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		switch basename {
		case "error.go":
			// only used in panics
			switch funcDecl.Name.Name {
			case "printany", "printanycustomtype":
				funcDecl.Body.List = nil
			}
		case "mgcscavenge.go":
			// used in tracing the scavenger
			if funcDecl.Name.Name == "printScavTrace" {
				funcDecl.Body.List = nil
			}
		case "mprof.go":
			// remove all functions that print debug/tracing info
			// of the runtime
			if strings.HasPrefix(funcDecl.Name.Name, "trace") {
				funcDecl.Body.List = nil
			}
		case "panic.go":
			// used for printing panics
			switch funcDecl.Name.Name {
			case "preprintpanics", "printpanics":
				funcDecl.Body.List = nil
			}
		case "print.go":
			// only used in tracebacks
			if funcDecl.Name.Name == "hexdumpWords" {
				funcDecl.Body.List = nil
			}
		case "proc.go":
			// used in tracing the scheduler
			if funcDecl.Name.Name == "schedtrace" {
				funcDecl.Body.List = nil
			}
		case "runtime1.go":
			switch funcDecl.Name.Name {
			case "setTraceback":
				// tracebacks are completely hidden, no
				// sense keeping this function
				funcDecl.Body.List = nil
			}
		case "traceback.go":
			// only used for printing tracebacks
			switch funcDecl.Name.Name {
			case "tracebackdefers", "printcreatedby", "printcreatedby1", "traceback", "tracebacktrap", "traceback1", "printAncestorTraceback",
				"printAncestorTracebackFuncInfo", "goroutineheader", "tracebackothers", "tracebackHexdump", "printCgoTraceback":
				funcDecl.Body.List = nil
			case "printOneCgoTraceback":
				funcDecl.Body = ah.BlockStmt(ah.ReturnStmt(ast.NewIdent("false")))
			default:
				if strings.HasPrefix(funcDecl.Name.Name, "print") {
					funcDecl.Body.List = nil
				}
			}
		}

	}

	stripFatalMessageText(basename, file, info, "throw", "fatal")
	if basename == "print.go" {
		// print.go implements application-facing print/println support; rewriting
		// its internal calls breaks interface and pointer formatting. The helper
		// declaration is needed by calls rewritten in the other runtime files.
		file.Decls = append(file.Decls, hidePrintDecl)
		return
	}

	// replace all 'print' and 'println' statements in
	// the runtime with an empty func, which will be
	// optimized out by the compiler
	ast.Inspect(file, stripPrints)
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
