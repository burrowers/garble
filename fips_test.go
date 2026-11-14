// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func corruptFIPSChecksum() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: fipscorrupt input output")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	magic := []byte("\xff Go fipsinfo \xff\x00")
	i := bytes.Index(data, magic)
	if i < 0 || bytes.Contains(data[i+len(magic):], magic) || i+len(magic)+32 > len(data) {
		panic("expected exactly one complete FIPS checksum")
	}
	data[i+len(magic)] ^= 1
	if err := os.WriteFile(os.Args[2], data, 0o755); err != nil {
		panic(err)
	}
}

func TestFIPSSnapshotPackageLookup(t *testing.T) {
	old := sharedCache
	defer func() { sharedCache = old }()
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	root := &listedPackage{ImportPath: "crypto/internal/fips140/v1.0.0-c2097c7c", Standard: true, CompiledGoFiles: []string{"fips.go"}}
	probe := &listedPackage{ImportPath: root.ImportPath + "/check/checktest", Standard: true, CompiledGoFiles: []string{"test.go"}}
	sharedCache.ListedPackages.set(root.ImportPath, root)
	sharedCache.ListedPackages.set(probe.ImportPath, probe)
	for _, test := range []struct {
		path string
		want *listedPackage
	}{{"crypto/internal/fips140", root}, {"crypto/internal/fips140/check/checktest", probe}, {probe.ImportPath, probe}} {
		pkg, err := listPackage(probe, test.path)
		if err != nil || pkg != test.want {
			t.Errorf("list %s = %v, %v; want %v", test.path, pkg, err, test.want)
		}
	}
}

func TestFIPSLinknamesObfuscated(t *testing.T) {
	oldCache, oldSeed := sharedCache, flagSeed
	defer func() { sharedCache, flagSeed = oldCache, oldSeed }()
	flagSeed = seedFlag{bytes: []byte("fips-linknames-test")}
	for _, path := range []string{"crypto/internal/fips140", "crypto/internal/fips140/v1.0.0-c2097c7c"} {
		sharedCache = &sharedCacheType{BinaryContentID: []byte("test"), ListedPackages: newListedPackages()}
		fips := &listedPackage{Name: "fips140", ImportPath: path, CompiledGoFiles: []string{"fips.go"}, GarbleActionID: [32]byte{1}}
		runtime := &listedPackage{Name: "runtime", ImportPath: "runtime", Standard: true, CompiledGoFiles: []string{"runtime.go"}, GarbleActionID: [32]byte{2}}
		sharedCache.ListedPackages.set(path, fips)
		sharedCache.ListedPackages.set("runtime", runtime)
		fips.BuildID = strings.Repeat("A", 20) + "/content"
		before := linknameActionID()
		fips.BuildID = strings.Repeat("B", 20) + "/content"
		if before == linknameActionID() {
			t.Errorf("%s: runtime FIPS linkname target changed without invalidating its cache identity", path)
		}
		for _, name := range []string{"fatal", "getIndicator", "setIndicator"} {
			tf := &transformer{curPkg: runtime}
			_, got := tf.transformLinkname("runtimeHook", "crypto/internal/fips140."+name)
			want := fips.obfuscatedImportPath() + "." + hashWithPackage(fips, name)
			if got != want {
				t.Errorf("%s: linkname = %q, want %q", path, got, want)
			}
			mapping := want + "=crypto/internal/fips140." + name
			if !strings.Contains(buildSymbolMap(), mapping) {
				t.Errorf("missing FIPS ABI mapping %s", mapping)
			}
		}
	}
}
