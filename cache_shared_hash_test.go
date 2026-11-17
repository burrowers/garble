// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"strings"
	"testing"
)

func TestLinknameActionIDExternalTestPresence(t *testing.T) {
	oldCache, oldSeed := sharedCache, flagSeed
	t.Cleanup(func() { sharedCache, flagSeed = oldCache, oldSeed })
	for _, test := range []struct {
		path, localName, targetName string
		plainPlaceholder            bool
	}{
		{"sync_test", "sync_test_runtime_blockUntilEmptyCleanupQueue", "runtime_blockUntilEmptyCleanupQueue", true},
		{"runtime/metrics_test", "readMetricNames", "runtime_readMetricNames", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			if test.path == "runtime/metrics_test" {
				t.Skip("TODO: include metrics test linknames in the cache identity")
			}
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
					placeholder := &listedPackage{ImportPath: test.path}
					forTest := strings.TrimSuffix(test.path, "_test")
					external := &listedPackage{
						ImportPath: test.path + " [" + forTest + ".test]", ForTest: forTest, Name: strings.TrimPrefix(test.path, "runtime/"),
						BuildID: strings.Repeat("E", 20) + "/content", CompiledGoFiles: []string{"sync_test.go"},
					}
					sharedCache = &sharedCacheType{
						ListedPackages: newListedPackages(), BinaryContentID: []byte("test binary"),
					}
					sharedCache.ListedPackages.set(runtime.ImportPath, runtime)
					if test.plainPlaceholder {
						sharedCache.ListedPackages.set(placeholder.ImportPath, placeholder)
					}
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
						_, target := tf.transformLinkname(test.localName, test.path+"."+test.targetName)
						return state{sharedCache.LinknameActionID, runtime.GarbleActionID, addGarbleToHash([]byte("tool identity")), target}
					}
					before := snapshot()
					sharedCache.ListedPackages.set(external.ImportPath, external)
					wantNaming := external
					if test.plainPlaceholder {
						wantNaming = placeholder
					}
					if external.namingPackage() != wantNaming {
						t.Fatal("external test symbols must retain the canonical naming identity")
					}
					after := snapshot()
					t.Run("presence", func(t *testing.T) {
						if before.target != test.path+"."+test.targetName || after.target == before.target {
							t.Fatalf("expected external test presence to obfuscate the runtime target: before=%q after=%q", before.target, after.target)
						}
						if before.linknameActionID == after.linknameActionID || before.runtimeActionID == after.runtimeActionID || before.toolID == after.toolID {
							t.Fatalf("runtime linkname output changed without invalidating cache identities: digest_equal=%v runtime_equal=%v tool_equal=%v; before=%q after=%q",
								before.linknameActionID == after.linknameActionID, before.runtimeActionID == after.runtimeActionID, before.toolID == after.toolID, before.target, after.target)
						}
					})
					t.Run("action", func(t *testing.T) {
						external.BuildID = strings.Repeat("F", 20) + "/other-content"
						changed := snapshot()
						if test.plainPlaceholder {
							if changed != after {
								t.Fatal("the variant's action ID must not replace the canonical original naming identity")
							}
						} else {
							if changed.linknameActionID == after.linknameActionID || changed.runtimeActionID == after.runtimeActionID || changed.toolID == after.toolID {
								t.Fatalf("the variant's fallback naming identity must invalidate caches: digest_equal=%v runtime_equal=%v tool_equal=%v",
									changed.linknameActionID == after.linknameActionID, changed.runtimeActionID == after.runtimeActionID, changed.toolID == after.toolID)
							}
							if (changed.target != after.target) != (mode == "unseeded") {
								t.Fatalf("fallback action must affect only unseeded names: before=%q after=%q", after.target, changed.target)
							}
						}
					})
				})
			}
		})
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
