package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

func abiNamePatch(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	find := `return unsafe.String(n.DataChecked(1+i, "non-empty string"), l)`
	replace := `return _realName(unsafe.String(n.DataChecked(1+i, "non-empty string"), l))`

	str := strings.Replace(string(data), find, replace, 1)

	realname := `
//go:linkname _realName
func _realName(name string) string
`

	return str + realname, nil
}

var reflectPatchFile = ""

// reflectMainPrePatch adds the initial empty name mapping and _realName implementation
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

	nameMap := "\nvar _nameMap = map[string]string{}"

	return append(content, []byte(realNameCode+nameMap)...), nil
}

// reflectMainPostPatch populates the name mapping with the final obfuscated->real name
// mappings after all packages have been analyzed.
func reflectMainPostPatch(file []byte, lpkg *listedPackage, pkg pkgCache) []byte {
	obfMapName := hashWithPackage(lpkg, "_nameMap")
	nameMap := fmt.Sprintf("%s = map[string]string{", obfMapName)

	var b strings.Builder
	keys := slices.Sorted(maps.Keys(pkg.ReflectObjectNames))
	for _, obf := range keys {
		b.WriteString(fmt.Sprintf(`"%s": "%s",`, obf, pkg.ReflectObjectNames[obf]))
	}

	return bytes.Replace(file, []byte(nameMap), []byte(nameMap+b.String()), 1)
}

// The "name" internal/abi passes to this function doesn't have to be a simple "someName"
// it can also be for function names:
// "*pkgName.FuncName" (obfuscated)
// or for structs the entire struct definition:
// "*struct { AQ45rr68K string; ipq5aQSIqN string; hNfiW5O5LVq struct { gPTbGR00hu string } }"
//
// Therefore all obfuscated names which occur within name need to be replaced with their "real" equivalents.
//
// The code below does a more efficient version of:
//
//	func _realName(name string) string {
//			for obfName, real := range _nameMap {
//				name = strings.ReplaceAll(name, obfName, real)
//			}
//
//			return name
//	}
const realNameCode = `
//go:linkname _realName internal/abi._realName
func _realName(name string) string {
	for i := 0; i < len(name); {
		remLen := len(name[i:])
		found := false
		for obfName, real := range _nameMap {
			keyLen := len(obfName)
			if keyLen > remLen {
				continue
			}
			if name[i:i+keyLen] == obfName {
				name = name[:i] + real + name[i+keyLen:]
				found = true
				i += len(real)
				break
			}
		}
		if !found {
			i++
		}
	}
	return name
}`
