package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

func abiNamePatch(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	find := `return unsafe.String(n.DataChecked(1+i, "non-empty string"), l)`
	replace := `return _originalNames(unsafe.String(n.DataChecked(1+i, "non-empty string"), l))`

	str := strings.Replace(string(data), find, replace, 1)

	originalNames := `
//go:linkname _originalNames
func _originalNames(name string) string

//go:linkname _originalNamesInit
func _originalNamesInit()

func init() { _originalNamesInit() }
`

	return str + originalNames, nil
}

var reflectPatchFile = ""

// reflectMainPrePatch adds the initial empty name mapping and _originalNames implementation
// to a file in the main package. The name mapping will be populated later after
// analyzing the main package, since we need to know all obfuscated names that need mapping.
// We split this into pre/post steps so that all variable names in the generated code
// can be properly obfuscated - if we added the filled map directly, the obfuscated names
// would appear as plain strings in the binary.
func reflectMainPrePatch(path string) ([]byte, error) {
	if reflectPatchFile != "" {
		// already patched another file in main
		return nil, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	_, code, _ := strings.Cut(reflectAbiCode, "// Injected code below this line.")
	code = strings.ReplaceAll(code, "//disabledgo:", "//go:")
	// This constant is declared in our hash.go file.
	code = strings.ReplaceAll(code, "minHashLength", strconv.Itoa(minHashLength))
	return append(content, []byte(code)...), nil
}

// reflectMainPostPatch populates the name mapping with the final obfuscated->real name
// mappings after all packages have been analyzed.
func reflectMainPostPatch(file []byte, lpkg *listedPackage, pkg pkgCache) []byte {
	obfVarName := hashWithPackage(lpkg, "_originalNamePairs")
	namePairs := fmt.Appendf(nil, "%s = []string{", obfVarName)

	keys := slices.Sorted(maps.Keys(pkg.ReflectObjectNames))
	namePairsFilled := bytes.Clone(namePairs)
	for _, obf := range keys {
		namePairsFilled = fmt.Appendf(namePairsFilled, "%q, %q,", obf, pkg.ReflectObjectNames[obf])
	}

	return bytes.Replace(file, namePairs, namePairsFilled, 1)
}

//go:embed reflect_abi_code.go
var reflectAbiCode string
