// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

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

	"github.com/rogpeppe/go-internal/cache"
)

func objectNamesTransformer(t *testing.T, path, source string) *transformer {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "names.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := new(types.Config).Check(path, fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lpkg := &listedPackage{ImportPath: path, Name: pkg.Name(), CompiledGoFiles: []string{"names.go"}, GarbleActionID: [32]byte{1}}
	pkgs := newListedPackages()
	pkgs.set(path, lpkg)
	old := sharedCache
	sharedCache = &sharedCacheType{ListedPackages: pkgs}
	t.Cleanup(func() { sharedCache = old })
	return &transformer{curPkg: lpkg, pkg: pkg}
}

func TestObfuscatedCoverageNames(t *testing.T) {
	for _, path := range []string{"example/names", "example/names [example/names.test]"} {
		t.Run(path, func(t *testing.T) {
			tf := objectNamesTransformer(t, path, `package names
var goCover_M = [1]byte{1}
var goCover_P uint32
var goCover_0 [4]uint32
var goCover_1 [4]uint32
`)
			tf.coverageNames.MetaVar = "goCover_M"
			tf.coverageNames.PkgIdVar = "goCover_P"
			tf.coverageNames.CounterPrefix = "goCover_"
			for _, name := range tf.pkg.Scope().Names() {
				got, ok := tf.obfuscatedObjectName(tf.pkg.Scope().Lookup(name))
				if !ok || got == name || got == "" {
					t.Errorf("coverage variable %s: got (%q, %v)", name, got, ok)
				}
			}
			dir := t.TempDir()
			oldTemp := sharedTempDir
			sharedTempDir = dir
			t.Cleanup(func() { sharedTempDir = oldTemp })
			original := `{"MetaVar":"goCover_M","PkgIdVar":"goCover_P","CounterPrefix":"goCover_","CounterMode":"atomic","MetaLen":1,"UnknownSetting":[1,2]}`
			path := filepath.Join(dir, "original.json")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			flags, err := tf.transformCoverageConfig([]string{"-coveragecfg", path})
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(flagValue(flags, "-coveragecfg"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]json.RawMessage
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			if string(cfg["UnknownSetting"]) != "[1,2]" || string(cfg["CounterMode"]) != `"atomic"` || string(cfg["MetaLen"]) != "1" {
				t.Fatalf("lost coverage settings: %s", data)
			}
			for key, name := range map[string]string{"MetaVar": "goCover_M", "PkgIdVar": "goCover_P"} {
				got, _ := tf.obfuscatedObjectName(tf.pkg.Scope().Lookup(name))
				var configured string
				if err := json.Unmarshal(cfg[key], &configured); err != nil {
					t.Fatal(err)
				}
				if got != configured {
					t.Errorf("%s declaration = %q, compiler expects %q", key, got, configured)
				}
			}
			var prefix string
			if err := json.Unmarshal(cfg["CounterPrefix"], &prefix); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"goCover_0", "goCover_1"} {
				got, _ := tf.obfuscatedObjectName(tf.pkg.Scope().Lookup(name))
				if got != tf.coverageCounterName(name) || !strings.HasPrefix(got, prefix) {
					t.Errorf("counter %q does not match compiler prefix %q", got, prefix)
				}
			}
			meta := tf.pkg.Scope().Lookup("goCover_M").(*types.Var)
			if !tf.staticLiteralInits()[meta] {
				t.Fatal("coverage metadata lost its static initializer protection")
			}
		})
	}
}

func TestObfuscatedAssemblyHeaderConstants(t *testing.T) {
	tf := objectNamesTransformer(t, "example/names", `package names
const Exported = 42
const private = Exported + 1
`)
	fsCache, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldCache, oldTemp := openCache, sharedTempDir
	openCache = func() (*cache.Cache, error) { return fsCache, nil }
	sharedTempDir = t.TempDir()
	t.Cleanup(func() { openCache, sharedTempDir = oldCache, oldTemp })
	if err := tf.saveGoAsmNames(); err != nil {
		t.Fatal(err)
	}
	nameMap := loadGoAsmNames(tf.curPkg)
	for _, name := range []string{"Exported", "private"} {
		got, ok := tf.obfuscatedObjectName(tf.pkg.Scope().Lookup(name))
		want := hashWithPackage(tf.curPkg, name)
		if !ok || got != want {
			t.Errorf("constant %s: got (%q, %v), want %q", name, got, ok, want)
		}
		if got := nameMap["const_"+name]; got != "const_"+want {
			t.Errorf("assembly constant %s = %q, want %q", name, got, "const_"+want)
		}
	}
	dir := filepath.Join(sharedTempDir, tf.curPkg.obfuscatedSourceDir())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	asmPath := filepath.Join(dir, hashWithPackage(tf.curPkg, "asm.s")+".s")
	headerPath := filepath.Join(dir, "garbled_constants.h")
	for path, data := range map[string]string{asmPath: "#include \"garbled_constants.h\"\nMOVQ $const_Exported, AX\n", headerPath: "#define HEADER_CONST const_Exported\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tf.transformAsm([]string{"asm.s"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{asmPath, headerPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := "const_" + hashWithPackage(tf.curPkg, "Exported")
		if !strings.Contains(string(data), want) || strings.Contains(string(data), "const_Exported") {
			t.Errorf("assembly/header constant not mapped in %s: %s", filepath.Base(path), data)
		}
	}
}
