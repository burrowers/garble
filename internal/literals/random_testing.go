//go:build garble_testing

package literals

import (
	"go/ast"
	mathrand "math/rand"
	"os"
	"strings"

	"golang.org/x/exp/slices"
)

var (
	TestObfuscator         string
	packageToObfuscatorMap map[string]obfuscator
)

func init() {
	obfMapEnv := os.Getenv("GARBLE_TEST_LITERALS_OBFUSCATOR_MAP")
	if obfMapEnv == "" {
		panic("literals obfuscator map required for testing build")
	}
	packageToObfuscatorMap = make(map[string]obfuscator)
	// Parse obfuscator map: packageName1=obfName1,packageName2=obfName2
	pairs := strings.Split(obfMapEnv, ",")
	for _, pair := range pairs {
		keyValue := strings.SplitN(pair, "=", 2)

		obfName := keyValue[1]
		obfIdx := slices.Index(ObfuscatorNames, obfName)
		if obfIdx < 0 {
			panic("unknown obfuscator " + obfName)
		}
		packageToObfuscatorMap[keyValue[0]] = obfuscators[obfIdx]
	}
}

type obfRand struct {
	*mathrand.Rand
	obf obfuscator
}

func (r *obfRand) nextObfuscator() obfuscator {
	if r.obf == nil {
		return obfuscators[r.Intn(len(obfuscators))]
	}
	return r.obf
}

func NewObfuscatorRandom(rand *mathrand.Rand, f *ast.File) *obfRand {
	obf, _ := packageToObfuscatorMap[f.Name.Name]
	return &obfRand{rand, obf}
}
