// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"

	ah "mvdan.cc/garble/internal/asthelper"
)

// maxSizeShared bounds the string literals decoded by a shared per-file decoder.
// Short literals are the majority of all literals, and each one otherwise pays
// for its own lifted function, its per-function metadata and its own copy of a
// decode loop. One decoder per operator and file can serve all of them, with
// only the encrypted payload and its key left per literal.
const maxSizeShared = 24

// sharedDecoder returns the proxy-hidden decoder for op, generating it once per
// file. Each returned expression is an independent copy, since one AST node
// cannot be shared between several call sites.
func (r *obfRand) sharedDecoder(op token.Token) ast.Expr {
	if r.sharedDecoders == nil {
		r.sharedDecoders = make(map[sharedDecoderKey]*sharedHiddenDecoder)
	}
	key := sharedDecoderKey{dispatcher: r.proxyDispatcher, op: op}
	if ref, ok := r.sharedDecoders[key]; ok {
		// The path belongs to the dispatcher which created it, so its clones
		// must be registered there even when another owner calls it.
		return ref.dispatcher.cloneStringCall(ref.hidden)
	}
	dispatcher := r.proxyDispatcher
	hidden := r.newSharedDecoder(op)
	r.sharedDecoders[key] = &sharedHiddenDecoder{dispatcher: dispatcher, hidden: hidden}
	return dispatcher.cloneStringCall(hidden)
}

// sharedDecoderKey scopes one shared decoder to an original function, the same
// retention boundary as its owner's proxy tree. Sharing across owners would let
// a live decoder keep unreachable code reachable.
type sharedDecoderKey struct {
	dispatcher *proxyDispatcher
	op         token.Token
}

// sharedHiddenDecoder records which owner dispatcher owns the shared decoder's
// proxy path. The path is attached at that dispatcher's AddToFile, so clones
// made later must be registered with that same dispatcher.
type sharedHiddenDecoder struct {
	dispatcher *proxyDispatcher
	hidden     ast.Expr
}

func (r *obfRand) newSharedDecoder(op token.Token) ast.Expr {
	names := newGeneratedNames(r)
	name := r.nextLiftedFuncName("literalSharedDecoder")
	_, castType := r.stringConversionAdapter()

	params := &ast.FieldList{List: []*ast.Field{
		ah.Field(ast.NewIdent("string"), names.ident("payload")),
		ah.Field(ast.NewIdent("uint64"), names.ident("key")),
		ah.Field(ast.NewIdent("int"), names.ident("low")),
		ah.Field(ast.NewIdent("int"), names.ident("high")),
		ah.Field(cloneGeneratedType(castType), names.ident("cast")),
	}}
	body := ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("data"),
			ah.CallExprByName("make", ah.ByteSliceType(), ah.CallExprByName("len", names.ident("payload")))),
		ah.AssignDefineStmt(names.ident("mask"), ah.CallExprByName("byte", names.ident("key"))),
		ah.AssignDefineStmt(names.ident("seed"), ah.CallExprByName("byte", ah.BinaryExpr(names.ident("key"), token.SHR, ah.IntLit(8)))),
		&ast.ForStmt{
			Init: ah.AssignDefineStmt(names.ident("i"), ah.IntLit(0)),
			Cond: ah.BinaryExpr(names.ident("i"), token.LSS, ah.CallExprByName("len", names.ident("payload"))),
			Post: &ast.IncDecStmt{X: names.ident("i"), Tok: token.INC},
			Body: ah.BlockStmt(
				ah.AssignDefineStmt(names.ident("b"),
					ah.BinaryExpr(ah.IndexExpr(names.name("payload"), names.ident("i")), token.XOR, names.ident("mask")),
				),
				ah.AssignStmt(
					ah.IndexExpr(names.name("data"), names.ident("i")),
					operatorToReversedBinaryExpr(op, names.ident("b"), names.ident("seed")),
				),
				&ast.AssignStmt{
					Lhs: []ast.Expr{names.ident("seed")},
					Tok: token.ADD_ASSIGN,
					Rhs: []ast.Expr{names.ident("b")},
				},
			),
		},
		ah.ReturnStmt(ah.CallExpr(names.ident("cast"), names.ident("data"), names.ident("low"), names.ident("high"))),
	)

	decl := &ast.FuncDecl{
		Name: ast.NewIdent(name),
		Type: &ast.FuncType{
			Params:  params,
			Results: &ast.FieldList{List: []*ast.Field{{Type: ast.NewIdent("string")}}},
		},
		Body: body,
	}
	r.sharedDecls = append(r.sharedDecls, decl)

	return r.proxyDispatcher.HideValue(ast.NewIdent(name), &ast.FuncType{
		Params:  unnamedFieldList(params),
		Results: &ast.FieldList{List: []*ast.Field{{Type: ast.NewIdent("string")}}},
	})
}

// obfuscateSharedString decodes a short literal through the file's shared
// decoder. The payload keeps the usual leading and trailing junk, which the
// decoder trims through the low and high bounds.
func (or *obfRand) obfuscateSharedString(data string) *ast.CallExpr {
	junkBytes := make([]byte, or.rnd.Intn(maxStringJunkBytes-minStringJunkBytes)+minStringJunkBytes)
	or.rnd.Read(junkBytes)
	splitIdx := or.rnd.Intn(len(junkBytes))

	plainData := []byte(data)
	payload := append(append(append([]byte(nil), junkBytes[:splitIdx]...), plainData...), junkBytes[splitIdx:]...)

	op := randOperator(or.rnd)
	key := or.rnd.Uint64()
	mask := byte(key)
	seed := byte(key >> 8)

	encrypted := make([]byte, len(payload))
	for i, b := range payload {
		encB := evalOperator(op, b, seed)
		seed += encB

		encrypted[i] = encB ^ mask
	}

	hiddenCast, _ := or.stringConversionAdapter()
	call := ah.CallExpr(or.sharedDecoder(op),
		ah.StringLit(string(encrypted)),
		ah.UintLit(key),
		ah.IntLit(splitIdx),
		ah.IntLit(splitIdx+len(plainData)),
		hiddenCast,
	)
	if or.stringCalls != nil {
		or.stringCalls[data] = call
		return or.proxyDispatcher.cloneStringCall(call).(*ast.CallExpr)
	}
	return call
}
