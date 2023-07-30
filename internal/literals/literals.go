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

	"golang.org/x/tools/go/ast/astutil"
	ah "mvdan.cc/garble/internal/asthelper"
)

// MinSize is the lower bound limit, of the size of string-like literals
// which we will obfuscate. This is needed in order for binary size to stay relatively
// moderate, this also decreases the likelihood for performance slowdowns.
const MinSize = 8

// maxSize is the upper limit of the size of string-like literals
// which we will obfuscate with any of the available obfuscators.
// Beyond that we apply only a subset of obfuscators which are guaranteed to run efficiently.
const maxSize = 2 << 10 // KiB

// Obfuscate replaces literals with obfuscated anonymous functions.
func Obfuscate(rand *mathrand.Rand, file *ast.File, info *types.Info, linkStrings map[*types.Var]string) *ast.File {
	obfRand := newObfRand(rand, file)
	pre := func(cursor *astutil.Cursor) bool {
		switch node := cursor.Node().(type) {
		case *ast.GenDecl:
			// constants are obfuscated by replacing all references with the obfuscated value
			if node.Tok == token.CONST {
				return false
			}
		case *ast.ValueSpec:
			for _, name := range node.Names {
				obj := info.Defs[name].(*types.Var)
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
			if len(value) < MinSize {
				return true
			}

			cursor.Replace(withPos(obfuscateString(obfRand, value), node.Pos()))

			return true
		}

		switch node := node.(type) {
		case *ast.UnaryExpr:
			// Account for the possibility of address operators like
			// &[]byte used inline with function arguments.
			//
			// See issue #520.

			if node.Op != token.AND {
				return true
			}

			if child, ok := node.X.(*ast.CompositeLit); ok {
				newnode := handleCompositeLiteral(obfRand, true, child, info)
				if newnode != nil {
					cursor.Replace(newnode)
				}
			}

		case *ast.CompositeLit:
			// We replaced the &[]byte{...} case above. Here we account for the
			// standard []byte{...} or [4]byte{...} value form.
			//
			// We need two separate calls to cursor.Replace, as it only supports
			// replacing the node we're currently visiting, and the pointer variant
			// requires us to move the ampersand operator.

			parent, ok := cursor.Parent().(*ast.UnaryExpr)
			if ok && parent.Op == token.AND {
				return true
			}

			newnode := handleCompositeLiteral(obfRand, false, node, info)
			if newnode != nil {
				cursor.Replace(newnode)
			}
		}

		return true
	}

	return astutil.Apply(file, pre, post).(*ast.File)
}

// handleCompositeLiteral checks if the input node is []byte or [...]byte and
// calls the appropriate obfuscation method, returning a new node that should
// be used to replace it.
//
// If the input node cannot be obfuscated nil is returned.
func handleCompositeLiteral(obfRand *obfRand, isPointer bool, node *ast.CompositeLit, info *types.Info) ast.Node {
	if len(node.Elts) < MinSize {
		return nil
	}

	byteType := types.Universe.Lookup("byte").Type()

	var arrayLen int64
	switch y := info.TypeOf(node.Type).(type) {
	case *types.Array:
		if y.Elem() != byteType {
			return nil
		}

		arrayLen = y.Len()

	case *types.Slice:
		if y.Elem() != byteType {
			return nil
		}

	default:
		return nil
	}

	data := make([]byte, 0, len(node.Elts))

	for _, el := range node.Elts {
		elType := info.Types[el]

		if elType.Value == nil || elType.Value.Kind() != constant.Int {
			return nil
		}

		value, ok := constant.Uint64Val(elType.Value)
		if !ok {
			panic(fmt.Sprintf("cannot parse byte value: %v", elType.Value))
		}

		data = append(data, byte(value))
	}

	if arrayLen > 0 {
		return withPos(obfuscateByteArray(obfRand, isPointer, data, arrayLen), node.Pos())
	}

	return withPos(obfuscateByteSlice(obfRand, isPointer, data), node.Pos())
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

func obfuscateString(obfRand *obfRand, data string) *ast.CallExpr {
	obf := getNextObfuscator(obfRand, len(data))
	block := obf.obfuscate(obfRand.Rand, []byte(data))

	block.List = append(block.List, ah.ReturnStmt(ah.CallExpr(ast.NewIdent("string"), ast.NewIdent("data"))))

	return ah.LambdaCall(ast.NewIdent("string"), block)
}

func obfuscateByteSlice(obfRand *obfRand, isPointer bool, data []byte) *ast.CallExpr {
	obf := getNextObfuscator(obfRand, len(data))
	block := obf.obfuscate(obfRand.Rand, data)

	if isPointer {
		block.List = append(block.List, ah.ReturnStmt(&ast.UnaryExpr{
			Op: token.AND,
			X:  ast.NewIdent("data"),
		}))
		return ah.LambdaCall(&ast.StarExpr{
			X: &ast.ArrayType{Elt: ast.NewIdent("byte")},
		}, block)
	}

	block.List = append(block.List, ah.ReturnStmt(ast.NewIdent("data")))
	return ah.LambdaCall(&ast.ArrayType{Elt: ast.NewIdent("byte")}, block)
}

func obfuscateByteArray(obfRand *obfRand, isPointer bool, data []byte, length int64) *ast.CallExpr {
	obf := getNextObfuscator(obfRand, len(data))
	block := obf.obfuscate(obfRand.Rand, data)

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
	}

	var retexpr ast.Expr = ast.NewIdent("newdata")
	if isPointer {
		retexpr = &ast.UnaryExpr{X: retexpr, Op: token.AND}
	}

	sliceToArray = append(sliceToArray, ah.ReturnStmt(retexpr))
	block.List = append(block.List, sliceToArray...)

	if isPointer {
		return ah.LambdaCall(&ast.StarExpr{X: arrayType}, block)
	}

	return ah.LambdaCall(arrayType, block)
}

func getNextObfuscator(obfRand *obfRand, size int) obfuscator {
	if size <= maxSize {
		return obfRand.nextObfuscator()
	} else {
		return obfRand.nextLinearTimeObfuscator()
	}
}
