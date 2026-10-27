// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

const (
	maxChunkSize = 4
	minCaseCount = 3
)

// Split obfuscator splits data into chunks of random length and shuffles them,
// then encrypts them using xor.
type split struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = split{}

func splitIntoRandomChunks(obfRand *mathrand.Rand, data []byte) [][]byte {
	if len(data) == 1 {
		return [][]byte{data}
	}

	var chunks [][]byte
	for len(data) > 0 {
		chunkSize := min(1+obfRand.Intn(maxChunkSize), len(data))

		chunks = append(chunks, data[:chunkSize])
		data = data[chunkSize:]
	}
	return chunks
}

func splitIntoOneByteChunks(data []byte) [][]byte {
	var chunks [][]byte
	for _, d := range data {
		chunks = append(chunks, []byte{d})
	}
	return chunks
}

// The chunks stay intact, including their shuffled execution order and their
// per-chunk operators, but they are described by records interpreted by one
// loop. Emitting one append of a byte-slice literal per chunk made the decoder
// code grow by roughly a hundred bytes per literal byte, which dominated the
// binary size of -literals builds.
func (split) obfuscate(rand *mathrand.Rand, names *generatedNames, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	var chunks [][]byte
	// Short arrays should be divided into single-byte fragments
	if len(data)/maxChunkSize < minCaseCount {
		chunks = splitIntoOneByteChunks(data)
	} else {
		chunks = splitIntoRandomChunks(rand, data)
	}

	key := byte(rand.Uint32())
	operators := []token.Token{token.XOR, token.ADD, token.SUB}
	opcodes := rand.Perm(len(operators))

	// Destination offsets follow the original data order; the payload and the
	// record order are shuffled independently.
	offsets := make([]uint16, len(chunks))
	chunkOps := make([]int, len(chunks))
	var offset uint16
	for i, chunk := range chunks {
		offsets[i] = offset
		offset += uint16(len(chunk))
		chunkOps[i] = rand.Intn(len(operators))
	}

	for i, chunk := range chunks {
		start := int(offsets[i])
		for j, b := range chunk {
			chunk[j] = evalOperator(operators[chunkOps[i]], b, key^byte(start+j))
		}
	}

	order := rand.Perm(len(chunks))
	payload := make([]byte, 0, len(data))
	for _, i := range order {
		payload = append(payload, chunks[i]...)
	}

	recordOffsets := &ast.CompositeLit{Type: ah.ArrayType(nil, ast.NewIdent("uint16")), Elts: make([]ast.Expr, 0, len(order))}
	recordLens := &ast.CompositeLit{Type: ah.ArrayType(nil, ast.NewIdent("uint8")), Elts: make([]ast.Expr, 0, len(order))}
	recordOps := &ast.CompositeLit{Type: ah.ArrayType(nil, ast.NewIdent("uint8")), Elts: make([]ast.Expr, 0, len(order))}
	for _, i := range order {
		recordOffsets.Elts = append(recordOffsets.Elts, ah.IntLit(int(offsets[i])))
		recordLens.Elts = append(recordLens.Elts, ah.IntLit(len(chunks[i])))
		recordOps.Elts = append(recordOps.Elts, ah.IntLit(opcodes[chunkOps[i]]))
	}

	handlers := &ast.CompositeLit{Type: ah.ArrayType(nil, &ast.FuncType{
		Params:  &ast.FieldList{List: []*ast.Field{ah.Field(ast.NewIdent("byte")), ah.Field(ast.NewIdent("byte"))}},
		Results: &ast.FieldList{List: []*ast.Field{ah.Field(ast.NewIdent("byte"))}},
	}), Elts: make([]ast.Expr, len(operators))}
	for i, op := range operators {
		handlers.Elts[opcodes[i]] = &ast.FuncLit{
			Type: &ast.FuncType{
				Params:  &ast.FieldList{List: []*ast.Field{ah.Field(ast.NewIdent("byte"), names.ident("x")), ah.Field(ast.NewIdent("byte"), names.ident("y"))}},
				Results: &ast.FieldList{List: []*ast.Field{ah.Field(ast.NewIdent("byte"))}},
			},
			Body: ah.BlockStmt(ah.ReturnStmt(operatorToReversedBinaryExpr(op, names.ident("x"), names.ident("y")))),
		}
	}

	return ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("payload"), ah.StringLit(string(payload))),
		ah.AssignDefineStmt(names.ident("offsets"), recordOffsets),
		ah.AssignDefineStmt(names.ident("lens"), recordLens),
		ah.AssignDefineStmt(names.ident("ops"), recordOps),
		ah.AssignDefineStmt(names.ident("handlers"), handlers),
		ah.AssignDefineStmt(names.ident("key"), ah.CallExprByName("byte", byteLitWithExtKey(rand, key, extKeys, normalProb))),
		ah.AssignDefineStmt(names.ident("data"),
			ah.CallExprByName("make", ah.ByteSliceType(), ah.IntLit(len(data)))),
		ah.AssignDefineStmt(names.ident("src"), ah.IntLit(0)),
		&ast.ForStmt{
			Init: ah.AssignDefineStmt(names.ident("r"), ah.IntLit(0)),
			Cond: ah.BinaryExpr(names.ident("r"), token.LSS, ah.CallExprByName("len", names.ident("offsets"))),
			Post: &ast.IncDecStmt{X: names.ident("r"), Tok: token.INC},
			Body: ah.BlockStmt(
				ah.AssignDefineStmt(names.ident("dst"), ah.CallExprByName("int", ah.IndexExpr(names.name("offsets"), names.ident("r")))),
				ah.AssignDefineStmt(names.ident("n"), ah.CallExprByName("int", ah.IndexExpr(names.name("lens"), names.ident("r")))),
				ah.AssignDefineStmt(names.ident("op"), ah.IndexExpr(names.name("handlers"), ah.IndexExpr(names.name("ops"), names.ident("r")))),
				&ast.ForStmt{
					Init: ah.AssignDefineStmt(names.ident("j"), ah.IntLit(0)),
					Cond: ah.BinaryExpr(names.ident("j"), token.LSS, names.ident("n")),
					Post: &ast.IncDecStmt{X: names.ident("j"), Tok: token.INC},
					Body: ah.BlockStmt(
						ah.AssignStmt(
							ah.IndexExpr(names.name("data"), ah.BinaryExpr(names.ident("dst"), token.ADD, names.ident("j"))),
							ah.CallExpr(
								names.ident("op"),
								ah.IndexExpr(names.name("payload"), ah.BinaryExpr(names.ident("src"), token.ADD, names.ident("j"))),
								ah.BinaryExpr(names.ident("key"), token.XOR,
									ah.CallExprByName("byte", ah.BinaryExpr(names.ident("dst"), token.ADD, names.ident("j")))),
							),
						),
					),
				},
				&ast.AssignStmt{
					Lhs: []ast.Expr{names.ident("src")},
					Tok: token.ADD_ASSIGN,
					Rhs: []ast.Expr{names.ident("n")},
				},
			),
		},
	)
}
