package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
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
		for _, name := range []string{"Error"} {
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
	names := compilerMethodNames(t, info)
	if names["CrossPackageMethod"] != obfuscatedMethodName("CrossPackageMethod") {
		t.Fatalf("missing evaluated method constant: %v", names)
	}
	if _, exists := names["not a method"]; exists {
		t.Fatal("invalid method name recorded")
	}
}

func compilerMethodNames(t *testing.T, info *types.Info) map[string]string {
	t.Helper()
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
	return names
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

// A covered build compiles cmd/cover's rewritten copy of reflect/type.go, where
// the counter of a block is prepended to the statement it instruments. The
// patch has to apply to that shape as well, and it has to leave the counter in
// place rather than cut it off with the replaced method search.
func TestReflectMethodsPatchCoveredSource(t *testing.T) {
	const src = `package reflect

func (t *rtype) Method(i int) (Method, bool) {
	p := methods[i]
	pname := t.nameOff(p.Name)
	_ = pname
	return Method{}, false
}

func (t *abiType) ExportedMethod(i int) (Method, bool) {
	p := &t.Methods[i]
	pname := t.nameOff(p.Name)
	_ = pname
	return Method{}, false
}

func (t *rtype) MethodByName(name string) (Method, bool) {
	methods := t.ExportedMethods()
	// We are looking for the first index i where the string becomes >= s.
	i, j := 0, len(methods)
	_, _ = i, j
	if i < len(methods) && name == t.nameOff(methods[i].Name).Name() {
		return t.Method(i), true
	}

	goCover_909ca0096d51__72[8] = 1;return Method{}, false
}

func (t *abiType) MethodByName(name string) (Method, bool) {
	i := t.methodIndex(name)
	if name == "" {
		return t.Method(i), true
		}
	return Method{}, false
}
`
	path := filepath.Join(t.TempDir(), "type.cover.go")
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := reflectMethodsPatch(path, "type.go")
	if err != nil {
		t.Fatalf("covered source rejected: %v", err)
	}
	for _, want := range []string{
		"goCover_909ca0096d51__72[8] = 1;return Method{}, false",
		"methods[_garbleMethodIndex(t, i)]",
		"t.Methods[_garbleMethodIndex((*rtype)(unsafe.Pointer(t)), i)]",
		"return t.Method(_garbleMethodRank(t, i)), true",
		"func _garbleMethodRank(t *rtype, raw int) int",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("patched covered source is missing %q:\n%s", want, got)
		}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), path, got, 0); err != nil {
		t.Fatalf("patched covered source does not parse: %v\n%s", err, got)
	}
}
