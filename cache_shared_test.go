// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestListedTestMainCacheRoundTrip(t *testing.T) {
	original := listedPackage{
		Name:       "main",
		ImportPath: "example/p.test",
		BuildID:    "test-main-build",
		Module:     &listedModule{Path: "example", Version: "v1.2.3"},
		GoFiles:    []string{"/cache/generated-test-main-d"},
		Imports:    []string{"testing", "example/p"},
		ImportMap:  map[string]string{"example/p": "example/p [example/p.test]"},
	}
	data, err := original.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded listedPackage
	rest, err := decoded.UnmarshalMsg(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 || !reflect.DeepEqual(original, decoded) {
		t.Fatalf("test main changed during cache round trip: %#v", decoded)
	}
	if decoded.toObfuscate() {
		t.Fatal("retaining generated test-main source must not enable obfuscation")
	}
}

func TestTestVariantNamesLikePlainPackage(t *testing.T) {
	// A package rebuilt for a test binary is compiled with its plain import
	// path, and a package compiled only once writes linkname targets against
	// that same plain identity, so the variant has to name itself like the
	// plain package while keeping its own source directory.
	plain := &listedPackage{
		Name:            "pprof",
		ImportPath:      "runtime/pprof",
		CompiledGoFiles: []string{"pprof.go"},
		GarbleActionID:  [32]byte{1},
	}
	variant := &listedPackage{
		Name:            "pprof",
		ImportPath:      "runtime/pprof [runtime/pprof.test]",
		ForTest:         "runtime/pprof",
		CompiledGoFiles: []string{"pprof.go", "pprof_test.go"},
		GarbleActionID:  [32]byte{2},
	}
	pkgs := newListedPackages()
	pkgs.set(plain.ImportPath, plain)
	pkgs.set(variant.ImportPath, variant)
	old := sharedCache
	sharedCache = &sharedCacheType{ListedPackages: pkgs}
	t.Cleanup(func() { sharedCache = old })

	if got := variant.namingPackage(); got != plain {
		t.Fatalf("naming package = %q, want the plain package %q", got.ImportPath, plain.ImportPath)
	}
	if got, want := variant.obfuscatedImportPath(), plain.obfuscatedImportPath(); got != want {
		t.Errorf("obfuscated import path = %q, want %q", got, want)
	}
	if got, want := hashWithPackage(variant, "runtime_FrameStartLine"), hashWithPackage(plain, "runtime_FrameStartLine"); got != want {
		t.Errorf("hashed name = %q, want %q", got, want)
	}
	if got, want := variant.obfuscatedSourceDir(), plain.obfuscatedSourceDir(); got == want {
		t.Errorf("source directory %q is shared with the plain package", got)
	}

	// A variant whose plain package is not listed keeps its own identity, so
	// a build that never compiles the plain package stays self-consistent.
	orphan := &listedPackage{
		Name:            "q",
		ImportPath:      "example/q [other.test]",
		ForTest:         "other",
		CompiledGoFiles: []string{"q.go"},
		GarbleActionID:  [32]byte{3},
	}
	pkgs.set(orphan.ImportPath, orphan)
	if got := orphan.namingPackage(); got != orphan {
		t.Errorf("unlisted plain counterpart: naming package = %q, want the variant itself", got.ImportPath)
	}
}

func TestTestVariantsAreObfuscatable(t *testing.T) {
	for _, path := range []string{
		"crypto/internal/fips140",
		"crypto/internal/fips140/sha256",
		"runtime/cgo",
	} {
		variant := listedPackage{
			ImportPath:      path + " [example/p.test]",
			ForTest:         "example/p",
			CompiledGoFiles: []string{"file.go"},
		}
		if !variant.toObfuscate() {
			t.Errorf("%s must be obfuscatable when rebuilt for a test binary", path)
		}
		if got := variant.obfuscatedImportPath(); got == path {
			t.Errorf("obfuscated import path = %q, want a hashed path", got)
		}
	}

	// Ordinary test variants stay obfuscatable.
	variant := listedPackage{
		ImportPath:      "example/q [example/p.test]",
		ForTest:         "example/p",
		CompiledGoFiles: []string{"file.go"},
	}
	if !variant.toObfuscate() {
		t.Error("an ordinary package must stay obfuscatable when rebuilt for a test binary")
	}
}

