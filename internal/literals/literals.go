// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"strconv"

	"golang.org/x/tools/go/ast/astutil"
	ah "mvdan.cc/garble/internal/asthelper"
)

// maxSizeBytes is the limit, in bytes, of the size of string-like literals
// which we will obfuscate. This is important, because otherwise garble can take
// a very long time to obfuscate huge code-generated literals, such as those
// corresponding to large assets.
//
// Note that this is the size of the literal in source code. For example, "\xab"
// counts as four bytes.
//
// If someone truly wants to obfuscate those, they should do that when they
// generate the code, not at build time. Plus, with Go 1.16 that technique
// should largely stop being used.
const maxSizeBytes = 2 << 10 // KiB

func randObfuscator() obfuscator {
	randPos := mathrand.Intn(len(obfuscators))
	return obfuscators[randPos]
}

// Obfuscate replaces literals with obfuscated anonymous functions.
func Obfuscate(file *ast.File, info *types.Info, fset *token.FileSet, ignoreObj map[types.Object]bool) *ast.File {
	pre := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.GenDecl:
			if x.Tok != token.CONST {
				return true
			}
			for _, spec := range x.Specs {
				spec := spec.(*ast.ValueSpec) // guaranteed for Tok==CONST
				if len(spec.Values) == 0 {
					// skip constants with inferred values
					return false
				}

				for _, name := range spec.Names {
					obj := info.ObjectOf(name)

					basic, ok := obj.Type().(*types.Basic)
					if !ok {
						// skip the block if it contains non basic types
						return false
					}

					if basic.Info()&types.IsUntyped != 0 {
						// skip the block if it contains untyped constants
						return false
					}

					// The object cannot be obfuscated, e.g. a value that needs to be constant
					if ignoreObj[obj] {
						return false
					}
				}
			}

			x.Tok = token.VAR
			// constants are not possible if we want to obfuscate literals, therefore
			// move all constant blocks which only contain strings to variables
		}
		return true
	}

	post := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.CompositeLit:
			byteType := types.Universe.Lookup("byte").Type()

			if len(x.Elts) == 0 {
				return true
			}

			switch y := info.TypeOf(x.Type).(type) {
			case *types.Array:
				if y.Elem() != byteType {
					return true
				}
				if y.Len() > maxSizeBytes {
					return true
				}

				data := make([]byte, y.Len())

				for i, el := range x.Elts {
					lit, ok := el.(*ast.BasicLit)
					if !ok {
						return true
					}

					value, err := strconv.Atoi(lit.Value)
					if err != nil {
						return true
					}

					data[i] = byte(value)
				}
				cursor.Replace(withPos(obfuscateByteArray(data, y.Len()), x.Pos()))

			case *types.Slice:
				if y.Elem() != byteType {
					return true
				}
				if len(x.Elts) > maxSizeBytes {
					return true
				}

				data := make([]byte, 0, len(x.Elts))

				for _, el := range x.Elts {
					lit, ok := el.(*ast.BasicLit)
					if !ok {
						return true
					}

					value, err := strconv.Atoi(lit.Value)
					if err != nil {
						return true
					}

					data = append(data, byte(value))
				}
				cursor.Replace(withPos(obfuscateByteSlice(data), x.Pos()))

			}

		case *ast.BasicLit:
			switch cursor.Name() {
			case "Values", "Rhs", "Value", "Args", "X", "Y", "Results":
			default:
				return true // we don't want to obfuscate imports etc.
			}

			if x.Kind != token.STRING {
				return true
			}
			if len(x.Value) > maxSizeBytes {
				return true
			}
			typeInfo := info.TypeOf(x)
			if typeInfo != types.Typ[types.String] && typeInfo != types.Typ[types.UntypedString] {
				return true
			}
			value, err := strconv.Unquote(x.Value)
			if err != nil {
				panic(fmt.Sprintf("cannot unquote string: %v", err))
			}

			if len(value) == 0 {
				return true
			}

			cursor.Replace(withPos(obfuscateString(value), x.Pos()))
		}

		return true
	}

	return astutil.Apply(file, pre, post).(*ast.File)
}

