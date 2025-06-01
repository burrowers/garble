// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"fmt"
	"go/ast"
	"go/token"
	"math"
	mathrand "math/rand"
	"slices"
	"strconv"

	ah "mvdan.cc/garble/internal/asthelper"
)

// extKeyRarity probability of using an external key.
// Larger value, greater probability of using an external key.
// Must be between 0 and 1
type extKeyRarity float32

const (
	rareRarity   extKeyRarity = 0.4
	normalRarity extKeyRarity = 0.6
	commonRarity extKeyRarity = 0.8
)

func (r extKeyRarity) Try(rand *mathrand.Rand) bool {
	return rand.Float32() < float32(r)
}

// extKey contains all information about the external key
type extKey struct {
	name, typ string
	value     uint64
	bits      int
	refs      int
}

func (k *extKey) Type() *ast.Ident {
	return ast.NewIdent(k.typ)
}

func (k *extKey) Name() *ast.Ident {
	return ast.NewIdent(k.name)
}

func (k *extKey) AddRef() {
	k.refs++
}

func (k *extKey) IsUsed() bool {
	return k.refs > 0
}

// obfuscator takes a byte slice and converts it to a ast.BlockStmt
type obfuscator interface {
	obfuscate(obfRand *mathrand.Rand, data []byte, extKeys []*extKey) *ast.BlockStmt
}

var (
	simpleObfuscator = simple{}

	// Obfuscators contains all types which implement the obfuscator Interface
	Obfuscators = []obfuscator{
		simpleObfuscator,
		swap{},
		split{},
		shuffle{},
		seed{},
	}

	// LinearTimeObfuscators contains all types which implement the obfuscator Interface and can safely be used on large literals
	LinearTimeObfuscators = []obfuscator{
		simpleObfuscator,
	}

	TestObfuscator         string
	testPkgToObfuscatorMap map[string]obfuscator
)

func genRandIntSlice(obfRand *mathrand.Rand, max, count int) []int {
	indexes := make([]int, count)
	for i := range count {
		indexes[i] = obfRand.Intn(max)
	}
	return indexes
}

func randOperator(obfRand *mathrand.Rand) token.Token {
	operatorTokens := [...]token.Token{token.XOR, token.ADD, token.SUB}
	return operatorTokens[obfRand.Intn(len(operatorTokens))]
}

func evalOperator(t token.Token, x, y byte) byte {
	switch t {
	case token.XOR:
		return x ^ y
	case token.ADD:
		return x + y
	case token.SUB:
		return x - y
	default:
		panic(fmt.Sprintf("unknown operator: %s", t))
	}
}

func operatorToReversedBinaryExpr(t token.Token, x, y ast.Expr) *ast.BinaryExpr {
	expr := &ast.BinaryExpr{X: x, Y: y}

	switch t {
	case token.XOR:
		expr.Op = token.XOR
	case token.ADD:
		expr.Op = token.SUB
	case token.SUB:
		expr.Op = token.ADD
	default:
		panic(fmt.Sprintf("unknown operator: %s", t))
	}

	return expr
}

const (
	// minExtKeyCount is minimum number of external keys for one lambda call
	minExtKeyCount = 2
	// maxExtKeyCount is maximum number of external keys for one lambda call
	maxExtKeyCount = 6

	// minByteSliceExtKeyOps minimum number of operations with external keys for one byte slice
	minByteSliceExtKeyOps = 2
	// maxByteSliceExtKeyOps maximum number of operations with external keys for one byte slice
	maxByteSliceExtKeyOps = 12
)

// extKeyRanges contains a list of different ranges of random numbers for external keys
// Different types and bitnesses will increase the chance of changing patterns
var extKeyRanges = []struct {
	typ  string
	max  uint64
	bits int
}{
	{"uint8", math.MaxUint8, 8},
	{"uint16", math.MaxUint16, 16},
	{"uint32", math.MaxUint32, 32},
	{"uint64", math.MaxUint64, 64},
}

// randExtKey generates a random external key with a unique name, type, value, and bitnesses
func randExtKey(obfRand *mathrand.Rand, idx int) *extKey {
	r := extKeyRanges[obfRand.Intn(len(extKeyRanges))]
	return &extKey{
		name:  "garbleExternalKey" + strconv.Itoa(idx),
		typ:   r.typ,
		value: obfRand.Uint64() & r.max,
		bits:  r.bits,
	}
}

func randExtKeys(obfRand *mathrand.Rand) []*extKey {
	count := minExtKeyCount + obfRand.Intn(maxExtKeyCount-minExtKeyCount)
	keys := make([]*extKey, count)
	for i := 0; i < count; i++ {
		keys[i] = randExtKey(obfRand, i)
	}
	return keys
}

