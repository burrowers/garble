// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/token"
	ah "mvdan.cc/garble/internal/asthelper"
)

// updateMagicValue updates hardcoded value of hdr.magic
// when verifying header in symtab.go
func updateMagicValue(file *ast.File, magicValue int) {
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
		binExpr.Y = ah.IntLit(magicValue)
		return false
	}

	ast.Inspect(file, updateMagic)
}
