// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"

	ah "mvdan.cc/garble/internal/asthelper"
)

type shuffle struct{}

var _ obfuscator = shuffle{}

func (shuffle) obfuscate(rand *mathrand.Rand, names *generatedNames, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	key := make([]byte, len(data))
	rand.Read(key)
	fullData := make([]byte, 2*len(data))
	positions := rand.Perm(len(fullData))
	operators := []token.Token{token.XOR, token.ADD, token.SUB}
	opcodes := rand.Perm(len(operators))
	fields := rand.Perm(3)
	mask := byte(rand.Uint32())

	// Records carry masked destination, ciphertext and key positions, followed
	// by a permuted opcode. uint16 positions cover the per-byte decoder limit.
	const recordSize = 7
	var records [][]byte
	addRecord := func(dst, x, y, opcode int) {
		record := make([]byte, recordSize)
		for i, value := range []int{dst, x, y} {
			value ^= int(mask)
			off := 2 * fields[i]
			record[off], record[off+1] = byte(value), byte(value>>8)
		}
		record[6] = byte(opcode)
		records = append(records, record)
	}
	for i, b := range data {
		op := rand.Intn(len(operators))
		fullData[positions[i]] = evalOperator(operators[op], b, key[i])
		fullData[positions[len(data)+i]] = key[i]
		addRecord(i, positions[i], positions[len(data)+i], opcodes[op])
	}
	// Junk records execute the same indirect operations without writing output.
	for range 1 + rand.Intn(4) {
		addRecord(len(data), rand.Intn(len(fullData)), rand.Intn(len(fullData)), rand.Intn(len(operators)))
	}
	rand.Shuffle(len(records), func(i, j int) { records[i], records[j] = records[j], records[i] })
	stream := make([]byte, 0, recordSize*len(records))
	for _, record := range records {
		stream = append(stream, record...)
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
	readPosition := func(field int) ast.Expr {
		at := func(offset int) ast.Expr {
			return ah.CallExprByName("int", ah.IndexExpr(names.name("operations"), ah.BinaryExpr(names.ident("i"), token.ADD, ah.IntLit(offset))))
		}
		off := 2 * fields[field]
		return ah.BinaryExpr(ah.BinaryExpr(at(off), token.OR, ah.BinaryExpr(at(off+1), token.SHL, ah.IntLit(8))), token.XOR, ah.CallExprByName("int", names.ident("mask")))
	}
	return ah.BlockStmt(
		ah.AssignDefineStmt(names.ident("fullData"), dataToByteSliceWithExtKeys(rand, names, fullData, extKeys)),
		ah.AssignDefineStmt(names.ident("operations"), dataToByteSliceWithExtKeys(rand, names, stream, extKeys)),
		ah.AssignDefineStmt(names.ident("mask"), ah.CallExprByName("byte", byteLitWithExtKey(rand, mask, extKeys, 1))),
		ah.AssignDefineStmt(names.ident("handlers"), handlers),
		ah.AssignDefineStmt(names.ident("data"), ah.CallExprByName("make", ah.ByteSliceType(), ah.IntLit(len(data)))),
		&ast.ForStmt{
			Init: ah.AssignDefineStmt(names.ident("i"), ah.IntLit(0)),
			Cond: ah.BinaryExpr(names.ident("i"), token.LSS, ah.IntLit(len(stream))),
			Post: &ast.AssignStmt{Lhs: []ast.Expr{names.ident("i")}, Tok: token.ADD_ASSIGN, Rhs: []ast.Expr{ah.IntLit(recordSize)}},
			Body: ah.BlockStmt(
				ah.AssignDefineStmt(names.ident("dst"), readPosition(0)),
				ah.AssignDefineStmt(names.ident("value"), ah.CallExpr(
					ah.IndexExpr(names.name("handlers"), ah.IndexExpr(names.name("operations"), ah.BinaryExpr(names.ident("i"), token.ADD, ah.IntLit(6)))),
					ah.IndexExpr(names.name("fullData"), readPosition(1)),
					ah.IndexExpr(names.name("fullData"), readPosition(2)),
				)),
				&ast.IfStmt{
					Cond: ah.BinaryExpr(names.ident("dst"), token.LSS, ah.IntLit(len(data))),
					Body: ah.BlockStmt(ah.AssignStmt(ah.IndexExpr(names.name("data"), names.ident("dst")), names.ident("value"))),
				},
			),
		},
	)
}
