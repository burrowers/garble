package literals

import (
	cryptrand "crypto/rand"
	"fmt"
	"go/ast"
	"go/token"
	mathrand "math/rand"
	"os"
	"strings"
)

// obfuscator takes a byte slice and converts it to a ast.BlockStmt
type obfuscator interface {
	obfuscate(data []byte) *ast.BlockStmt
}

var (
	// obfuscators contains all types which implement the obfuscator Interface
	obfuscators = []obfuscator{
		xor{},
		swap{},
		split{},
		xorShuffle{},
		xorSeed{},
	}
	envGarbleSeed = os.Getenv("GARBLE_SEED")
)

// If math/rand.Seed() is not called, the generator behaves as if seeded by rand.Seed(1),
// so the generator is deterministic.

// genRandBytes return a random []byte with the length of size.
func genRandBytes(buffer []byte) {
	if strings.HasPrefix(envGarbleSeed, "random;") {
		_, err := cryptrand.Read(buffer)
		if err != nil {
			panic(fmt.Sprintf("couldn't generate random key:  %v", err))
		}
	} else {
		_, err := mathrand.Read(buffer)
		if err != nil {
			panic(fmt.Sprintf("couldn't generate random key:  %v", err))
		}
	}
}

func genRandByte() byte {
	bytes := make([]byte, 1)
	genRandBytes(bytes)
	return bytes[0]
}

func genRandIntSlice(max, count int) []int {
	indexes := make([]int, count)
	for i := 0; i < count; i++ {
		indexes[i] = mathrand.Intn(max)
	}
	return indexes
}

func randOperator() token.Token {
	operatorTokens := [...]token.Token{token.XOR, token.ADD, token.SUB}
	return operatorTokens[mathrand.Intn(len(operatorTokens))]
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
