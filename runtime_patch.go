// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strconv"
	"strings"

	ah "mvdan.cc/garble/internal/asthelper"
)

// cloneIdent creates a copy of an ast.Ident and registers it in the type info
// so that the obfuscator can process it correctly.
func cloneIdent(node *ast.Ident, info *types.Info) *ast.Ident {
	clone := &ast.Ident{
		NamePos: node.NamePos,
		Name:    node.Name,
		Obj:     node.Obj,
	}
	// Register the clone in the type info so obfuscator sees it
	if info != nil {
		if obj := info.ObjectOf(node); obj != nil {
			info.Uses[clone] = obj
		}
	}
	return clone
}

// updateMagicValue updates hardcoded value of hdr.magic
// when verifying header in symtab.go
func updateMagicValue(file *ast.File, magicValue uint32, info *types.Info, fieldToStruct map[*types.Var]*types.Struct) {
	magicUpdated := false

	// Find `hdr.magic != 0xfffffff?` in symtab.go and update to random magicValue
	updateMagic := func(node ast.Node) bool {
		binExpr, ok := node.(*ast.BinaryExpr)
		if !ok || binExpr.Op != token.NEQ {
			return true
		}

		selectorExpr, ok := binExpr.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		if ident, ok := selectorExpr.X.(*ast.Ident); !ok || ident.Name != "hdr" {
			return true
		}
		if selectorExpr.Sel.Name != "magic" {
			return true
		}

		if _, ok := binExpr.Y.(*ast.BasicLit); !ok {
			return true
		}
		binExpr.Y = &ast.BasicLit{
			Kind:  token.INT,
			Value: strconv.FormatUint(uint64(magicValue), 10),
		}
		magicUpdated = true
		return false
	}

	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if ok && funcDecl.Name.Name == "moduledataverify1" {
			ast.Inspect(funcDecl, updateMagic)

			break
		}
	}

	if !magicUpdated {
		panic("magic value not updated")
	}
	if flagDebug {
		fmt.Fprintf(os.Stderr, "GARBLE_DEBUG: Set runtime magic check to %d (0x%x)\n", magicValue, magicValue)
	}
}

// injectDebugPrint was used for troubleshooting; keep as a no-op placeholder
// so we can re-enable without changing call sites if needed.
func injectDebugPrint(funcDecl *ast.FuncDecl, info *types.Info, fieldToStruct map[*types.Var]*types.Struct) {
}

// updateEntryOffset adds xor encryption for funcInfo.entryoff
// Encryption algorithm contains 1 xor and 1 multiply operations and is not cryptographically strong.
// Its goal, without slowing down program performance (reflection, stacktrace),
// is to make it difficult to determine relations between function metadata and function itself in a binary file.
// Difficulty of decryption is based on the difficulty of finding a small (probably inlined) entry() function without obvious patterns.
//
// The info parameter is used to register new AST nodes with type information,
// which is necessary for the obfuscator to process them correctly when runtime
// obfuscation is enabled. The fieldToStruct map is needed to register field objects
// for proper field name obfuscation.
func updateEntryOffset(file *ast.File, entryOffKey uint32, info *types.Info, fieldToStruct map[*types.Var]*types.Struct) {
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

		// Get the receiver identifier (e.g., "f" in "f.entryOff")
		receiverIdent, ok := selExpr.X.(*ast.Ident)
		if !ok {
			return true
		}

		// Clone the receiver identifier for the new selector expression.
		// This is crucial: we must NOT share the same ast.Ident node between
		// multiple parent nodes, as that confuses the obfuscator which walks
		// the AST and modifies node.Name in place.
		clonedReceiver := cloneIdent(receiverIdent, info)

		// Find the nameOff field in the struct to get its type object
		// so we can properly register the new selector in the type info.
		// We use LookupFieldOrMethod because nameOff might be in an embedded struct.
		var nameOffVar *types.Var
		var nameOffStruct *types.Struct
		if info != nil {
			if obj := info.ObjectOf(receiverIdent); obj != nil {
				// LookupFieldOrMethod handles embedded fields correctly.
				// For unexported fields like nameOff, we need to pass the package.
				pkg := obj.Pkg()
				fieldObj, _, _ := types.LookupFieldOrMethod(obj.Type(), true, pkg, nameOffField)
				if fv, ok := fieldObj.(*types.Var); ok {
					nameOffVar = fv
					// Find the struct that contains this field in fieldToStruct
					// by looking up the existing field entry
					if fieldToStruct != nil {
						nameOffStruct = fieldToStruct[fv]
					}
				}
			}
		}

		// Create the new nameOff selector: clonedReceiver.nameOff
		nameOffIdent := ast.NewIdent(nameOffField)
		if info != nil && nameOffVar != nil {
			info.Uses[nameOffIdent] = nameOffVar
			// Also ensure the field is in fieldToStruct for proper obfuscation
			if fieldToStruct != nil && nameOffStruct != nil {
				fieldToStruct[nameOffVar] = nameOffStruct
			}
		}

		newSelector := &ast.SelectorExpr{
			X:   clonedReceiver,
			Sel: nameOffIdent,
		}

		callExpr.Args[0] = &ast.BinaryExpr{
			X:  selExpr,
			Op: token.XOR,
			Y: &ast.ParenExpr{X: &ast.BinaryExpr{
				X:  ah.CallExpr(ast.NewIdent("uint32"), newSelector),
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

// stripRuntime removes unnecessary code from the runtime,
// such as panic and fatal error printing, and code that
// prints trace/debug info of the runtime.
func stripRuntime(basename string, file *ast.File) {
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

	if basename == "print.go" {
		file.Decls = append(file.Decls, hidePrintDecl)
		return
	}

	// replace all 'print' and 'println' statements in
	// the runtime with an empty func, which will be
	// optimized out by the compiler
	ast.Inspect(file, stripPrints)
}

var hidePrintDecl = &ast.FuncDecl{
	Doc: &ast.CommentGroup{
		List: []*ast.Comment{
			{Text: "//go:nowritebarrierrec"},
			{Text: "//go:nosplit"},
		},
	},
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
