// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

// toolchainNameDependencies contains declarations whose exact symbol names are
// consumed by the Go runtime even though they are not compiler intrinsics or
// go:linkname targets.
//
// runtime.stkframe.argMapInternal matches these reflect assembly stubs by name
// to synthesize their dynamic argument maps while the garbage collector scans
// a stack. Renaming either stub can make a live pointer appear to be freed.
var toolchainNameDependencies = map[string]map[string]bool{
	"reflect": {
		"makeFuncStub":    true,
		"methodValueCall": true,
	},
}

func isToolchainNameDependency(path, name string) bool {
	return compilerIntrinsics[path][name] || toolchainNameDependencies[path][name]
}
