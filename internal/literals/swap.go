package literals

import (
	"go/ast"
	"go/token"
	"math"
	"strconv"
)

type swap struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = swap{}

func getIndexType(dataLen int) string {
	switch {
	case dataLen <= math.MaxUint8:
		return "byte"
	case dataLen <= math.MaxUint16:
		return "uint16"
	default:
		return "uint32"
	}
}

func positionsToSlice(data []int) *ast.CompositeLit {
	arr := &ast.CompositeLit{
		Type: &ast.ArrayType{
			Len: &ast.Ellipsis{}, // Performance optimization
			Elt: ident(getIndexType(len(data))),
		},
		Elts: []ast.Expr{},
	}
	for _, data := range data {
		arr.Elts = append(arr.Elts, intLiteral(strconv.Itoa(data)))
	}
	return arr
}

func (x swap) obfuscate(data []byte) *ast.BlockStmt {
	maxJunkIdxCount := len(data) / 2
	if maxJunkIdxCount == 0 {
		maxJunkIdxCount = 1
	}
	count := len(data) + genRandIntn(maxJunkIdxCount)
	if count%2 != 0 {
		count++
	}
	shiftKey := byte(genRandIntn(math.MaxUint8))

	positions := generateIntSlice(len(data), count)
	for i := len(positions) - 2; i >= 0; i -= 2 {
		localKey := byte(i) + byte(positions[i]^positions[i+1]) + shiftKey
		data[positions[i]], data[positions[i+1]] = data[positions[i+1]]^localKey, data[positions[i]]^localKey
	}

	return &ast.BlockStmt{List: []ast.Stmt{
		&ast.AssignStmt{
			Lhs: []ast.Expr{ident("data")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSlice(data)},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{ident("positions")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{positionsToSlice(positions)},
		},
		&ast.ForStmt{
			Init: &ast.AssignStmt{
				Lhs: []ast.Expr{ident("i")},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{intLiteral("0")},
			},
			Cond: &ast.BinaryExpr{
				X:  ident("i"),
				Op: token.LSS,
				Y:  intLiteral(strconv.Itoa(len(positions))),
			},
			Post: &ast.AssignStmt{
				Lhs: []ast.Expr{ident("i")},
				Tok: token.ADD_ASSIGN,
				Rhs: []ast.Expr{intLiteral("2")},
			},
			Body: &ast.BlockStmt{
				List: []ast.Stmt{
					&ast.AssignStmt{
						Lhs: []ast.Expr{ident("localKey")},
						Tok: token.DEFINE,
						Rhs: []ast.Expr{
							&ast.BinaryExpr{
								X: &ast.BinaryExpr{
									X: &ast.CallExpr{
										Fun:  ident("byte"),
										Args: []ast.Expr{ident("i")},
									},
									Op: token.ADD,
									Y: &ast.CallExpr{
										Fun: ident("byte"),
										Args: []ast.Expr{
											&ast.BinaryExpr{
												X: &ast.IndexExpr{
													X:     ident("positions"),
													Index: ident("i"),
												},
												Op: token.XOR,
												Y: &ast.IndexExpr{
													X: ident("positions"),
													Index: &ast.BinaryExpr{
														X:  ident("i"),
														Op: token.ADD,
														Y:  intLiteral("1"),
													},
												},
											},
										},
									},
								},
								Op: token.ADD,
								Y:  intLiteral(strconv.Itoa(int(shiftKey))),
							},
						},
					},
					&ast.AssignStmt{
						Lhs: []ast.Expr{
							indexExpr("data", indexExpr("positions", ident("i"))),
							indexExpr("data", indexExpr("positions", &ast.BinaryExpr{
								X:  ident("i"),
								Op: token.ADD,
								Y:  intLiteral("1"),
							})),
						},
						Tok: token.ASSIGN,
						Rhs: []ast.Expr{
							&ast.BinaryExpr{
								X: indexExpr("data", indexExpr("positions", &ast.BinaryExpr{
									X:  ident("i"),
									Op: token.ADD,
									Y:  intLiteral("1"),
								})),
								Op: token.XOR,
								Y:  ident("localKey"),
							},
							&ast.BinaryExpr{
								X:  indexExpr("data", indexExpr("positions", ident("i"))),
								Op: token.XOR,
								Y:  ident("localKey"),
							},
						},
					},
				},
			},
		},
	}}
}
