package literals

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// CheckSource parses src and type-checks it as the package at path, recording
// into info, and returns the parsed file with the file set its positions refer
// to. It is exported for the external test package.
func CheckSource(t *testing.T, path, filename, src string, mode parser.Mode, info *types.Info) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, mode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := new(types.Config).Check(path, fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	return file, fset
}

// useObfuscator forces obf for the package named pkgName until the test ends,
// so that a test can select the decoder family it asserts on.
func useObfuscator(t *testing.T, pkgName string, obf obfuscator) {
	t.Helper()
	previous := testPkgToObfuscatorMap
	t.Cleanup(func() { testPkgToObfuscatorMap = previous })
	testPkgToObfuscatorMap = map[string]obfuscator{pkgName: obf}
}