// extKeysToParams converts a list of extKeys into a parameter list and argument expressions for function calls.
// It ensures unused keys have placeholder names and sometimes use proxyDispatcher.HideValue for key values
func extKeysToParams(objRand *obfRand, keys []*extKey) (params *ast.FieldList, args []ast.Expr) {
	params = &ast.FieldList{}
	for _, key := range keys {
		name := key.Name()
		if !key.IsUsed() {
			name.Name = "_"
		}
		params.List = append(params.List, &ast.Field{
			Names: []*ast.Ident{name},
			Type:  key.Type(),
		})

		var extKeyExpr ast.Expr = &ast.BasicLit{
			Kind:  token.INT,
			Value: fmt.Sprint(key.value),
		}
		if rareRarity.Try(objRand.Rand) {
			extKeyExpr = objRand.proxyDispatcher.HideValue(extKeyExpr, ast.NewIdent(key.typ))
		}
		args = append(args, extKeyExpr)
	}
	return
}

// extKeyToExpr converts an external key into an AST expression like:
//
// uint8(key >> b)
func extKeyToExpr(key *extKey, b int) ast.Expr {
	var x ast.Expr = key.Name()
	if b > 0 {
		x = &ast.BinaryExpr{
			X:  x,
			Op: token.SHR,
			Y:  ah.IntLit(b * 8),
		}
	}
	if key.typ != "uint8" {
		x = ah.CallExprByName("byte", x)
	}
	return x
}

// dataToByteSliceWithExtKeys scramble and turn a byte slice into an AST expression like:
//
//	func() []byte {
//		data := []byte("<data>")
//		data[<index>] = data[<index>] <random operator> byte(<external key> >> <random shift>) // repeated random times
//		return data
//	}()
func dataToByteSliceWithExtKeys(obfRand *mathrand.Rand, data []byte, extKeys []*extKey) ast.Expr {
	extKeyOpCount := minByteSliceExtKeyOps + obfRand.Intn(maxByteSliceExtKeyOps-minByteSliceExtKeyOps)

	var stmts []ast.Stmt
	for i := 0; i < extKeyOpCount; i++ {
		key := extKeys[obfRand.Intn(len(extKeys))]
		key.AddRef()

		idx, op, b := obfRand.Intn(len(data)), randOperator(obfRand), obfRand.Intn(key.bits/8)
		data[idx] = evalOperator(op, data[idx], byte(key.value>>(b*8)))
		stmts = append(stmts, &ast.AssignStmt{
			Lhs: []ast.Expr{ah.IndexExpr("data", ah.IntLit(idx))},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{
				operatorToReversedBinaryExpr(op,
					ah.IndexExpr("data", ah.IntLit(idx)),
					extKeyToExpr(key, b),
				),
			},
		})
	}

	// External keys can be applied several times to the same array element,
	// and it is important to invert the order of execution to correctly restore the original value
	slices.Reverse(stmts)

	stmts = append([]ast.Stmt{ah.AssignDefineStmt(ast.NewIdent("data"), ah.DataToByteSlice(data))}, append(stmts, ah.ReturnStmt(ast.NewIdent("data")))...)
	return ah.LambdaCall(nil, &ast.ArrayType{Elt: ast.NewIdent("byte")}, ah.BlockStmt(stmts...), nil)
}

// dataToByteSliceWithExtKeys scramble and turns a byte into an AST expression like:
//
//	byte(<obfuscated value>) <random operator> byte(<external key> >> <random shift>)
func byteLitWithExtKey(obfRand *mathrand.Rand, val byte, extKeys []*extKey, rarity extKeyRarity) ast.Expr {
	if !rarity.Try(obfRand) {
		return ah.IntLit(int(val))
	}

	key := extKeys[obfRand.Intn(len(extKeys))]
	key.AddRef()

	op, b := randOperator(obfRand), obfRand.Intn(key.bits/8)
	newVal := evalOperator(op, val, byte(key.value>>(b*8)))

	return operatorToReversedBinaryExpr(op,
		ah.CallExprByName("byte", ah.IntLit(int(newVal))),
		extKeyToExpr(key, b),
	)
}

type obfRand struct {
	*mathrand.Rand
	testObfuscator obfuscator

	proxyDispatcher *proxyDispatcher
}

func (r *obfRand) nextObfuscator() obfuscator {
	if r.testObfuscator != nil {
		return r.testObfuscator
	}
	return Obfuscators[r.Intn(len(Obfuscators))]
}

func (r *obfRand) nextLinearTimeObfuscator() obfuscator {
	if r.testObfuscator != nil {
		return r.testObfuscator
	}
	return Obfuscators[r.Intn(len(LinearTimeObfuscators))]
}

func newObfRand(rand *mathrand.Rand, file *ast.File, nameFunc NameProviderFunc) *obfRand {
	testObf := testPkgToObfuscatorMap[file.Name.Name]
	return &obfRand{rand, testObf, newProxyDispatcher(rand, nameFunc)}
}
