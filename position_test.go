// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"
)

func TestGeneratedHeaderPreservesOriginalBodyPositions(t *testing.T) {
	oldSet := fset
	t.Cleanup(func() { fset = oldSet })
	fset = token.NewFileSet()
	file, err := parser.ParseFile(fset, "original.go", "package p\nfunc original() { println(1) }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	fn.Name.NamePos = token.NoPos
	fn.Type.Func = token.NoPos
	fn.Type.Params.Opening = token.NoPos
	fn.Type.Params.Closing = token.NoPos
	src, err := printFile(&listedPackage{ImportPath: "p", CompiledGoFiles: []string{"original.go"}}, file)
	if err != nil {
		t.Fatal(err)
	}
	// The body still points at its original line, even though the function
	// header around it has no position of its own.
	outSet := token.NewFileSet()
	output, err := parser.ParseFile(outSet, "", src, 0)
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	var call *ast.CallExpr
	for node := range ast.Preorder(output.Decls[0]) {
		if c, ok := node.(*ast.CallExpr); ok {
			call = c
			break
		}
	}
	if call == nil {
		t.Fatalf("missing call in printed source: %s", src)
	}
	if p := outSet.Position(call.Lparen); p.Filename == "" || p.Line != 2 {
		t.Fatalf("original body source position lost: %v\n%s", p, src)
	}
}

func TestGeneratedFunctionPositions(t *testing.T) {
	oldSet := fset
	t.Cleanup(func() { fset = oldSet })
	fset = token.NewFileSet()
	file, err := parser.ParseFile(fset, "original.go", "package p\nfunc original() { println(1) }\nfunc following() { println(2) }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := parser.ParseFile(token.NewFileSet(), "", "package p\nfunc opaque(x []byte) string {\n y := string(x)\n return y\n}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Match the positionless AST emitted by the literal helper generator.
	decl := generated.Decls[0].(*ast.FuncDecl)
	for node := range ast.Preorder(decl) {
		v := reflect.ValueOf(node).Elem()
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).Type() == reflect.TypeFor[token.Pos]() {
				v.Field(i).SetInt(0)
			}
		}
	}
	file.Decls = []ast.Decl{file.Decls[0], decl, file.Decls[1]}
	src, err := printFile(&listedPackage{ImportPath: "p", CompiledGoFiles: []string{"original.go"}}, file)
	if err != nil {
		t.Fatal(err)
	}
	outSet := token.NewFileSet()
	output, err := parser.ParseFile(outSet, "", src, 0)
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	helper := output.Decls[1].(*ast.FuncDecl)
	for node := range ast.Preorder(helper.Body) {
		p := outSet.Position(node.Pos())
		if p.Filename != "" || p.Line != 1 {
			t.Fatalf("artificial helper position %v\n%s", p, src)
		}
	}
	// Both original calls keep their own line; the generated helper above
	// carries no hashed filename at all.
	want := []int{2, 3}
	callIndex := 0
	for _, decl := range []ast.Decl{output.Decls[0], output.Decls[2]} {
		for node := range ast.Preorder(decl) {
			c, ok := node.(*ast.CallExpr)
			if !ok {
				continue
			}
			if callIndex >= len(want) {
				t.Fatalf("unexpected extra call in printed source: %s", src)
			}
			if p := outSet.Position(c.Lparen); p.Filename == "" || p.Line != want[callIndex] {
				t.Fatalf("original call position lost: %v\n%s", p, src)
			}
			callIndex++
		}
	}
	if callIndex != len(want) {
		t.Fatalf("original call position lost: %s", src)
	}
}
