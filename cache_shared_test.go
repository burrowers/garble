// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"reflect"
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

func TestTestVariantsKeepUnsupportedPackagesReadable(t *testing.T) {
	// A dependency rebuilt for a test binary gets an ImportPath such as
	// "crypto/internal/fips140 [strings.test]" while ForTest names the
	// package under test. The packages we cannot obfuscate must stay
	// readable in that variant too.
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
		if variant.toObfuscate() {
			t.Errorf("%s must not be obfuscated when rebuilt for a test binary", path)
		}
		if got, want := variant.obfuscatedImportPath(), path; got != want {
			t.Errorf("obfuscated import path = %q, want the readable path %q", got, want)
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
