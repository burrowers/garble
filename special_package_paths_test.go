// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"path"
	"slices"
	"strings"
	"testing"
)

func TestSpecialPackagePathsAreObfuscated(t *testing.T) {
	oldCache, oldSeed := sharedCache, flagSeed
	t.Cleanup(func() { sharedCache, flagSeed = oldCache, oldSeed })
	flagSeed = seedFlag{bytes: []byte("special-package-paths")}
	sharedCache = &sharedCacheType{BinaryContentID: []byte("test"), ListedPackages: newListedPackages()}
	paths := []string{
		"reflect", "embed", "structs", "syscall", "internal/abi",
		"internal/bytealg", "internal/chacha8rand", "internal/runtime/sys",
		"internal/runtime/syscall/linux", "internal/runtime/syscall/windows",
		"internal/runtime/startlinetest",
	}
	for _, pkgPath := range paths {
		pkg := &listedPackage{
			Name: path.Base(pkgPath), ImportPath: pkgPath,
			GarbleActionID:  addGarbleToHash([]byte(pkgPath)),
			CompiledGoFiles: []string{"synthetic.go"},
		}
		sharedCache.ListedPackages.set(pkgPath, pkg)
	}
	mappings := strings.Split(buildRuntimePkgPathMap(), ",")
	for _, pkgPath := range paths {
		t.Run(pkgPath, func(t *testing.T) {
			pkg, _ := sharedCache.ListedPackages.get(pkgPath)
			obfuscated := pkg.obfuscatedImportPath()
			if obfuscated == pkgPath {
				t.Errorf("package path %q was preserved", pkgPath)
			}
			if pkg.obfuscatedPackageName() == pkg.Name {
				t.Errorf("package name %q was preserved", pkg.Name)
			}
			if want := obfuscated + "=" + pkgPath; !slices.Contains(mappings, want) {
				t.Errorf("toolchain path map is missing %q", want)
			}
		})
	}
}
