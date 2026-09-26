package main

import (
	"strings"
	"testing"
)

// The compiler sees intrinsics by their original names, but compiled object
// symbols and transformed declarations use the same hashed spelling.
func TestIntrinsicSymbolMapUsesTransformedDeclaration(t *testing.T) {
	oldCache := sharedCache
	defer func() { sharedCache = oldCache }()
	pkg := &listedPackage{ImportPath: "math/bits", Name: "bits", CompiledGoFiles: []string{"bits.go"}}
	pkg.GarbleActionID[0] = 83
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	sharedCache.ListedPackages.set(pkg.ImportPath, pkg)
	name := "Len64"
	if isToolchainNameDependency(pkg.ImportPath, name) {
		t.Fatalf("intrinsic %s must be eligible for renaming", name)
	}
	hashed := obfuscatedPackageObjectName(pkg, name)
	if hashed == name {
		t.Fatal("intrinsic declaration not renamed")
	}
	mapping := pkg.obfuscatedImportPath() + "." + hashed + "=" + pkg.ImportPath + "." + name
	if !strings.Contains(","+buildSymbolMap()+",", ","+mapping+",") {
		t.Fatalf("missing intrinsic symbol mapping %s", mapping)
	}
}

func TestIntrinsicAndBuiltinSymbolProducersAgree(t *testing.T) {
	oldCache := sharedCache
	defer func() { sharedCache = oldCache }()
	pkg := &listedPackage{ImportPath: "math/bits", Name: "bits", CompiledGoFiles: []string{"bits.go"}}
	pkg.GarbleActionID[0] = 84
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	sharedCache.ListedPackages.set(pkg.ImportPath, pkg)
	oldBuiltin := builtinSymbols[pkg.ImportPath]
	defer func() {
		if oldBuiltin == nil {
			delete(builtinSymbols, pkg.ImportPath)
		} else {
			builtinSymbols[pkg.ImportPath] = oldBuiltin
		}
	}()
	builtinSymbols[pkg.ImportPath] = []string{"Len64"}
	mapping := pkg.obfuscatedImportPath() + "." + obfuscatedPackageObjectName(pkg, "Len64") + "=" + pkg.ImportPath + ".Len64"
	count := 0
	for _, entry := range strings.Split(buildSymbolMap(), ",") {
		if entry == mapping {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("intrinsic/builtin duplicate or conflicting mapping: %d for %q", count, mapping)
	}
}

func TestRuntimeSentinelRemainsReadable(t *testing.T) {
	if !isToolchainNameDependency("runtime", "goexit") {
		t.Fatal("runtime sentinel must retain its spelling")
	}
}