// testRuntimePkgPathsAreMapped lists paths as synthetic packages in a fresh
// shared cache with the given seed, and checks that each one has a hashed path
// which the toolchain path map translates back to the original.
func testRuntimePkgPathsAreMapped(t *testing.T, seed string, paths []string) {
	t.Helper()
	oldCache, oldSeed := sharedCache, flagSeed
	t.Cleanup(func() { sharedCache, flagSeed = oldCache, oldSeed })
	flagSeed = seedFlag{bytes: []byte(seed)}
	sharedCache = &sharedCacheType{BinaryContentID: []byte("test"), ListedPackages: newListedPackages()}
	for _, path := range paths {
		sharedCache.ListedPackages.set(path, &listedPackage{
			Name:            "synthetic",
			ImportPath:      path,
			GarbleActionID:  addGarbleToHash([]byte(path)),
			CompiledGoFiles: []string{"synthetic.go"},
		})
	}

	mappings := strings.Split(buildRuntimePkgPathMap(), ",")
	for _, path := range paths {
		pkg, _ := sharedCache.ListedPackages.get(path)
		obfuscated := pkg.obfuscatedImportPath()
		if obfuscated == path {
			t.Fatalf("%s is exempt from path hashing; the test needs a hashed path", path)
		}
		if want := obfuscated + "=" + path; !slices.Contains(mappings, want) {
			t.Errorf("toolchain path map is missing %q; mappings: %v", want, mappings)
		}
	}
}

// TestNoRaceFuncPkgPathsAreMapped covers the packages whose functions the
// compiler keeps free of race memory instrumentation. cmd/compile looks that
// property up by package path, so an obfuscated path that the toolchain map
// does not translate makes the race detector instrument sync internals that
// plain Go leaves alone, and their unsynchronized state reads are then
// reported as data races in programs that contain none.
func TestNoRaceFuncPkgPathsAreMapped(t *testing.T) {
	// internal/runtime/atomic is the control: it is also a runtime package, so
	// its path is mapped even without the no-race-func table.
	testRuntimePkgPathsAreMapped(t, "no-race-func-paths",
		[]string{"internal/runtime/atomic", "internal/sync", "sync", "sync/atomic"})
}

// TestLinkerLoadedPkgPathsAreMapped covers the packages cmd/link opens by a
// literal import path rather than through a package's Imports. Their obfuscated
// paths only resolve if the toolchain path map contains them, so a missing entry
// silently drops cgo or the race, memory or address sanitizer runtime from the link.
func TestLinkerLoadedPkgPathsAreMapped(t *testing.T) {
	// runtime is the control: it is listed by the toolchain's own runtime
	// package table and must stay mapped.
	testRuntimePkgPathsAreMapped(t, "linker-internal-paths",
		[]string{"runtime", "runtime/cgo", "runtime/race", "runtime/msan", "runtime/asan", "runtime/pprof"})
}

// TestLinkOnlyReflectDepsFollowCoverage covers the packages whose reflected
// names a main package's cache has to merge although no package imports them.
// internal/coverage/cfile only reaches a covered test binary through
// runtime/coverage, so a merge driven by Imports alone leaves the coverage
// meta-data collection's field names obfuscated and the auxiliary fragments
// undecodable.
func TestLinkOnlyReflectDepsFollowCoverage(t *testing.T) {
	saved := sharedCache
	defer func() { sharedCache = saved }()
	sharedCache = &sharedCacheType{}
	for _, test := range []struct {
		name  string
		flags []string
		want  []string
	}{
		{"no flags", nil, nil},
		{"build flags", []string{"-trimpath", "-p", "1", "-ldflags=-s"}, nil},
		{"cover", []string{"-cover"}, []string{"runtime/coverage"}},
		{"coverpkg", []string{"-coverpkg=./..."}, []string{"runtime/coverage"}},
		{"covermode", []string{"-covermode=atomic"}, []string{"runtime/coverage"}},
		{"double hyphen", []string{"--cover"}, []string{"runtime/coverage"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sharedCache.ForwardBuildFlags = test.flags
			if got := linkOnlyReflectDeps(); !slices.Equal(got, test.want) {
				t.Fatalf("linkOnlyReflectDeps() = %v, want %v", got, test.want)
			}
		})
	}
}
