package main

import (
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
)

// runtimePackagePrefixes lists the string literals with which a
// standard-library package compares stack frame function names against its own
// import path. A frame names its package by the obfuscated import path, so the
// comparison only keeps working once it uses that path too. Only the exact
// literal is rewritten: neighbouring messages such as "net/http: " keep the
// path they print for the user. Standard-library packages are listed one by
// one because some of them, such as runtime and reflect, keep the plain path
// in their symbol names for toolchain reasons, so a single rule over the
// obfuscated import path does not hold for all of them.
var runtimePackagePrefixes = map[string][]string{
	"net/http": {"net/http."},
}

// transformRuntimeMetadataNames keeps runtime consumers in step with the
// linker's hashed function metadata. Package prefixes still carry runtime
// classification, but these declarations no longer need plaintext names.
// Rewrite before type checking so constant references and folded expressions
// use the same spelling as the source, including during literal obfuscation.
func transformRuntimeMetadataNames(lpkg *listedPackage, file *ast.File) {
	consumer := lpkg.baseImportPath()
	prefixes := runtimePackagePrefixes[consumer]
	// Packages outside the standard library always carry their obfuscated
	// import path, so a frame filter which compares against their own path
	// follows that path directly. Package main keeps the plain "main" prefix
	// in both builds and has nothing to rewrite, unless it is a main package
	// compiled into a test binary, which is imported and therefore hashed.
	selfPrefix := ""
	if !lpkg.Standard && (lpkg.Name != "main" || lpkg.ForTest != "") {
		selfPrefix = consumer + "."
	}
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
		if selfPrefix == "" && len(prefixes) == 0 {
			return
		}
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
		if (selfPrefix != "" && value == selfPrefix) || slices.Contains(prefixes, value) {
			literal.Value = strconv.Quote(lpkg.obfuscatedImportPath() + ".")
			return true
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
		case "runtime", "runtime/pprof":
			switch value {
			case "runtime.goexit", "runtime.gopanic", "reflect.makeFuncStub", "reflect.methodValueCall":
			default:
				return true
			}
		default:
			return true
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
