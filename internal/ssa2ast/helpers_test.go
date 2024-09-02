package ssa2ast

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"

	"github.com/google/go-cmp/cmp/cmpopts"
)

//lint:ignore SA1019 we need to mention go/ast.Object here to ignore the fields with its type.
var astCmpOpt = cmpopts.IgnoreTypes(token.NoPos, &ast.Object{})

func findStruct(file *ast.File, structName string) (name *ast.Ident, structType *ast.StructType) {
	ast.Inspect(file, func(node ast.Node) bool {
		if structType != nil {
			return false
		}

		typeSpec, ok := node.(*ast.TypeSpec)
		if !ok || typeSpec.Name == nil || typeSpec.Name.Name != structName {
			return true
		}
		typ, ok := typeSpec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		structType = typ
		name = typeSpec.Name
		return true
	})

	if structType == nil {
		panic(structName + " not found")
	}
	return
}

func findFunc(file *ast.File, funcName string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fDecl, ok := decl.(*ast.FuncDecl)
		if ok && fDecl.Name.Name == funcName {
			return fDecl
		}
	}
	panic(funcName + " not found")
}

func mustParseAndTypeCheckFile(src string) (*ast.File, *token.FileSet, *types.Info, *types.Package) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "a.go", src, 0)
	if err != nil {
		panic(err)
	}

	config := types.Config{Importer: importer.Default()}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Instances:  make(map[*ast.Ident]types.Instance),
		Implicits:  make(map[ast.Node]types.Object),
		Scopes:     make(map[ast.Node]*types.Scope),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	pkg, err := config.Check("test/main", fset, []*ast.File{f}, info)
	if err != nil {
		panic(err)
	}
	return f, fset, info, pkg
}
