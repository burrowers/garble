// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestLinknameActionIDExternalTestPresence(t *testing.T) {
	oldCache, oldSeed := sharedCache, flagSeed
	t.Cleanup(func() { sharedCache, flagSeed = oldCache, oldSeed })
	for _, mode := range []string{"unseeded", "fixed-seed"} {
		t.Run(mode, func(t *testing.T) {
			flagSeed = seedFlag{}
			if mode == "fixed-seed" {
				flagSeed.bytes = []byte("12345678")
			}
			runtime := &listedPackage{
				ImportPath: "runtime", Name: "runtime", Standard: true,
				BuildID: strings.Repeat("A", 20) + "/content", CompiledGoFiles: []string{"runtime.go"},
			}
			placeholder := &listedPackage{ImportPath: "sync_test"}
			external := &listedPackage{
				ImportPath: "sync_test [sync.test]", ForTest: "sync", Name: "sync_test",
				BuildID: strings.Repeat("E", 20) + "/content", CompiledGoFiles: []string{"sync_test.go"},
			}
			sharedCache = &sharedCacheType{
				ListedPackages: newListedPackages(), BinaryContentID: []byte("test binary"),
			}
			sharedCache.ListedPackages.set(runtime.ImportPath, runtime)
			sharedCache.ListedPackages.set(placeholder.ImportPath, placeholder)
			type state struct {
				linknameActionID, runtimeActionID, toolID [32]byte
				target                                    string
			}
			snapshot := func() state {
				sharedCache.LinknameActionID = linknameActionID()
				for _, pkg := range sharedCache.ListedPackages.all() {
					if pkg.BuildID != "" {
						pkg.GarbleActionID = addGarbleToHash(decodeBuildIDHash(splitActionID(pkg.BuildID)))
					}
				}
				tf := &transformer{curPkg: runtime}
				_, target := tf.transformLinkname("sync_test_runtime_blockUntilEmptyCleanupQueue", "sync_test.runtime_blockUntilEmptyCleanupQueue")
				return state{sharedCache.LinknameActionID, runtime.GarbleActionID, addGarbleToHash([]byte("tool identity")), target}
			}
			before := snapshot()
			sharedCache.ListedPackages.set(external.ImportPath, external)
			if external.namingPackage() != placeholder {
				t.Fatal("external test symbols must retain the plain placeholder's naming identity")
			}
			after := snapshot()
			if before.target != "sync_test.runtime_blockUntilEmptyCleanupQueue" || after.target == before.target {
				t.Fatalf("expected external test presence to obfuscate the runtime target: before=%q after=%q", before.target, after.target)
			}
			if before.linknameActionID == after.linknameActionID || before.runtimeActionID == after.runtimeActionID || before.toolID == after.toolID {
				t.Fatalf("runtime linkname output changed without invalidating cache identities: digest_equal=%v runtime_equal=%v tool_equal=%v; before=%q after=%q",
					before.linknameActionID == after.linknameActionID, before.runtimeActionID == after.runtimeActionID, before.toolID == after.toolID, before.target, after.target)
			}
			external.BuildID = strings.Repeat("F", 20) + "/other-content"
			if changed := snapshot(); changed != after {
				t.Fatal("the variant's action ID must not replace the canonical original naming identity")
			}
		})
	}
}

