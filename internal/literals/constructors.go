// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

// ConstructorSummaries proves a deliberately small, terminating subset of Go:
// a single return built from parameters, constants and other proven constructors.
// Keys use Func.FullName so summaries also apply to imported type objects.
func ConstructorSummaries(files []*ast.File, info *types.Info, imported map[string]bool) map[string]bool {
	result := make(map[string]bool)
	bodies := make(map[*types.Func]*ast.FuncDecl)
	linknamed := make(map[string]bool)
	for _, file := range files {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				fields := strings.Fields(comment.Text)
				if len(fields) >= 2 && (fields[0] == "//go:linkname" || fields[0] == "//go:linknamestd") {
					linknamed[fields[1]] = true
				}
			}
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if obj, ok := info.Defs[fn.Name].(*types.Func); ok {
					bodies[obj] = fn
				}
			}
		}
	}
	seen := make(map[*types.Func]bool)
	var prove func(*types.Func) bool
	prove = func(fn *types.Func) bool {
		name := fn.FullName()
		if seen[fn] {
			return result[name]
		}
		seen[fn] = true // Cycles fail closed, including indirect recursion.
		decl := bodies[fn]
		if decl == nil {
			result[name] = imported[name]
			return result[name]
		}
		sig := fn.Signature()
		if linknamed[fn.Name()] || sig.Variadic() || sig.TypeParams().Len() != 0 || sig.RecvTypeParams().Len() != 0 || sig.Results().Len() != 1 || decl.Body == nil || len(decl.Body.List) != 1 {
			return false
		}
		params := make(map[types.Object]bool)
		for i := range sig.Params().Len() {
			params[sig.Params().At(i)] = true
		}
		if recv := sig.Recv(); recv != nil {
			if _, ok := recv.Type().Underlying().(*types.Pointer); ok {
				return false
			}
			params[recv] = true
		}
		ret, ok := decl.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return false
		}
		result[name] = constructorVariables(ret.Results[0], info, params) && constructorExpr(ret.Results[0], info, params, prove)
		return result[name]
	}
	for fn := range bodies {
		prove(fn)
	}
	for name, pure := range result {
		if !pure {
			delete(result, name)
		}
	}
	return result
}

func constructorFunc(call *ast.CallExpr, info *types.Info) (*types.Func, ast.Expr) {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[fun].(*types.Func)
		return fn, nil
	case *ast.SelectorExpr:
		if sel := info.Selections[fun]; sel != nil {
			// Promoted/pointer methods can introduce an implicit nil dereference.
			if sel.Kind() != types.MethodVal || sel.Indirect() || len(sel.Index()) != 1 {
				return nil, nil
			}
			if _, ok := sel.Recv().Underlying().(*types.Pointer); ok {
				return nil, nil
			}
			fn, _ := sel.Obj().(*types.Func)
			return fn, fun.X
		}
		if id, ok := fun.X.(*ast.Ident); ok {
			if _, ok := info.Uses[id].(*types.PkgName); ok {
				fn, _ := info.Uses[fun.Sel].(*types.Func)
				return fn, nil
			}
		}
	}
	return nil, nil
}

func constructorVariables(expr ast.Expr, info *types.Info, params map[types.Object]bool) bool {
	safe := true
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if v, ok := info.Uses[id].(*types.Var); ok && !v.IsField() && !params[v] {
				safe = false
			}
		}
		return safe
	})
	return safe
}

func constructorExpr(expr ast.Expr, info *types.Info, params map[types.Object]bool, prove func(*types.Func) bool) bool {
	if info.TypeOf(expr) == nil {
		return false
	} // Generated embed/linker expressions have no original proof.
	pure := func(expr ast.Expr) bool { return constructorExpr(expr, info, params, prove) }
	switch expr := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		obj := info.Uses[expr]
		if _, ok := obj.(*types.Const); ok {
			return true
		}
		return obj == types.Universe.Lookup("nil") || params[obj]
	case *ast.SelectorExpr:
		_, ok := info.Uses[expr.Sel].(*types.Const)
		return ok // No field loads, package variables or method values.
	case *ast.UnaryExpr:
		return info.Types[expr].Value != nil && pure(expr.X)
	case *ast.BinaryExpr:
		return info.Types[expr].Value != nil && pure(expr.X) && pure(expr.Y)
	case *ast.CompositeLit:
		switch info.TypeOf(expr).Underlying().(type) {
		case *types.Struct, *types.Array:
		default:
			return false
		}
		for _, elt := range expr.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			if !pure(elt) {
				return false
			}
		}
		return true
	case *ast.CallExpr:
		if expr.Ellipsis != token.NoPos {
			return false
		}
		if info.Types[expr.Fun].IsType() {
			if len(expr.Args) != 1 || info.TypeOf(expr.Args[0]) == nil {
				return false
			}
			to, ok := info.TypeOf(expr).Underlying().(*types.Basic)
			from, fromOK := info.TypeOf(expr.Args[0]).Underlying().(*types.Basic)
			return ok && fromOK && to.Kind() != types.UnsafePointer && from.Kind() != types.UnsafePointer && pure(expr.Args[0])
		}
		fn, recv := constructorFunc(expr, info)
		if fn == nil || !prove(fn) || (recv != nil && !pure(recv)) {
			return false
		}
		for _, arg := range expr.Args {
			if !pure(arg) {
				return false
			}
		}
		return true
	}
	return false
}

// constructorResult names only non-generic value types already in file scope.
// Unknown or inaccessible result types leave the initializer strong.
func constructorResult(file *ast.File, info *types.Info, typ types.Type) ast.Expr {
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok || named.TypeArgs().Len() != 0 || named.Obj().Pkg() == nil {
		return nil
	}
	obj := named.Obj()
	for _, imp := range file.Imports {
		var pkg *types.PkgName
		if imp.Name != nil {
			pkg, _ = info.Defs[imp.Name].(*types.PkgName)
		} else {
			pkg, _ = info.Implicits[imp].(*types.PkgName)
		}
		if pkg == nil || pkg.Imported().Path() != obj.Pkg().Path() || pkg.Name() == "_" || !obj.Exported() {
			continue
		}
		ident := ast.NewIdent(obj.Name())
		info.Uses[ident] = obj
		if pkg.Name() == "." {
			return ident
		}
		qualifier := ast.NewIdent(pkg.Name())
		info.Uses[qualifier] = pkg
		return &ast.SelectorExpr{X: qualifier, Sel: ident}
	}
	// A declaration object in this package establishes a local result name.
	for _, defined := range info.Defs {
		if defined == obj {
			ident := ast.NewIdent(obj.Name())
			info.Uses[ident] = obj
			return ident
		}
	}
	return nil
}
