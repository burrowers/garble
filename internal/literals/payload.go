// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	"slices"

	ah "mvdan.cc/garble/internal/asthelper"
)

type sharedPayload struct {
	name string
	key  []byte
}

// Only the blob family has a bounded-size decoder and a separable byte key.
// Constants carry no initialization dependencies or function-retention edges.
// String literals keep the plain path: an identical string within one owner
// already shares a single decoder, and so its ciphertext, via stringCalls.
func (r *obfRand) obfuscatePayload(obf obfuscator, names *generatedNames, data []byte, keys []*externalKey) *ast.BlockStmt {
	if _, ok := obf.(blob); !ok || r.payloads == nil {
		return obf.obfuscate(r.rnd, names, data, keys)
	}
	payload := r.payloads[string(data)]
	if payload == nil {
		encoded := EncodeBlob(r.rnd, data)
		payload = &sharedPayload{
			name: r.nextLiftedFuncName("literalPayload"),
			key:  slices.Clone(encoded[len(BlobMagic):BlobHeaderSize]),
		}
		r.payloads[string(data)] = payload
		r.payloadDecls = append(r.payloadDecls, &ast.GenDecl{
			Tok: token.CONST,
			Specs: []ast.Spec{&ast.ValueSpec{
				Names:  []*ast.Ident{ast.NewIdent(payload.name)},
				Values: []ast.Expr{ah.StringLit(string(encoded[BlobHeaderSize:]))},
			}},
		})
	}
	// Re-encode the canonical key independently at every site. External key
	// parameters must participate in decoding, not merely decorate the call.
	key := dataToByteSliceWithExtKeys(r.rnd, names, slices.Clone(payload.key), keys)
	return ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("key"), key),
		ah.AssignDefineStmt(names.ident("data"), ah.CallExpr(ah.ByteSliceType(), ast.NewIdent(payload.name))),
		&ast.RangeStmt{
			Key: names.ident("i"), Tok: token.DEFINE, X: names.ident("data"),
			Body: ah.BlockStmt(ah.AssignStmt(
				ah.IndexExpr(names.name("data"), names.ident("i")),
				ah.BinaryExpr(ah.IndexExpr(names.name("data"), names.ident("i")), token.XOR,
					ah.IndexExpr(names.name("key"), ah.BinaryExpr(names.ident("i"), token.REM, ah.IntLit(len(payload.key))))),
			)),
		},
		ah.AssignStmt(names.ident("data"), &ast.SliceExpr{
			X: names.ident("data"), High: ah.IntLit(len(data)), Max: ah.IntLit(len(data)),
		}),
	)
}
