package main

import "testing"

func TestRuntimeCgoIsObfuscated(t *testing.T) {
	pkg := listedPackage{ImportPath: "runtime/cgo [example/p.test]", CompiledGoFiles: []string{"callbacks.go"}}
	if !pkg.toObfuscate() {
		t.Fatal("runtime/cgo test variant must be obfuscatable")
	}
}
