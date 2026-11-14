// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/token"
	"strings"
)

// fips140Package returns the selected implementation of the runtime FIPS ABI.
func fips140Package() *listedPackage {
	if sharedCache == nil || sharedCache.ListedPackages == nil {
		return nil
	}
	if pkg, ok := sharedCache.ListedPackages.get("crypto/internal/fips140"); ok && len(pkg.CompiledGoFiles) > 0 {
		return pkg
	}
	for _, pkg := range sharedCache.ListedPackages.all() {
		if version, ok := strings.CutPrefix(pkg.baseImportPath(), "crypto/internal/fips140/v"); ok && !strings.Contains(version, "/") && len(pkg.CompiledGoFiles) > 0 {
			return pkg
		}
	}
	return nil
}

// localizeFIPSProxies makes generated literal proxy roots on demand instead of
// initializing global storage. SHA-256 and HMAC initialize before the integrity
// check that uses them; mutating their hashed data would invalidate the checksum.
// The decoder and key indirection remain, without any unchecked data exemption.
func localizeFIPSProxies(file *ast.File, original map[ast.Decl]bool) {
	roots := make(map[string]bool)
	for i, decl := range file.Decls {
		if original[decl] {
			continue
		}
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			if len(value.Names) != 1 || len(value.Values) != 1 {
				panic("unexpected literal proxy declaration")
			}
			lit := value.Values[0].(*ast.CompositeLit)
			roots[value.Names[0].Name] = true
			file.Decls[i] = &ast.FuncDecl{
				Name: value.Names[0],
				Type: &ast.FuncType{Func: gen.Pos(), Params: &ast.FieldList{Opening: gen.Pos(), Closing: gen.Pos()}, Results: &ast.FieldList{Opening: gen.Pos(), Closing: gen.Pos(), List: []*ast.Field{{Type: lit.Type}}}},
				Body: &ast.BlockStmt{Lbrace: gen.Pos(), List: []ast.Stmt{&ast.ReturnStmt{Return: gen.Pos(), Results: value.Values}}, Rbrace: gen.Pos()},
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		// Root references are the base of generated selector paths. Matching only
		// that position leaves declaration names and field keys untouched.
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && roots[id.Name] {
			sel.X = &ast.CallExpr{Fun: id, Lparen: id.Pos(), Rparen: id.End()}
		}
		return true
	})
}
