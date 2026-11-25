package main

import (
	"strings"
	"testing"
)

func TestIntrinsicSymbolMapDefersLen64Renaming(t *testing.T) {
	oldCache := sharedCache
	defer func() { sharedCache = oldCache }()
	pkg := &listedPackage{ImportPath: "math/bits", Name: "bits", Standard: true, CompiledGoFiles: []string{"bits.go"}}
	pkg.GarbleActionID[0] = 83
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	sharedCache.ListedPackages.set(pkg.ImportPath, pkg)
	prefix := pkg.obfuscatedImportPath()
	emitted := prefix + "." + obfuscatedPackageObjectName(pkg, "Len64")
	symbols := strings.SplitSeq(buildSymbolMap(), ",")
	want := "@" + prefix + ".Len64=" + emitted
	found := false
	for entry := range symbols {
		if entry == emitted+"=math/bits.Len64" {
			t.Fatal("Len64 must not need a reverse intrinsic mapping")
		}
		found = found || entry == want
	}
	if !found {
		t.Fatalf("missing emission mapping %s", want)
	}
}

// A SIMD intrinsic is registered per receiver type and method, and the
// compiler looks up the original spelling of each half, so the symbol map has
// to carry each declaration instead of the qualified combination.
func TestIntrinsicSymbolMapSplitsReceiverQualifiedNames(t *testing.T) {
	oldCache := sharedCache
	defer func() { sharedCache = oldCache }()
	pkg := &listedPackage{ImportPath: "simd/archsimd", Name: "archsimd", CompiledGoFiles: []string{"types_amd64.go"}}
	pkg.GarbleActionID[0] = 85
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages(), BinaryContentID: []byte("test")}
	sharedCache.ListedPackages.set(pkg.ImportPath, pkg)
	mapText := buildSymbolMap()
	if strings.Contains(mapText, "=simd/archsimd.Uint8x16.Add") {
		t.Fatal("symbol map maps a receiver-qualified name instead of its declarations")
	}
	for _, want := range []struct {
		name   string
		method bool
	}{
		{name: "Uint8x16"},
		{name: "Add", method: true},
		{name: "v128"},
	} {
		obfuscated := obfuscatedPackageObjectName(pkg, want.name)
		if want.method {
			obfuscated = methodName(pkg, want.name)
		}
		entry := pkg.obfuscatedImportPath() + "." + obfuscated + "=" + pkg.ImportPath + "." + want.name
		if !strings.Contains(","+mapText+",", ","+entry+",") {
			t.Fatalf("missing SIMD symbol mapping %s", entry)
		}
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
	builtinSymbols[pkg.ImportPath] = []string{"Len32"}
	mapping := pkg.obfuscatedImportPath() + "." + obfuscatedPackageObjectName(pkg, "Len32") + "=" + pkg.ImportPath + ".Len32"
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

func TestRuntimeSentinelUsesMappedDeclaration(t *testing.T) {
	if isToolchainNameDependency("runtime", "goexit") {
		t.Fatal("runtime sentinel declaration must be eligible for renaming")
	}
}
