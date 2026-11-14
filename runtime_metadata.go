package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// transformRuntimeMetadataNames keeps runtime consumers in step with the
// linker's hashed function metadata. Package prefixes still carry runtime
// classification, but these declarations no longer need plaintext names.
// Rewrite before type checking so constant references and folded expressions
// use the same spelling as the source, including during literal obfuscation.
func transformRuntimeMetadataNames(lpkg *listedPackage, file *ast.File) {
	consumer := lpkg.baseImportPath()
	var root ast.Node = file
	switch consumer {
	case "runtime", "runtime/pprof", "testing":
	case "reflect":
		root = nil
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "valueMethodName" {
				root = fn.Body
				break
			}
		}
		if root == nil {
			return
		}
	default:
		return
	}
	ast.Inspect(root, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			panic(err)
		}
		switch consumer {
		case "testing":
			if value != "runtime.gopanic" {
				return true
			}
		case "reflect":
			if value != "reflect.Value." {
				return true
			}
			value = strings.TrimSuffix(value, ".")
		default:
			switch value {
			case "runtime.goexit", "runtime.gopanic", "reflect.makeFuncStub", "reflect.methodValueCall":
			default:
				return true
			}
		}
		path, name, _ := strings.Cut(value, ".")
		pkg, _ := sharedCache.ListedPackages.get(path)
		if pkg == nil {
			panic("runtime metadata package not listed: " + path)
		}
		value = path + "." + obfuscatedPackageObjectName(pkg, name)
		if consumer == "reflect" {
			value += "."
		}
		literal.Value = strconv.Quote(value)
		return true
	})
}
