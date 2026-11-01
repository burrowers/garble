// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"strings"
)

// Exported method identities are global: an interface can be declared in a
// package which neither the implementation nor its dependencies import. Do not
// include the signature: generic declarations and their instantiations must
// use the same spelling even when their parameter types differ.
func obfuscatedMethodName(name string) string {
	if preservedMethodNames[name] {
		return name
	}
	salt := []byte("garble exported method")
	if !flagSeed.present() {
		withGarble := addGarbleToHash(salt)
		salt = withGarble[:]
	}
	return hashWithCustomSalt(salt, name)
}

func methodName(lpkg *listedPackage, name string) string {
	if !lpkg.toObfuscate() {
		return name
	}
	if token.IsExported(name) {
		return obfuscatedMethodName(name)
	}
	// Unexported methods retain their declaring package's identity in Go.
	return hashWithPackage(lpkg, name)
}

// A dependency can reflect on a receiver supplied only by its importers. Build
// the compiler's retention-name map from evaluated string constants, not the
// concrete reflected types known while compiling that dependency.
func writeCompilerMethodNames(info *types.Info) (string, error) {
	names := make(map[string]string)
	for _, tv := range info.Types {
		if tv.Value == nil || tv.Value.Kind() != constant.String {
			continue
		}
		name := constant.StringVal(tv.Value)
		if token.IsIdentifier(name) && token.IsExported(name) {
			names[name] = obfuscatedMethodName(name)
		}
	}
	file, err := os.CreateTemp(sharedTempDir, "method-names-")
	if err != nil {
		return "", err
	}
	if err := json.NewEncoder(file).Encode(names); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// Reflection exposes original names in lexical order, while the ABI method
// tables must stay in hashed order for interface dispatch. Translate indexes
// at the public reflection boundary, never by mutating those shared tables.
func reflectMethodsPatch(path, base string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	source := string(data)
	replace := func(old, new string) error {
		if strings.Count(source, old) != 1 {
			return fmt.Errorf("cannot patch reflected methods in %s: %q", path, old)
		}
		source = strings.Replace(source, old, new, 1)
		return nil
	}
	var edits [][2]string
	switch base {
	case "type.go":
		edits = [][2]string{
			{"p := methods[i]\n	pname :=", "p := methods[_garbleMethodIndex(t, i)]\n	pname :="},
			{"p := &t.Methods[i]\n	pname :=", "p := &t.Methods[_garbleMethodIndex((*rtype)(unsafe.Pointer(t)), i)]\n	pname :="},
			{"return t.Method(i), true\n		}", "return t.Method(_garbleMethodRank((*rtype)(unsafe.Pointer(t)), i)), true\n		}"},
		}
		start := strings.Index(source, "	// We are looking for the first index i where the string becomes >= s.")
		if start < 0 {
			return "", fmt.Errorf("cannot find MethodByName search in %s", path)
		}
		end := strings.Index(source[start:], "\n	return Method{}, false")
		if end < 0 {
			return "", fmt.Errorf("cannot find MethodByName search in %s", path)
		}
		source = source[:start] + `	for i := range methods {
		if name == t.nameOff(methods[i].Name).Name() {
			return t.Method(_garbleMethodRank(t, i)), true
		}
	}
` + source[start+end:]
		source += reflectMethodIndexCode
	case "value.go":
		edits = [][2]string{{"fl |= flag(i)<<flagMethodShift | flagMethod", "fl |= flag(_garbleMethodIndex(toRType(v.typ()), i))<<flagMethodShift | flagMethod"}}
	}
	for _, edit := range edits {
		if err := replace(edit[0], edit[1]); err != nil {
			return "", err
		}
	}
	return source, nil
}

const reflectMethodIndexCode = `
func _garbleMethodCount(t *rtype) int {
	if t.Kind() == Interface { return len((*interfaceType)(unsafe.Pointer(t)).Methods) }
	return len(t.exportedMethods())
}

func _garbleMethodName(t *rtype, i int) string {
	if t.Kind() == Interface { return t.nameOff((*interfaceType)(unsafe.Pointer(t)).Methods[i].Name).Name() }
	return t.nameOff(t.exportedMethods()[i].Name).Name()
}

func _garbleMethodRank(t *rtype, raw int) int {
	name := _garbleMethodName(t, raw)
	rank := 0
	for i := 0; i < _garbleMethodCount(t); i++ {
		other := _garbleMethodName(t, i)
		if other < name || other == name && i < raw { rank++ }
	}
	return rank
}

func _garbleMethodIndex(t *rtype, index int) int {
	for i := 0; i < _garbleMethodCount(t); i++ {
		if _garbleMethodRank(t, i) == index { return i }
	}
	panic("reflect: Method index out of range")
}
`
