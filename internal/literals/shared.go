// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"

	ah "mvdan.cc/garble/internal/asthelper"
)

// Share short seed decoders only within an owner: an original function or method
// (including nested closures), a generated global factory, or the file's remaining
// package initializers. Sharing across dispatchers would retain dead decoders.
const maxSizeShared = 24

func (r *obfRand) sharedDecoder(op token.Token) ast.Expr {
	d := r.proxyDispatcher
	if d.sharedDecoders == nil {
		d.sharedDecoders = make(map[token.Token]ast.Expr)
	}
	if d.sharedDecoders[op] == nil {
		names := newGeneratedNames(r)
		_, castType := r.stringConversionAdapter()
		params := &ast.FieldList{List: []*ast.Field{
			ah.Field(ast.NewIdent("string"), names.ident("input")),
			ah.Field(ast.NewIdent("uint64"), names.ident("key")),
			ah.Field(ast.NewIdent("int"), names.ident("low")),
			ah.Field(ast.NewIdent("int"), names.ident("high")),
			ah.Field(cloneGeneratedType(castType), names.ident("cast")),
		}}
		body := seedBody(names, op, names.ident("input"),
			ah.CallExprByName("byte", names.ident("key")),
			ah.CallExprByName("byte", ah.BinaryExpr(names.ident("key"), token.SHR, ah.IntLit(8))))
		body.List = append(body.List, ah.ReturnStmt(ah.CallExpr(names.ident("cast"), names.ident("data"), names.ident("low"), names.ident("high"))))
		d.sharedDecoders[op] = r.liftCall(params, ast.NewIdent("string"), body, nil).Fun
	}
	// Register independent paths with the dispatcher that will attach them.
	return d.cloneStringCall(d.sharedDecoders[op])
}

func (or *obfRand) obfuscateSharedString(data string) *ast.CallExpr {
	junkBytes := make([]byte, or.rnd.Intn(maxStringJunkBytes-minStringJunkBytes)+minStringJunkBytes)
	or.rnd.Read(junkBytes)
	splitIdx := or.rnd.Intn(len(junkBytes))
	payload := append(append(junkBytes[:splitIdx:splitIdx], []byte(data)...), junkBytes[splitIdx:]...)
	op := randOperator(or.rnd)
	key := or.rnd.Uint64()
	encrypted := encryptSeed(payload, op, byte(key), byte(key>>8))
	hiddenCast, _ := or.stringConversionAdapter()
	call := ah.CallExpr(or.sharedDecoder(op),
		ah.StringLit(string(encrypted)), ah.UintLit(key),
		ah.IntLit(splitIdx), ah.IntLit(splitIdx+len(data)), hiddenCast)
	if or.stringCalls != nil {
		or.stringCalls[data] = call
		return or.proxyDispatcher.cloneStringCall(call).(*ast.CallExpr)
	}
	return call
}
