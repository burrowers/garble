//go:build garble_testing

package literals

import (
	"os"
	"strconv"
	"strings"
)

func init() {
	obfMapEnv := os.Getenv("GARBLE_TEST_LITERALS_OBFUSCATOR_MAP")
	if obfMapEnv == "" {
		panic("literals obfuscator map required for testing build")
	}
	testPkgToObfuscatorMap = make(map[string]obfuscator)

	// Parse obfuscator mapping: pkgName1=obfIndex1,pkgName2=obfIndex2
	pairs := strings.Split(obfMapEnv, ",")
	for _, pair := range pairs {
		keyValue := strings.SplitN(pair, "=", 2)

		pkgName := keyValue[0]
		obfIndex, err := strconv.Atoi(keyValue[1])
		if err != nil {
			panic(err)
		}
		testPkgToObfuscatorMap[pkgName] = Obfuscators[obfIndex]
	}
	TestObfuscator = obfMapEnv
}
