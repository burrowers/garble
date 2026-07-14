// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func TestStripRuntimeFatalMessageText(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", `package runtime
func f(name string) {
	throw("literal diagnostic")
	throw(name)
	throw("prefix " + name)
	throw("before " + sideEffect() + " after")
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	stripRuntime("synthetic.go", file, nil)

	var args []ast.Expr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if ok && id.Name == "throw" {
			args = append(args, call.Args[0])
			return false
		}
		return true
	})
	if len(args) != 4 {
		t.Fatalf("found %d throw arguments, want 4", len(args))
	}
	if literal, ok := args[0].(*ast.BasicLit); !ok || literal.Kind != token.STRING || literal.Value != `""` {
		t.Fatalf("direct literal was not blanked: %#v", args[0])
	}

	for i, arg := range args[1:] {
		slice, ok := arg.(*ast.SliceExpr)
		if !ok {
			t.Fatalf("computed argument %d is %T, want *ast.SliceExpr", i+1, arg)
		}
		high, ok := slice.High.(*ast.BasicLit)
		if !ok || high.Kind != token.INT || high.Value != "0" {
			t.Fatalf("computed argument %d does not use [:0]", i+1)
		}
		ast.Inspect(slice.X, func(node ast.Node) bool {
			if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING && literal.Value != `""` {
				t.Errorf("computed argument %d retained static fragment %s", i+1, literal.Value)
			}
			return true
		})
	}

	var sawName, sawSideEffect bool
	ast.Inspect(args[2], func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == "name" {
			sawName = true
		}
		return true
	})
	ast.Inspect(args[3], func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "sideEffect" {
				sawSideEffect = true
			}
		}
		return true
	})
	if !sawName || !sawSideEffect {
		t.Fatalf("computed evaluation was lost: name=%v sideEffect=%v", sawName, sawSideEffect)
	}
}

func TestStripRuntimeLeavesTestFatalHelpersAlone(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic_test.go", `package runtime
func testHelper() { fatal("test-owned diagnostic") }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	stripRuntime("synthetic_test.go", file, nil)

	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if ok && literal.Kind == token.STRING && literal.Value == `"test-owned diagnostic"` {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("runtime test helper diagnostic was unexpectedly changed")
	}
}

func TestStripRuntimeLinkedFatalPackages(t *testing.T) {
	for importPath, callNames := range runtimeFatalCallNames {
		t.Run(importPath, func(t *testing.T) {
			var source strings.Builder
			source.WriteString("package synthetic\nfunc f(message string, size int) {\n")
			if importPath == "internal/runtime/cgroup" {
				source.WriteString("println(\"runtime: cgroup buffer length\", len(message), \"want\", size)\n")
			}
			for _, callName := range callNames {
				source.WriteString(callName + "(\"diagnostic prefix: \" + message)\n")
			}
			source.WriteString("}\n")

			basename := "synthetic.go"
			if importPath == "internal/runtime/cgroup" {
				basename = "cgroup_linux.go"
			}
			file, err := parser.ParseFile(token.NewFileSet(), basename, source.String(), 0)
			if err != nil {
				t.Fatal(err)
			}
			result := stripRuntimeDependency(importPath, basename, file, nil)
			if result.fatalMessages != len(callNames) {
				t.Fatalf("stripped %d fatal messages, want %d", result.fatalMessages, len(callNames))
			}
			if importPath == "internal/runtime/cgroup" && result.cgroupPrints != 1 {
				t.Fatalf("stripped %d cgroup prints, want 1", result.cgroupPrints)
			}

			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if ok && literal.Kind == token.STRING {
					if strings.Contains(literal.Value, "diagnostic prefix") || strings.Contains(literal.Value, "cgroup buffer") {
						t.Errorf("diagnostic string survived: %s", literal.Value)
					}
				}
				return true
			})
		})
	}
}

func TestStripCgroupFatalPrintPreservesConcreteEvaluation(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "cgroup_linux.go", `package cgroup
func f(s []byte, size int) {
	println("runtime: cgroup buffer length", len(s), "want", size)
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := stripCgroupFatalPrint("cgroup_linux.go", file); got != 1 {
		t.Fatalf("stripped %d calls, want 1", got)
	}

	var replacement *ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if _, ok := call.Fun.(*ast.FuncLit); ok {
				replacement = call
				return false
			}
		}
		return true
	})
	if replacement == nil || len(replacement.Args) != 4 {
		t.Fatalf("missing four-argument no-op replacement: %#v", replacement)
	}
	if call, ok := replacement.Args[1].(*ast.CallExpr); !ok {
		t.Fatalf("len(s) evaluation was lost: %T", replacement.Args[1])
	} else if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "len" {
		t.Fatalf("len(s) evaluation changed: %#v", call.Fun)
	}
}

func TestStripFatalMessageTextIgnoresLocalShadow(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", `package synthetic
func fatal(string)
func packageCall() { fatal("package diagnostic") }
func localCall() {
	fatal := func(string) {}
	fatal("local diagnostic")
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
	}
	if _, err := (&types.Config{}).Check("synthetic", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	if got := stripFatalMessageText("synthetic.go", file, info, "fatal"); got != 1 {
		t.Fatalf("stripped %d calls, want only the package-level call", got)
	}

	var foundPackage, foundLocal bool
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		foundPackage = foundPackage || literal.Value == `"package diagnostic"`
		foundLocal = foundLocal || literal.Value == `"local diagnostic"`
		return true
	})
	if foundPackage || !foundLocal {
		t.Fatalf("package/local diagnostics after stripping: package=%v local=%v", foundPackage, foundLocal)
	}
}
