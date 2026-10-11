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
		flags  []string
		strong bool
	}{
		{nil, false},
		{[]string{"-buildmode=default"}, false},
		{[]string{"-buildmode=exe"}, false},
		{[]string{"-buildmode=pie"}, false},
		{[]string{"-buildmode=archive"}, false},
		{[]string{"-buildmode=c-archive"}, true},
		{[]string{"-buildmode", "c-shared"}, true},
		{[]string{"-buildmode=shared"}, true},
		{[]string{"-buildmode=plugin"}, true},
		{[]string{"-linkshared"}, true},
		{[]string{"-linkshared=false"}, false},
		{[]string{"-buildmode=c-shared", "-buildmode=pie"}, false},
	} {
		t.Run(fmt.Sprint(tc.flags), func(t *testing.T) {
			sharedCache.ForwardBuildFlags = tc.flags
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
}

func TestLiteralInitPluginCacheIdentity(t *testing.T) {
	oldCache, oldLiterals := sharedCache, flagLiterals
	defer func() { sharedCache, flagLiterals = oldCache, oldLiterals }()
	sharedCache = &sharedCacheType{BinaryContentID: []byte("test")}
	flagLiterals = true
	without := addGarbleToHash([]byte("compiler"))
	sharedCache.UsesPlugins = true
	with := addGarbleToHash([]byte("compiler"))
	if without == with {
		t.Fatal("plugin builds can reuse weak literal initializer objects")
	}
	flagLiterals = false
	with = addGarbleToHash([]byte("compiler"))
	sharedCache.UsesPlugins = false
	if with != addGarbleToHash([]byte("compiler")) {
		t.Fatal("plugin marker affects builds without literal obfuscation")
	}
}

func TestLiteralInitPluginCacheRoundTrip(t *testing.T) {
	original := sharedCacheType{UsesPlugins: true, ListedPackages: newListedPackages()}
	data, err := original.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sharedCacheType
	if _, err := decoded.UnmarshalMsg(data); err != nil {
		t.Fatal(err)
	}
	if !decoded.UsesPlugins {
		t.Fatal("plugin safeguard is lost in compiler subprocesses")
	}
}
