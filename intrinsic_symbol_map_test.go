package main

import (
	"strings"
	"testing"
)

func TestSymbolMapDefersGoFunctionRenaming(t *testing.T) {
	oldCache := sharedCache
	defer func() { sharedCache = oldCache }()
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	for _, tc := range []struct {
		path  string
		names []string
	}{
		{"math/bits", []string{"Len64", "ReverseBytes64", "Div32"}},
		{"internal/byteorder", []string{
			"LEUint16", "LEUint32", "LEUint64",
			"BEUint16", "BEUint32", "BEUint64",
			"LEPutUint16", "LEPutUint32", "LEPutUint64",
			"BEPutUint16", "BEPutUint32", "BEPutUint64",
			"LEAppendUint16", "LEAppendUint32", "LEAppendUint64",
			"BEAppendUint16", "BEAppendUint32", "BEAppendUint64",
		}},
		{"internal/runtime/math", []string{"Add64", "MulUintptr"}},
	} {
		pkg := &listedPackage{ImportPath: tc.path, Standard: true, CompiledGoFiles: []string{"source.go"}}
		pkg.GarbleActionID[0] = 83
		sharedCache.ListedPackages.set(pkg.ImportPath, pkg)
		prefix := pkg.obfuscatedImportPath()
		symbols := "," + buildSymbolMap() + ","
		for _, name := range tc.names {
			emitted := prefix + "." + obfuscatedPackageObjectName(pkg, name)
			want := "@" + prefix + "." + name + "=" + emitted
			if !strings.Contains(symbols, ","+want+",") {
				t.Errorf("%s.%s: missing emission mapping", tc.path, name)
			}
			if strings.Contains(symbols, ","+emitted+"="+tc.path+"."+name+",") {
				t.Errorf("%s.%s: unexpectedly uses a reverse symbol mapping", tc.path, name)
			}
		}
		if tc.path == "math/bits" && strings.Contains(symbols, ",@"+prefix+".divideError=") {
			t.Error("linkname alias incorrectly marked for renaming at emission")
		}
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