func TestMainLinknameTargetNaming(t *testing.T) {
	for _, test := range []struct {
		name string
		src  string
		want string
	}{
		{"none", "package p\n", ""},
		{"variable", "package p\n\n//go:linkname counter main.counter\nvar counter int\n", "counter"},
		{"function", "package p\n\n//go:linkname marker main.mainMarker\nfunc marker() string\n", "mainMarker"},
		{"linknamestd", "package p\n//go:linknamestd marker main.mainMarker\nfunc marker() string\n", "mainMarker"},
		{"pointer method", "package p\n//go:linkname inc main.(*T).inc\nfunc inc(*T, int) int\n", "T,inc"},
		{"value method", "package p\n//go:linkname inc main.T.inc\nfunc inc(T, int) int\n", "T,inc"},
		{"indented and tabs", "package p\n	//go:linkname	counter	main.counter\n", "counter"},
		{"other package", "package p\n//go:linkname marker other.Marker\nfunc marker() string\n", ""},
		{"local only", "package p\n//go:linkname marker\nfunc marker() string\n", ""},
		{"main subpackage", "package p\n//go:linkname value main/sub.Value\nvar value int\n", ""},
		{"inside a string literal line", "package p\nvar s = \"//go:linkname counter main.counter\"\n", ""},
		{"three arguments", "package p\n//go:linkname counter main.counter extra\n", ""},
	} {
		targets := make(map[string]bool)
		collectMainLinknameTargets([]byte(test.src), targets)
		got := strings.Join(slices.Sorted(maps.Keys(targets)), ",")
		if got != test.want {
			t.Errorf("%s: got %q, want %q", test.name, got, test.want)
		}
	}

	old := sharedCache
	t.Cleanup(func() { sharedCache = old })
	sharedCache = &sharedCacheType{
		ListedPackages:      newListedPackages(),
		MainLinknameTargets: []string{"counter"},
	}
	mainPkg := &listedPackage{ImportPath: "example.com/app", Name: "main"}
	if !isMainLinknameTarget(mainPkg, "counter") {
		t.Fatal("a linkname target of the main package must keep its name")
	}
	if isMainLinknameTarget(mainPkg, "other") {
		t.Fatal("unrelated main-package names must stay obfuscated")
	}
	library := &listedPackage{ImportPath: "example.com/app/lib", Name: "lib"}
	if isMainLinknameTarget(library, "counter") {
		t.Fatal("the exemption must only cover the main package")
	}
	testMain := &listedPackage{ImportPath: "example.com/app.test", Name: "main", ForTest: "example.com/app"}
	if isMainLinknameTarget(testMain, "counter") {
		t.Fatal("a package rebuilt for a test binary is not the linked main package")
	}

	data, err := sharedCache.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sharedCacheType
	if rest, err := decoded.UnmarshalMsg(data); err != nil || len(rest) != 0 {
		t.Fatalf("shared cache decode: remaining=%d, error=%v", len(rest), err)
	}
	if got := strings.Join(decoded.MainLinknameTargets, ","); got != "counter" {
		t.Fatalf("shared cache lost the main linkname targets: %q", got)
	}
}

func TestLinknameActionID(t *testing.T) {
	old := sharedCache
	t.Cleanup(func() { sharedCache = old })
	pkg := func(path, action string) *listedPackage {
		return &listedPackage{ImportPath: path, BuildID: strings.Repeat(action, 20) + "/content"}
	}
	runtime := pkg("runtime", "A")
	target := pkg("internal/godebug", "B")
	application := pkg("example.com/main", "C")
	digest := func(packages ...*listedPackage) [32]byte {
		sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
		for _, p := range packages {
			sharedCache.ListedPackages.set(p.ImportPath, p)
		}
		return linknameActionID()
	}
	want := digest(runtime, target, application)
	variant := pkg("internal/godebug [example.com/main.test]", "D")
	variant.ForTest = "example.com/main"
	external := pkg("sync_test [sync.test]", "E")
	external.ForTest = "sync"
	for _, test := range []struct {
		name     string
		packages []*listedPackage
		changed  bool
	}{
		{"insertion order", []*listedPackage{application, target, runtime}, false},
		{"unrelated action", []*listedPackage{runtime, target, pkg(application.ImportPath, "D")}, false},
		{"content ID", []*listedPackage{runtime, {ImportPath: target.ImportPath, BuildID: strings.Repeat("B", 20) + "/other-content"}, application}, false},
		{"plain test identity", []*listedPackage{runtime, target, variant, application}, false},
		{"target action", []*listedPackage{runtime, pkg(target.ImportPath, "D"), application}, true},
		{"missing target", []*listedPackage{runtime, application}, true},
		{"external test target", []*listedPackage{runtime, target, external, application}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := digest(test.packages...); (got != want) != test.changed {
				t.Fatalf("linkname identity changed = %v, want %v", got != want, test.changed)
			}
		})
	}
	if got, want := digest(runtime, &listedPackage{ImportPath: target.ImportPath}), digest(runtime); got != want {
		t.Fatal("a build-constrained package must contribute no naming dependency")
	}

	sharedCache.LinknameActionID = want
	sharedCache.BinaryContentID = []byte("test binary")
	data, err := sharedCache.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sharedCacheType
	if rest, err := decoded.UnmarshalMsg(data); err != nil || len(rest) != 0 {
		t.Fatalf("shared cache decode: remaining=%d, error=%v", len(rest), err)
	}
	if decoded.LinknameActionID != want {
		t.Fatal("shared cache lost the foreign naming dependencies")
	}
	before := addGarbleToHash([]byte("tool identity"))
	sharedCache.LinknameActionID[0] ^= 1
	if before == addGarbleToHash([]byte("tool identity")) {
		t.Fatal("foreign naming dependencies must invalidate Go's tool identity")
	}
}
