package main

import (
	"fmt"
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
	for _, tc := range []struct {
		path       string
		name       string
		standard   bool
		obfuscated bool
	}{
		{path: "runtime"},
		{path: "runtime/pprof"},
		{path: "testing"},
		// A package whose own path is kept has nothing to rewrite.
		{path: "example/user"},
		{path: "reflect"},
		// A standard and a non-standard package whose own path is hashed. The
		// standard-library table names net/http only, because a standard
		// package such as runtime keeps its plain path in symbol names.
		{path: "net/http", standard: true, obfuscated: true},
		{path: "strings", standard: true, obfuscated: true},
		{path: "example.com/user/pkg", obfuscated: true},
		// Package main keeps the "main" prefix in both builds.
		{path: "example.com/user/cmd", name: "main", obfuscated: true},
	} {
		path := tc.path
		t.Run(path, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "file.go", fmt.Sprintf(`package p
var names = []string{"runtime.goexit", "runtime.gopanic", "reflect.makeFuncStub", "reflect.methodValueCall", "unrelated", %q, %q}
func other() string { return "reflect.Value." }
const diagnostic = "reflect.Value."
`, path+".", path+": "), 0)
			if err != nil {
				t.Fatal(err)
			}
			original := make(map[*ast.BasicLit]string)
			for node := range ast.Preorder(file) {
				if literal, ok := node.(*ast.BasicLit); ok {
					original[literal] = literal.Value
				}
			}
			lpkg := &listedPackage{ImportPath: path, Name: tc.name, Standard: tc.standard}
			if tc.obfuscated {
				// A package path comparison only needs rewriting when the
				// consumer's own path is obfuscated.
				lpkg.CompiledGoFiles = []string{"file.go"}
				lpkg.GarbleActionID = [32]byte{87}
			}
			transformRuntimeMetadataNames(lpkg, file)
			for literal, before := range original {
				name, err := strconv.Unquote(before)
				if err != nil {
					t.Fatal(err)
				}
				// Package main keeps its plain prefix in both builds.
				selfPrefix := !tc.standard && tc.obfuscated && tc.name != "main" && name == path+"."
				tablePrefix := tc.path == "net/http" && name == "net/http."
				symbol := name != "unrelated" && name != "reflect.Value." && name != path+"." && name != path+": "
				shouldChange := (path == "runtime" || path == "runtime/pprof") && symbol ||
					path == "testing" && name == "runtime.gopanic" ||
					selfPrefix || tablePrefix
				if (before != literal.Value) != shouldChange {
					t.Errorf("metadata literal %s -> %s, shouldChange=%v", before, literal.Value, shouldChange)
				}
				if (selfPrefix || tablePrefix) && literal.Value != strconv.Quote(lpkg.obfuscatedImportPath()+".") {
					t.Errorf("package prefix literal %s does not name the obfuscated path", literal.Value)
				}
			}
		})
	}
}
