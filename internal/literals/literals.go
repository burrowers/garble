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
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	ah "mvdan.cc/garble/internal/asthelper"
)

// MinSize is the lower bound limit, of the size of string-like literals
// which we will obfuscate. This is needed in order for binary size to stay relatively
// moderate, this also decreases the likelihood for performance slowdowns.
const MinSize = 8

// MaxSize is the upper limit of the size of string-like literals we will obfuscate.
const MaxSize = 2 << 10 // 2 KiB

// MaxSizeExpensive is the upper limit for using expensive obfuscators (split, seed).
// Above this size, only cheap obfuscators are used.
const MaxSizeExpensive = 256

const (
	// minStringJunkBytes defines the minimum number of junk bytes to prepend or append during string obfuscation.
	minStringJunkBytes = 2
	// maxStringJunkBytes defines the maximum number of junk bytes to prepend or append during string obfuscation.
	maxStringJunkBytes = 8
)

// NameProviderFunc defines a function type that generates a string based on a random source and a base name.
type NameProviderFunc func(rand *mathrand.Rand, baseName string) string

// Obfuscate replaces literals with obfuscated anonymous functions.
func Obfuscate(rand *mathrand.Rand, file *ast.File, info *types.Info, linkStrings map[*types.Var]string, nameFunc NameProviderFunc) *ast.File {
	or := newObfRand(rand, file, nameFunc)
	parents := nodeParents(file)
	pre := func(cursor *astutil.Cursor) bool {
		switch node := cursor.Node().(type) {
		case *ast.FuncDecl:
			// Obfuscating literals can push the stack frame over the //go:nosplit limit,
			// which is just 800 bytes. These funcs are mostly in the runtime,
			// so obfuscating strings in these is less important in any case.
			if node.Doc != nil {
				for _, comment := range node.Doc.List {
					if strings.HasPrefix(comment.Text, "//go:nosplit") {
						return false
					}
				}
			}
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

		if value, ok := obfuscatableString(node, info, parents); ok {
			cursor.Replace(withPos(obfuscateString(or, value), node.Pos()))
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
			if hasConstantValueAncestor(node, info, parents) {
				return true
			}

			if child, ok := node.X.(*ast.CompositeLit); ok {
				newnode := handleCompositeLiteral(or, true, child, info)
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

			if hasConstantValueAncestor(node, info, parents) {
				// Constant len/cap/unsafe.Sizeof expressions do not evaluate an
				// array operand. A decoder would be needless and can make an
				// array length or keyed index stop being a constant.
				return true
			}

			parent, ok := cursor.Parent().(*ast.UnaryExpr)
			if ok && parent.Op == token.AND {
				return true
			}

			newnode := handleCompositeLiteral(or, false, node, info)
			if newnode != nil {
				cursor.Replace(newnode)
			}
		}

		return true
	}

	newFile := astutil.Apply(file, pre, post).(*ast.File)
	or.proxyDispatcher.AddToFile(newFile)
	return newFile
}

// nodeParents records the original syntax ancestry before rewrites begin. It
// lets string selection account for constant expressions above the immediate
// astutil cursor parent without depending on already-replaced child nodes.
func nodeParents(root ast.Node) map[ast.Node]ast.Node {
	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parents
}

// obfuscatableString selects only maximal materialized string constants.
//
// A larger eligible string expression will be rewritten when its post-order
// turn arrives, so generating decoders for its children would only leave dead
// generated expressions and proxy entries. Conversely, a surrounding constant
// expression with a non-string result (for example len(s), a comparison, or
// unsafe.Sizeof) is folded by the compiler. Rewriting a string below it is both
// unnecessary and can make constant-required uses such as array lengths fail.
func obfuscatableString(node ast.Expr, info *types.Info, parents map[ast.Node]ast.Node) (string, bool) {
	typeAndValue := info.Types[node]
	if typeAndValue.Type != types.Typ[types.String] || typeAndValue.Value == nil {
		return "", false
	}
	value := constant.StringVal(typeAndValue.Value)
	if len(value) < MinSize || len(value) > MaxSize {
		return "", false
	}

	for parent := parents[node]; parent != nil; parent = parents[parent] {
		expr, ok := parent.(ast.Expr)
		if !ok {
			continue
		}
		parentValue := info.Types[expr]
		if parentValue.Value == nil || parentValue.Type == nil {
			continue
		}
		if !isStringLike(parentValue.Type) {
			return "", false
		}
		if parentValue.Type == types.Typ[types.String] {
			parentString := constant.StringVal(parentValue.Value)
			if len(parentString) >= MinSize && len(parentString) <= MaxSize {
				return "", false
			}
		}
	}
	return value, true
}

func isStringLike(typ types.Type) bool {
	basic, ok := typ.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

func hasConstantValueAncestor(node ast.Node, info *types.Info, parents map[ast.Node]ast.Node) bool {
	for parent := parents[node]; parent != nil; parent = parents[parent] {
		if expr, ok := parent.(ast.Expr); ok && info.Types[expr].Value != nil {
			return true
		}
	}
	return false
}

// handleCompositeLiteral checks if the input node is []byte or [...]byte and
// calls the appropriate obfuscation method, returning a new node that should
// be used to replace it.
//
// If the input node cannot be obfuscated nil is returned.
func handleCompositeLiteral(or *obfRand, isPointer bool, node *ast.CompositeLit, info *types.Info) ast.Node {
	if len(node.Elts) < MinSize || len(node.Elts) > MaxSize {
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
		return withPos(obfuscateByteArray(or, isPointer, data, arrayLen), node.Pos())
	}

	return withPos(obfuscateByteSlice(or, isPointer, data), node.Pos())
}

// withPos sets any token.Pos fields under node which affect printing to pos.
// Note that we can't set all token.Pos fields, since some affect the semantics.
//
// This function is useful so that go/printer doesn't try to estimate position
// offsets, which can end up in printing comment directives too early.
//
// We don't set any "end" or middle positions, because they seem irrelevant.
func withPos(node ast.Node, pos token.Pos) ast.Node {
	for node := range ast.Preorder(node) {
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
	}
	return node
}

func obfuscateString(or *obfRand, data string) *ast.CallExpr {
	obf := or.pickObfuscator(len(data))

	// Generate junk bytes to to prepend and append to the data.
	// This is to prevent the obfuscated string from being easily fingerprintable.
	junkBytes := make([]byte, or.rnd.Intn(maxStringJunkBytes-minStringJunkBytes)+minStringJunkBytes)
	or.rnd.Read(junkBytes)
	splitIdx := or.rnd.Intn(len(junkBytes))

	extKeys := randExtKeys(or.rnd)

	plainData := []byte(data)
	plainDataWithJunkBytes := append(append(junkBytes[:splitIdx], plainData...), junkBytes[splitIdx:]...)

	block := obf.obfuscate(or.rnd, plainDataWithJunkBytes, extKeys)
	params, args := extKeysToParams(or, extKeys)

	// Generate unique cast bytes to string function and hide it using proxyDispatcher:
	//
	// func(x []byte) string {
	//		return string(x[<splitIdx>:<splitIdx+len(plainData)>])
	//	}
	funcTyp := &ast.FuncType{
		Params: &ast.FieldList{List: []*ast.Field{{
			Type: ah.ByteSliceType(),
		}}},
		Results: &ast.FieldList{List: []*ast.Field{{
			Type: ast.NewIdent("string"),
		}}},
	}
	funcVal := &ast.FuncLit{
		Type: &ast.FuncType{
			Params: &ast.FieldList{List: []*ast.Field{{
				Names: []*ast.Ident{ast.NewIdent("x")},
				Type:  ah.ByteSliceType(),
			}}},
			Results: &ast.FieldList{List: []*ast.Field{{
				Type: ast.NewIdent("string"),
			}}},
		},
		Body: ah.BlockStmt(
			ah.ReturnStmt(
				ah.CallExprByName("string",
					&ast.SliceExpr{
						X:    ast.NewIdent("x"),
						Low:  ah.IntLit(splitIdx),
						High: ah.IntLit(splitIdx + len(plainData)),
					},
				),
			),
		),
	}
	block.List = append(block.List, ah.ReturnStmt(ah.CallExpr(or.proxyDispatcher.HideValue(funcVal, funcTyp), ast.NewIdent("data"))))
	return ah.LambdaCall(params, ast.NewIdent("string"), block, args)
}

func obfuscateByteSlice(or *obfRand, isPointer bool, data []byte) *ast.CallExpr {
	obf := or.pickObfuscator(len(data))

	extKeys := randExtKeys(or.rnd)
	block := obf.obfuscate(or.rnd, data, extKeys)
	params, args := extKeysToParams(or, extKeys)

	if isPointer {
		block.List = append(block.List, ah.ReturnStmt(
			ah.UnaryExpr(token.AND, ast.NewIdent("data")),
		))
		return ah.LambdaCall(params, ah.StarExpr(ah.ByteSliceType()), block, args)
	}

	block.List = append(block.List, ah.ReturnStmt(ast.NewIdent("data")))
	return ah.LambdaCall(params, ah.ByteSliceType(), block, args)
}

func obfuscateByteArray(or *obfRand, isPointer bool, data []byte, length int64) *ast.CallExpr {
	obf := or.pickObfuscator(len(data))

	extKeys := randExtKeys(or.rnd)
	block := obf.obfuscate(or.rnd, data, extKeys)
	params, args := extKeysToParams(or, extKeys)

	arrayType := ah.ByteArrayType(length)

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
			Body: ah.BlockStmt(
				ah.AssignStmt(
					ah.IndexExprByExpr(ast.NewIdent("newdata"), ast.NewIdent("i")),
					ah.IndexExprByExpr(ast.NewIdent("data"), ast.NewIdent("i")),
				),
			),
		},
	}

	var retexpr ast.Expr = ast.NewIdent("newdata")
	if isPointer {
		retexpr = ah.UnaryExpr(token.AND, retexpr)
	}

	sliceToArray = append(sliceToArray, ah.ReturnStmt(retexpr))
	block.List = append(block.List, sliceToArray...)

	if isPointer {
		return ah.LambdaCall(params, ah.StarExpr(arrayType), block, args)
	}

	return ah.LambdaCall(params, arrayType, block, args)
}

func (or *obfRand) pickObfuscator(size int) obfuscator {
	if size < MinSize || size > MaxSize {
		panic(fmt.Sprintf("nextObfuscator called with size %d outside [%d, %d]", size, MinSize, MaxSize))
	}
	if or.testObfuscator != nil {
		return or.testObfuscator
	}
	if size <= MaxSizeExpensive {
		return Obfuscators[or.rnd.Intn(len(Obfuscators))]
	}
	return CheapObfuscators[or.rnd.Intn(len(CheapObfuscators))]
}