// withPos sets any token.Pos fields under node which affect printing to pos.
// Note that we can't set all token.Pos fields, since some affect the semantics.
//
// This function is useful so that go/printer doesn't try to estimate position
// offsets, which can end up in printing comment directives too early.
//
// We don't set any "end" or middle positions, because they seem irrelevant.
func withPos(node ast.Node, pos token.Pos) ast.Node {
	ast.Inspect(node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BasicLit:
			node.ValuePos = pos
		case *ast.Ident:
			node.NamePos = pos
		case *ast.CompositeLit:
			node.Lbrace = pos
			node.Rbrace = pos
		case *ast.ArrayType:
			node.Lbrack = pos
		case *ast.FuncType:
			node.Func = pos
		case *ast.BinaryExpr:
			node.OpPos = pos
		case *ast.StarExpr:
			node.Star = pos
		case *ast.CallExpr:
			node.Lparen = pos
			node.Rparen = pos

		case *ast.GenDecl:
			node.TokPos = pos
		case *ast.ReturnStmt:
			node.Return = pos
		case *ast.ForStmt:
			node.For = pos
		case *ast.RangeStmt:
			node.For = pos
		case *ast.BranchStmt:
			node.TokPos = pos
		}
		return true
	})
	return node
}

func obfuscateString(data string) *ast.CallExpr {
	obfuscator := randObfuscator()
	block := obfuscator.obfuscate([]byte(data))

	block.List = append(block.List, ah.ReturnStmt(ah.CallExpr(ast.NewIdent("string"), ast.NewIdent("data"))))

	return ah.LambdaCall(ast.NewIdent("string"), block)
}

func obfuscateByteSlice(data []byte) *ast.CallExpr {
	obfuscator := randObfuscator()
	block := obfuscator.obfuscate(data)
	block.List = append(block.List, ah.ReturnStmt(ast.NewIdent("data")))
	return ah.LambdaCall(&ast.ArrayType{Elt: ast.NewIdent("byte")}, block)
}

func obfuscateByteArray(data []byte, length int64) *ast.CallExpr {
	obfuscator := randObfuscator()
	block := obfuscator.obfuscate(data)

	arrayType := &ast.ArrayType{
		Len: ah.IntLit(int(length)),
		Elt: ast.NewIdent("byte"),
	}

	sliceToArray := []ast.Stmt{
		&ast.DeclStmt{
			Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent("newdata")},
					Type:  arrayType,
				}},
			},
		},
		&ast.RangeStmt{
			Key: ast.NewIdent("i"),
			Tok: token.DEFINE,
			X:   ast.NewIdent("newdata"),
			Body: &ast.BlockStmt{List: []ast.Stmt{
				&ast.AssignStmt{
					Lhs: []ast.Expr{ah.IndexExpr("newdata", ast.NewIdent("i"))},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{ah.IndexExpr("data", ast.NewIdent("i"))},
				},
			}},
		},
		ah.ReturnStmt(ast.NewIdent("newdata")),
	}

	block.List = append(block.List, sliceToArray...)

	return ah.LambdaCall(arrayType, block)
}

// RecordUsedAsConstants records identifieres used in constant expressions.
func RecordUsedAsConstants(node ast.Node, info *types.Info, ignoreObj map[types.Object]bool) {
	visit := func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}

		// Only record *types.Const objects.
		// Other objects, such as builtins or type names,
		// must not be recorded as they would be false positives.
		obj := info.ObjectOf(ident)
		if _, ok := obj.(*types.Const); ok {
			ignoreObj[obj] = true
		}

		return true
	}

	switch x := node.(type) {
	// in a slice or array composite literal all explicit keys must be constant representable
	case *ast.CompositeLit:
		if _, ok := x.Type.(*ast.ArrayType); !ok {
			break
		}
		for _, elt := range x.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				ast.Inspect(kv.Key, visit)
			}
		}
	// in an array type the length must be a constant representable
	case *ast.ArrayType:
		if x.Len != nil {
			ast.Inspect(x.Len, visit)
		}
	// in a const declaration all values must be constant representable
	case *ast.GenDecl:
		if x.Tok != token.CONST {
			break
		}
		for _, spec := range x.Specs {
			spec := spec.(*ast.ValueSpec)

			for _, val := range spec.Values {
				ast.Inspect(val, visit)
			}
		}
	}
}
