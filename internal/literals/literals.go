// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"fmt"
	"go/ast"
	"go/constant"
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
func Obfuscate(file *ast.File, info *types.Info, fset *token.FileSet, linkStrings map[types.Object]string) *ast.File {
	pre := func(cursor *astutil.Cursor) bool {
		switch node := cursor.Node().(type) {
		case *ast.GenDecl:
			// constants are obfuscated by replacing all references with the obfuscated value
			if node.Tok == token.CONST {
				return false
			}
		case *ast.ValueSpec:
			for _, name := range node.Names {
				obj := info.ObjectOf(name)
				if _, e := linkStrings[obj]; e {
					// Skip this entire ValueSpec to not break -ldflags=-X.
					// TODO: support obfuscating those injected strings, too.
					return false
				}
			}
		}
		return true
	}

	post := func(cursor *astutil.Cursor) bool {
		node, ok := cursor.Node().(ast.Expr)
		if !ok {
			return true
		}

		typeAndValue := info.Types[node]
		if !typeAndValue.IsValue() {
			return true
		}

		if typeAndValue.Type == types.Typ[types.String] && typeAndValue.Value != nil {
			value := constant.StringVal(typeAndValue.Value)
			if len(value) == 0 || len(value) > maxSizeBytes {
				return true
			}

			cursor.Replace(withPos(obfuscateString(value), node.Pos()))

			return true
		}

		if node, ok := node.(*ast.CompositeLit); ok {
			if len(node.Elts) == 0 || len(node.Elts) > maxSizeBytes {
				return true
			}

			byteType := types.Universe.Lookup("byte").Type()

			var arrayLen int64
			switch y := info.TypeOf(node.Type).(type) {
			case *types.Array:
				if y.Elem() != byteType {
					return true
				}

				arrayLen = y.Len()

			case *types.Slice:
				if y.Elem() != byteType {
					return true
				}

			default:
				return true
			}

			data := make([]byte, 0, len(node.Elts))

			for _, el := range node.Elts {
				elType := info.Types[el]

				if elType.Value == nil || elType.Value.Kind() != constant.Int {
					return true
				}

				value, ok := constant.Uint64Val(elType.Value)
				if !ok {
					panic(fmt.Sprintf("cannot parse byte value: %v", elType.Value))
				}

				data = append(data, byte(value))
			}

			if arrayLen > 0 {
				cursor.Replace(withPos(obfuscateByteArray(data, arrayLen), node.Pos()))
			} else {
				cursor.Replace(withPos(obfuscateByteSlice(data), node.Pos()))
			}
		}

		return true
	}

	// Imports which are used might be marked as unused by only looking at the ast,
	// because packages can declare a name different from the last element of their import path.
	//
	// 	package main
	//
	// 	import "github.com/user/somepackage"
	//
	//	func main(){
	//		// this line uses github.com/user/somepackage
	//		anotherpackage.Foo()
	//	}
	//
	// TODO: remove this check and detect used imports with go/types somehow
	prevUsedImports := make(map[string]bool)
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			panic(err)
		}
		prevUsedImports[path] = astutil.UsesImport(file, path)
	}

	file = astutil.Apply(file, pre, post).(*ast.File)

	// some imported constants might not be needed anymore, remove unnecessary imports
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			panic(err)
		}
		if !prevUsedImports[path] || astutil.UsesImport(file, path) {
			continue
		}

		if !astutil.DeleteImport(fset, file, path) {
			panic(fmt.Sprintf("cannot delete unused import: %v", path))
		}
	}

	return file
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
			X:   ast.NewIdent("data"),
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
