package main

import (
	"go/ast"
	"go/types"
	"testing"
)

func TestCompilerContractMethodsAreObfuscated(t *testing.T) {
	oldCache, oldTemp := sharedCache, sharedTempDir
	t.Cleanup(func() { sharedCache, sharedTempDir = oldCache, oldTemp })
	sharedCache = &sharedCacheType{BinaryContentID: []byte("contract methods")}
	sharedTempDir = t.TempDir()
	for _, name := range []string{"Method", "MethodByName"} {
		if obfuscatedMethodName(name) == name {
			t.Errorf("compiler contract method %s is still preserved", name)
		}
	}
	names := compilerMethodNames(t, &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)})
	for _, name := range []string{"Method", "MethodByName", "Pointer", "UnsafeAddr"} {
		if names[name] != obfuscatedMethodName(name) {
			t.Errorf("compiler-owned method %s missing without source constants", name)
		}
	}
}
