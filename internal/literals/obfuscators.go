// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	mathrand "math/rand"
	"strings"
)

// obfuscator takes a byte slice and converts it to a ast.BlockStmt
type obfuscator interface {
	obfuscate(obfRand *mathrand.Rand, data []byte) *ast.BlockStmt
}

var (
	simpleObfuscator = simple{}

	obfuscatorRegister map[string]obfuscator

	// Obfuscators contains all types which implement the obfuscator Interface
	Obfuscators []obfuscator

	// LinearTimeObfuscators contains all types which implement the obfuscator Interface and can safely be used on large literals
	LinearTimeObfuscators = []obfuscator{
		simpleObfuscator,
	}

	TestObfuscator         string
	testPkgToObfuscatorMap map[string]obfuscator
)

func init() {
	obfuscatorRegister = make(map[string]obfuscator)
	obfuscatorRegister["simple"] = simpleObfuscator
	obfuscatorRegister["swap"] = swap{}
	obfuscatorRegister["split"] = split{}
	obfuscatorRegister["shuffle"] = shuffle{}
	obfuscatorRegister["seed"] = seed{}
}

func GetRegisteredObfuscators() (names []string) {
	names = make([]string, 0, len(obfuscatorRegister))
	for name, _ := range obfuscatorRegister {
		names = append(names, name)
	}
	return
}

func SetObfuscators(names []string) {
	if names == nil || len(names) <= 0 {
		panic(errors.New("no obfuscator names provided"))
	}
	Obfuscators = make([]obfuscator, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name)) // always check for lower-case names
		obf, ok := obfuscatorRegister[name]
		if !ok {
			panic(errors.New(fmt.Sprintf("obfuscator [%s] not found! ", name)))
		}
		Obfuscators = append(Obfuscators, obf)
	}
}

func genRandIntSlice(obfRand *mathrand.Rand, max, count int) []int {
	indexes := make([]int, count)
	for i := 0; i < count; i++ {
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

type obfRand struct {
	*mathrand.Rand
	testObfuscator obfuscator
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

func newObfRand(rand *mathrand.Rand, file *ast.File) *obfRand {
	testObf := testPkgToObfuscatorMap[file.Name.Name]
	return &obfRand{rand, testObf}
}
