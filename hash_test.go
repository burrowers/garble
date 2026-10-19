package main

import (
	"fmt"
	"testing"
)

func TestLiteralInitBuildModeCacheIdentity(t *testing.T) {
	oldCache, oldLiterals := sharedCache, flagLiterals
	defer func() { sharedCache, flagLiterals = oldCache, oldLiterals }()
	sharedCache = &sharedCacheType{BinaryContentID: []byte("test"), ListedPackages: newListedPackages()}
	flagLiterals = true
	weak := addGarbleToHash([]byte("compiler"))
	for _, tc := range []struct {
		flags           []string
		plugins, strong bool
	}{
		{nil, false, false},
		{nil, true, true},
		{[]string{"-buildmode=default"}, false, false},
		{[]string{"-buildmode=exe"}, false, false},
		{[]string{"-buildmode=pie"}, false, false},
		{[]string{"-buildmode=archive"}, false, false},
		{[]string{"-buildmode=c-archive"}, false, true},
		{[]string{"-buildmode", "c-shared"}, false, true},
		{[]string{"-buildmode=shared"}, false, true},
		{[]string{"-buildmode=plugin"}, false, true},
		{[]string{"-linkshared"}, false, true},
		{[]string{"-linkshared=false"}, false, false},
		{[]string{"-buildmode=c-shared", "-buildmode=pie"}, false, false},
	} {
		t.Run(fmt.Sprintf("%v/plugins=%v", tc.flags, tc.plugins), func(t *testing.T) {
			sharedCache.ForwardBuildFlags, sharedCache.UsesPlugins = tc.flags, tc.plugins
			data, err := sharedCache.MarshalMsg(nil)
			if err != nil {
				t.Fatal(err)
			}
			var decoded sharedCacheType
			if _, err := decoded.UnmarshalMsg(data); err != nil {
				t.Fatal(err)
			}
			sharedCache = &decoded
			if strong := addGarbleToHash([]byte("compiler")) != weak; strong != tc.strong {
				t.Fatalf("strong initializer cache identity = %v, want %v", strong, tc.strong)
			}
		})
	}
	flagLiterals = false
	sharedCache.UsesPlugins = true
	withPlugin := addGarbleToHash([]byte("compiler"))
	sharedCache.UsesPlugins = false
	if withPlugin != addGarbleToHash([]byte("compiler")) {
		t.Fatal("plugin marker affects builds without literal obfuscation")
	}
}
