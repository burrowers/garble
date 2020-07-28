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
	}
	envGarbleSeed = os.Getenv("GARBLE_SEED")
)

// dataToByteSlice turns a byte slice like []byte{1, 2, 3} into an AST
// expression
func dataToByteSlice(data []byte) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.ArrayType{
			Elt: &ast.Ident{Name: "byte"},
		},
		Args: []ast.Expr{&ast.BasicLit{
			Kind:  token.STRING,
			Value: fmt.Sprintf("%q", data),
		}},
	}
}

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

func genRandIntSlice(max, count int) []int {
	indexes := make([]int, count)
	for i := 0; i < count; i++ {
		indexes[i] = mathrand.Intn(max)
	}
	return indexes
}
