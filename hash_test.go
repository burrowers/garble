package main

import "testing"

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
