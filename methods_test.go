package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"
)

func TestExportedMethodNamesAcrossPackages(t *testing.T) {
	oldCache, oldSeed := sharedCache, flagSeed
	t.Cleanup(func() { sharedCache, flagSeed = oldCache, oldSeed })
	sharedCache = &sharedCacheType{BinaryContentID: []byte("method naming test")}
	for _, seed := range []string{"", "FX4AAAAAAAA"} {
		flagSeed.bytes = nil
		if seed != "" {
			if err := flagSeed.Set(seed); err != nil {
				t.Fatal(err)
			}
		}
		one := &listedPackage{ImportPath: "test/one", CompiledGoFiles: []string{"one.go"}}
		two := &listedPackage{ImportPath: "test/two", CompiledGoFiles: []string{"two.go"}}
		one.GarbleActionID[0], two.GarbleActionID[0] = 1, 2
		got := methodName(one, "CrossPackageMethod")
		if got == "CrossPackageMethod" || !token.IsExported(got) || got != methodName(two, "CrossPackageMethod") {
			t.Fatalf("seed %q: exported method identity mismatch: %q / %q", seed, got, methodName(two, "CrossPackageMethod"))
		}
		if methodName(one, "sealed") == methodName(two, "sealed") {
			t.Fatal("unexported method package identity lost")
		}
		for _, name := range []string{"Error", "Method", "MethodByName", "Write", "Sum"} {
			if methodName(one, name) != name || methodName(two, name) != name {
				t.Fatalf("required method %q was renamed", name)
			}
		}
	}
}

func TestCompilerMethodNames(t *testing.T) {
	oldCache, oldTemp := sharedCache, sharedTempDir
	t.Cleanup(func() { sharedCache, sharedTempDir = oldCache, oldTemp })
	sharedCache = &sharedCacheType{BinaryContentID: []byte("method names")}
	sharedTempDir = t.TempDir()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", `package p; const name = "Cross" + "PackageMethod"; var _ = name; var _ = "not a method"`, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	if _, err := new(types.Config).Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	path, err := writeCompilerMethodNames(info)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var names map[string]string
	if err := json.Unmarshal(data, &names); err != nil {
		t.Fatal(err)
	}
	if names["CrossPackageMethod"] != obfuscatedMethodName("CrossPackageMethod") {
		t.Fatalf("missing evaluated method constant: %v", names)
	}
	if _, exists := names["not a method"]; exists {
		t.Fatal("invalid method name recorded")
	}
}

func TestReflectMethodsPatchRejectsChangedSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "type.go")
	if err := os.WriteFile(path, []byte("package reflect"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reflectMethodsPatch(path, "type.go"); err == nil {
		t.Fatal("unrecognized reflection source accepted")
	}
}
