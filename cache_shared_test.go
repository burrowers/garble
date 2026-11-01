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

// TestLinkerLoadedPkgPathsAreMapped covers the packages cmd/link opens by a
// literal import path rather than through a package's Imports. Their obfuscated
// paths only resolve if the toolchain path map contains them, so a missing entry
// silently drops the race, memory or address sanitizer runtime from the link.
func TestLinkerLoadedPkgPathsAreMapped(t *testing.T) {
	oldCache, oldSeed := sharedCache, flagSeed
	defer func() { sharedCache, flagSeed = oldCache, oldSeed }()
	flagSeed = seedFlag{bytes: []byte("linker-internal-paths")}
	sharedCache = &sharedCacheType{BinaryContentID: []byte("test"), ListedPackages: newListedPackages()}

	// runtime is the control: it is listed by the toolchain's own runtime
	// package table and must stay mapped.
	paths := []string{"runtime", "runtime/race", "runtime/msan", "runtime/asan"}
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
