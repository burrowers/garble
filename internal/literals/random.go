//go:build !garble_testing

package literals

import (
	"go/ast"
	mathrand "math/rand"
)

const TestObfuscator = ""

type obfRand struct {
	*mathrand.Rand
}

func (r *obfRand) nextObfuscator() obfuscator {
	return obfuscators[r.Intn(len(obfuscators))]
}

func NewObfuscatorRandom(rand *mathrand.Rand, _ *ast.File) *obfRand {
	return &obfRand{rand}
}
