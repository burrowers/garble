package literals

import (
	"go/ast"
	"go/token"
	"strconv"
)

type swap struct{}

// check that the obfuscator interface is implemented
var _ obfuscator = swap{}

// Source: https://golang.org/ref/spec#Numeric_types
const maxUInt8 = 255
const maxUint16 = 65535

func getIndexType(dataLen int) string {
	switch {
	case dataLen <= maxUInt8:
		return "byte"
	case dataLen <= maxUint16:
		return "uint16"
	default:
		return "uint32"
	}
}

func indexesToSlice(data []int) *ast.CompositeLit {
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
	shiftKey := byte(genRandIntn(maxUInt8))

	indexes := generateIntSlice(len(data), count)
	for i := len(indexes) - 2; i >= 0; i -= 2 {
		localKey := byte(i) + byte(indexes[i]^indexes[i+1]) + shiftKey
		data[indexes[i]], data[indexes[i+1]] = data[indexes[i+1]]^localKey, data[indexes[i]]^localKey
	}

	return &ast.BlockStmt{List: []ast.Stmt{
		&ast.AssignStmt{
			Lhs: []ast.Expr{ident("data")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{dataToByteSlice(data)},
		},
		&ast.AssignStmt{
			Lhs: []ast.Expr{
				ident("indexes"),
			},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{
				indexesToSlice(indexes),
			},
		},
		&ast.ForStmt{
			Init: &ast.AssignStmt{
				Lhs: []ast.Expr{
					ident("i"),
				},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{
					intLiteral("0"),
				},
			},
			Cond: &ast.BinaryExpr{
				X:  ident("i"),
				Op: token.LSS,
				Y:  intLiteral(strconv.Itoa(len(indexes))),
			},
			Post: &ast.AssignStmt{
				Lhs: []ast.Expr{
					ident("i"),
				},
				Tok: token.ADD_ASSIGN,
				Rhs: []ast.Expr{
					intLiteral("2"),
				},
			},
			Body: &ast.BlockStmt{
				List: []ast.Stmt{
					&ast.AssignStmt{
						Lhs: []ast.Expr{
							ident("localKey"),
						},
						Tok: token.DEFINE,
						Rhs: []ast.Expr{
							&ast.BinaryExpr{
								X: &ast.BinaryExpr{
									X: &ast.CallExpr{
										Fun: ident("byte"),
										Args: []ast.Expr{
											ident("i"),
										},
									},
									Op: token.ADD,
									Y: &ast.CallExpr{
										Fun: ident("byte"),
										Args: []ast.Expr{
											&ast.BinaryExpr{
												X: &ast.IndexExpr{
													X:     ident("indexes"),
													Index: ident("i"),
												},
												Op: token.XOR,
												Y: &ast.IndexExpr{
													X: ident("indexes"),
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
							indexExpr("data", indexExpr("indexes", ident("i"))),
							indexExpr("data", indexExpr("indexes", &ast.BinaryExpr{
								X:  ident("i"),
								Op: token.ADD,
								Y:  intLiteral("1"),
							})),
						},
						Tok: token.ASSIGN,
						Rhs: []ast.Expr{
							&ast.BinaryExpr{
								X: indexExpr("data", indexExpr("indexes", &ast.BinaryExpr{
									X:  ident("i"),
									Op: token.ADD,
									Y:  intLiteral("1"),
								})),
								Op: token.XOR,
								Y:  ident("localKey"),
							},
							&ast.BinaryExpr{
								X:  indexExpr("data", indexExpr("indexes", ident("i"))),
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
