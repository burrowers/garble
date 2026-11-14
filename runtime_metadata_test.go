package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Real consumers and typed constants are exercised by runtime_metadata.txtar.
// Keep synthetic literals here to cover names absent from a consumer's sources.
func TestRuntimeMetadataConsumerBoundaries(t *testing.T) {
	oldCache := sharedCache
	t.Cleanup(func() { sharedCache = oldCache })
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	for _, path := range []string{"runtime", "reflect"} {
		sharedCache.ListedPackages.set(path, &listedPackage{ImportPath: path, CompiledGoFiles: []string{"file.go"}, GarbleActionID: [32]byte{87}})
	}
	for _, path := range []string{"runtime", "runtime/pprof", "testing", "example/user", "reflect"} {
		t.Run(path, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "file.go", `package p
var names = []string{"runtime.goexit", "runtime.gopanic", "reflect.makeFuncStub", "reflect.methodValueCall", "unrelated"}
func other() string { return "reflect.Value." }
const diagnostic = "reflect.Value."
`, 0)
			if err != nil {
				t.Fatal(err)
			}
			original := make(map[*ast.BasicLit]string)
			for node := range ast.Preorder(file) {
				if literal, ok := node.(*ast.BasicLit); ok {
					original[literal] = literal.Value
				}
			}
			transformRuntimeMetadataNames(&listedPackage{ImportPath: path}, file)
			for literal, before := range original {
				name, err := strconv.Unquote(before)
				if err != nil {
					t.Fatal(err)
				}
				shouldChange := (path == "runtime" || path == "runtime/pprof") && name != "unrelated" && name != "reflect.Value." || path == "testing" && name == "runtime.gopanic"
				if (before != literal.Value) != shouldChange {
					t.Errorf("metadata literal %s -> %s, shouldChange=%v", before, literal.Value, shouldChange)
				}
			}
		})
	}
}
