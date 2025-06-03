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

// Shuffles the passed array and returns it back.
// Applies for inline declaration of randomly shuffled statement arrays
func shuffleStmts(obfRand *mathrand.Rand, stmts ...ast.Stmt) []ast.Stmt {
	obfRand.Shuffle(len(stmts), func(i, j int) {
		stmts[i], stmts[j] = stmts[j], stmts[i]
	})
	return stmts
}

// Encrypt chunks based on key and position
func encryptChunks(chunks [][]byte, op token.Token, key byte) {
	byteOffset := 0
	for _, chunk := range chunks {
		for i, b := range chunk {
			chunk[i] = evalOperator(op, b, key^byte(byteOffset))
			byteOffset++
		}
	}
}

func (split) obfuscate(rand *mathrand.Rand, data []byte, extKeys []*externalKey) *ast.BlockStmt {
	var chunks [][]byte
	// Short arrays should be divided into single-byte fragments
	if len(data)/maxChunkSize < minCaseCount {
		chunks = splitIntoOneByteChunks(data)
	} else {
		chunks = splitIntoRandomChunks(rand, data)
	}

	// Generate indexes for cases chunk count + 1 decrypt case + 1 exit case
	indexes := rand.Perm(len(chunks) + 2)

	decryptKeyInitial := byte(rand.Uint32())
	decryptKey := decryptKeyInitial
	// Calculate decrypt key based on indexes and position. Ignore exit index
	for i, index := range indexes[:len(indexes)-1] {
		decryptKey ^= byte(index * i)
	}

	op := randOperator(rand)
	encryptChunks(chunks, op, decryptKey)

	decryptIndex := indexes[len(indexes)-2]
	exitIndex := indexes[len(indexes)-1]
	switchCases := []ast.Stmt{&ast.CaseClause{
		List: []ast.Expr{ah.IntLit(decryptIndex)},
		Body: shuffleStmts(rand,
			&ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent("i")},
				Tok: token.ASSIGN,
				Rhs: []ast.Expr{ah.IntLit(exitIndex)},
			},
			&ast.RangeStmt{
				Key: ast.NewIdent("y"),
				Tok: token.DEFINE,
				X:   ast.NewIdent("data"),
				Body: ah.BlockStmt(&ast.AssignStmt{
					Lhs: []ast.Expr{ah.IndexExpr("data", ast.NewIdent("y"))},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{
						operatorToReversedBinaryExpr(
							op,
							ah.IndexExpr("data", ast.NewIdent("y")),
							ah.CallExpr(ast.NewIdent("byte"), &ast.BinaryExpr{
								X:  ast.NewIdent("decryptKey"),
								Op: token.XOR,
								Y:  ast.NewIdent("y"),
							}),
						),
					},
				}),
			},
		),
	}}
	for i := range chunks {
		index := indexes[i]
		nextIndex := indexes[i+1]
		chunk := chunks[i]

		appendCallExpr := &ast.CallExpr{
			Fun:  ast.NewIdent("append"),
			Args: []ast.Expr{ast.NewIdent("data")},
		}

		if len(chunk) != 1 {
			appendCallExpr.Args = append(appendCallExpr.Args, dataToByteSliceWithExtKeys(rand, chunk, extKeys))
			appendCallExpr.Ellipsis = 1
		} else {
			appendCallExpr.Args = append(appendCallExpr.Args, byteLitWithExtKey(rand, chunk[0], extKeys, lowProb))
		}

		switchCases = append(switchCases, &ast.CaseClause{
			List: []ast.Expr{ah.IntLit(index)},
			Body: shuffleStmts(rand,
				&ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent("i")},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{ah.IntLit(nextIndex)},
				},
				&ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent("data")},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{appendCallExpr},
				},
			),
		})
	}

	return ah.BlockStmt(
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("data")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.CallExpr(ast.NewIdent("make"), &ast.ArrayType{Elt: ast.NewIdent("byte")}, ah.IntLit(0), ah.IntLit(len(data)+1))},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("i")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.IntLit(indexes[0])},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("decryptKey")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.CallExprByName("int", byteLitWithExtKey(rand, decryptKeyInitial, extKeys, normalProb))},
		},
		&ast.ForStmt{
			Init: &ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent("counter")},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{ah.IntLit(0)},
			},
			Cond: &ast.BinaryExpr{
				X:  ast.NewIdent("i"),
				Op: token.NEQ,
				Y:  ah.IntLit(indexes[len(indexes)-1]),
			},
			Post: &ast.IncDecStmt{
				X:   ast.NewIdent("counter"),
				Tok: token.INC,
			},
			Body: ah.BlockStmt(
				&ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent("decryptKey")},
					Tok: token.XOR_ASSIGN,
					Rhs: []ast.Expr{
						&ast.BinaryExpr{
							X:  ast.NewIdent("i"),
							Op: token.MUL,
							Y:  ast.NewIdent("counter"),
						},
					},
				},
				&ast.SwitchStmt{
					Tag:  ast.NewIdent("i"),
					Body: ah.BlockStmt(shuffleStmts(rand, switchCases...)...),
				}),
		},
	)
}
