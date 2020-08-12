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

func splitIntoRandomChunks(data []byte) [][]byte {
	if len(data) == 1 {
		return [][]byte{data}
	}

	var chunks [][]byte
	for len(data) > 0 {
		chunkSize := 1 + mathrand.Intn(maxChunkSize)
		if chunkSize > len(data) {
			chunkSize = len(data)
		}

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
func shuffleStmts(stmts ...ast.Stmt) []ast.Stmt {
	mathrand.Shuffle(len(stmts), func(i, j int) {
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

func (x split) obfuscate(data []byte) *ast.BlockStmt {
	var chunks [][]byte
	// Short arrays should be divided into single-byte fragments
	if len(data)/maxChunkSize < minCaseCount {
		chunks = splitIntoOneByteChunks(data)
	} else {
		chunks = splitIntoRandomChunks(data)
	}

	// Generate indexes for cases chunk count + 1 decrypt case + 1 exit case
	indexes := mathrand.Perm(len(chunks) + 2)

	decryptKeyInitial := genRandByte()
	decryptKey := decryptKeyInitial
	// Calculate decrypt key based on indexes and position. Ignore exit index
	for i, index := range indexes[:len(indexes)-1] {
		decryptKey ^= byte(index * i)
	}

	op := randOperator()
	encryptChunks(chunks, op, decryptKey)

	decryptIndex := indexes[len(indexes)-2]
	exitIndex := indexes[len(indexes)-1]
	switchCases := []ast.Stmt{&ast.CaseClause{
		List: []ast.Expr{ah.IntLit(decryptIndex)},
		Body: shuffleStmts(
			&ast.AssignStmt{
				Lhs: []ast.Expr{ah.Ident("i")},
				Tok: token.ASSIGN,
				Rhs: []ast.Expr{ah.IntLit(exitIndex)},
			},
			&ast.RangeStmt{
				Key: ah.Ident("y"),
				Tok: token.DEFINE,
				X:   ah.Ident("data"),
				Body: ah.BlockStmt(&ast.AssignStmt{
					Lhs: []ast.Expr{ah.IndexExpr("data", ah.Ident("y"))},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{
						operatorToReversedBinaryExpr(
							op,
							ah.IndexExpr("data", ah.Ident("y")),
							ah.CallExpr(ah.Ident("byte"), &ast.BinaryExpr{
								X:  ah.Ident("decryptKey"),
								Op: token.XOR,
								Y:  ah.Ident("y"),
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
			Fun:  ah.Ident("append"),
			Args: []ast.Expr{ah.Ident("data")},
		}

		if len(chunk) != 1 {
			appendCallExpr.Args = append(appendCallExpr.Args, ah.StringLit(string(chunk)))
			appendCallExpr.Ellipsis = 1
		} else {
			appendCallExpr.Args = append(appendCallExpr.Args, ah.IntLit(int(chunk[0])))
		}

		switchCases = append(switchCases, &ast.CaseClause{
			List: []ast.Expr{ah.IntLit(index)},
			Body: shuffleStmts(
				&ast.AssignStmt{
					Lhs: []ast.Expr{ah.Ident("i")},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{ah.IntLit(nextIndex)},
				},
				&ast.AssignStmt{
					Lhs: []ast.Expr{ah.Ident("data")},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{appendCallExpr},
				},
			),
		})
	}

	return ah.BlockStmt(
		&ast.DeclStmt{Decl: &ast.GenDecl{
			Tok: token.VAR,
			Specs: []ast.Spec{
				&ast.ValueSpec{
					Names: []*ast.Ident{ah.Ident("data")},
					Type:  &ast.ArrayType{Elt: ah.Ident("byte")},
				},
			},
		}},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("i")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.IntLit(indexes[0])},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("decryptKey")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{ah.IntLit(int(decryptKeyInitial))},
		},
		&ast.ForStmt{
			Init: &ast.AssignStmt{
				Lhs: []ast.Expr{ah.Ident("counter")},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{ah.IntLit(0)},
			},
			Cond: &ast.BinaryExpr{
				X:  ah.Ident("i"),
				Op: token.NEQ,
				Y:  ah.IntLit(indexes[len(indexes)-1]),
			},
			Post: &ast.IncDecStmt{
				X:   ah.Ident("counter"),
				Tok: token.INC,
			},
			Body: ah.BlockStmt(
				&ast.AssignStmt{
					Lhs: []ast.Expr{ah.Ident("decryptKey")},
					Tok: token.XOR_ASSIGN,
					Rhs: []ast.Expr{
						&ast.BinaryExpr{
							X:  ah.Ident("i"),
							Op: token.MUL,
							Y:  ah.Ident("counter"),
						},
					},
				},
				&ast.SwitchStmt{
					Tag:  ah.Ident("i"),
					Body: ah.BlockStmt(shuffleStmts(switchCases...)...),
				}),
		})
}
