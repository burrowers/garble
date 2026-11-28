// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
)

// portableExportInfo keeps the literal pass from baking values which may differ
// on another target. Other type information, including object identities used by
// renaming, remains shared with the original package.
func portableExportInfo(file *ast.File, info *types.Info) *types.Info {
	portable := *info
	portable.Types = maps.Clone(info.Types)
	for node := range ast.Preorder(file) {
		if call, ok := node.(*ast.CallExpr); ok && len(call.Args) == 1 && info.Types[call.Fun].IsType() {
			if slice, ok := info.TypeOf(call.Fun).Underlying().(*types.Slice); ok &&
				(slice.Elem() == types.Universe.Lookup("byte").Type() || slice.Elem() == types.Universe.Lookup("rune").Type()) {
				arg := call.Args[0]
				if value := info.Types[arg].Value; value != nil && value.Kind() == constant.String && !portableLiteralExpr(arg) {
					// Keep the whole dependent constant argument in source:
					// decoding even its plain prefix loses exact slice capacity.
					// Inspect original Info before masking, and retain integer
					// values so unevaluated operands still prune the walk.
					for nested := range ast.Preorder(arg) {
						if expr, ok := nested.(ast.Expr); ok {
							value := portable.Types[expr]
							if value.Value != nil && value.Value.Kind() == constant.String {
								value.Value = nil
								portable.Types[expr] = value
							}
						}
					}
				}
			}
		}
		if lit, ok := node.(*ast.CompositeLit); ok {
			var elem types.Type
			switch typ := info.TypeOf(lit.Type).(type) {
			case *types.Array:
				elem = typ.Elem()
			case *types.Slice:
				elem = typ.Elem()
			}
			if elem == types.Universe.Lookup("byte").Type() && !portableCompositeLiteral(lit) {
				// Only the composite decoder reads this type. Mask it rather
				// than its integer operands, which must still prune the walk
				// through unevaluated len/unsafe.Sizeof arguments.
				portable.Types[lit.Type] = types.TypeAndValue{}
			}
		}
		if expr, ok := node.(ast.Expr); ok {
			value := portable.Types[expr]
			if lit, ok := ast.Unparen(expr).(*ast.CompositeLit); ok {
				if array, ok := lit.Type.(*ast.ArrayType); ok {
					if _, inferred := array.Len.(*ast.Ellipsis); inferred {
						for _, elem := range lit.Elts {
							if keyed, ok := elem.(*ast.KeyValueExpr); ok && !portableLiteralExpr(keyed.Key) {
								// A global factory would replace [...] with this
								// target's length. Decline it, including through
								// parentheses, without masking integer constants.
								value.Type = nil
								portable.Types[expr] = value
								break
							}
						}
					}
				}
			}
			if binary, ok := expr.(*ast.BinaryExpr); ok && binary.Op == token.ADD && value.Type == types.Typ[types.String] {
				// Untyped operands can be decoded only when the concatenation
				// accepts plain strings; a named string requires its own type.
				for _, operand := range []ast.Expr{binary.X, binary.Y} {
					if literal, ok := operand.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						v := portable.Types[literal]
						if v.Type == types.Typ[types.UntypedString] {
							v.Type = types.Typ[types.String]
							portable.Types[literal] = v
						}
					}
				}
			}
			if value.Value != nil && value.Value.Kind() == constant.String && !portableLiteralExpr(expr) {
				value.Value = nil
				portable.Types[expr] = value
			}
		}
	}
	return &portable
}

func portableCompositeLiteral(lit *ast.CompositeLit) bool {
	array, ok := lit.Type.(*ast.ArrayType)
	if !ok {
		return false
	}
	if _, inferred := array.Len.(*ast.Ellipsis); array.Len != nil && !inferred && !portableLiteralExpr(array.Len) {
		return false
	}
	for _, elem := range lit.Elts {
		if !portableLiteralExpr(elem) {
			return false
		}
	}
	return true
}

// References and calls are deliberately excluded, even for constants declared
// locally: their declarations can come from a different platform-specific file.
func portableLiteralExpr(expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.ParenExpr:
		return portableLiteralExpr(expr.X)
	case *ast.UnaryExpr:
		return portableLiteralExpr(expr.X)
	case *ast.BinaryExpr:
		return portableLiteralExpr(expr.X) && portableLiteralExpr(expr.Y)
	default:
		return false
	}
}
