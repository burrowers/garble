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

// Generates a random even swap count based on the length of data
func generateSwapCount(dataLen int) int {
	maxExtraPositions := dataLen / 2
	if maxExtraPositions == 0 {
		maxExtraPositions = 1
	}
	swapCount := dataLen + genRandIntn(maxExtraPositions)
	if swapCount%2 != 0 {
		swapCount++
	}
	return swapCount
}

func (x swap) obfuscate(data []byte) *ast.BlockStmt {
	swapCount := generateSwapCount(len(data))
	shiftKey := byte(genRandIntn(math.MaxUint8))

	positions := generateIntSlice(len(data), swapCount)
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
									X:  callExpr(ident("byte"), ident("i")),
									Op: token.ADD,
									Y: callExpr(ident("byte"), &ast.BinaryExpr{
										X:  indexExpr("positions", ident("i")),
										Op: token.XOR,
										Y: indexExpr("positions", &ast.BinaryExpr{
											X:  ident("i"),
											Op: token.ADD,
											Y:  intLiteral("1"),
										}),
									}),
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
