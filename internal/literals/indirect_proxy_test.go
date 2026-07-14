package literals

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"testing"
)

func TestIndirectDecoderProxyInitSafety(t *testing.T) {
	file := &ast.File{Name: ast.NewIdent("p")}
	seq := 0
	obfRand := newObfRand(mathrand.New(mathrand.NewSource(1)), file, func(_ *mathrand.Rand, base string) string {
		seq++
		return fmt.Sprintf("%s_%d", base, seq)
	})
	// Keep the fixture compact and deterministic; decoder placement, rather
	// than algorithm selection, is what this test exercises.
	obfRand.testObfuscator = simple{}

	// Every string hides both its cast helper and decoder, so this count must
	// cross a proxy-root boundary even if no external key is hidden.
	count := maxProxyValuesPerRoot/2 + 1
	values := make([]ast.Expr, 0, count)
	for i := range count {
		call := obfuscateString(obfRand, fmt.Sprintf("secret-value-%04d", i))
		if _, ok := call.Fun.(*ast.SelectorExpr); !ok {
			t.Fatalf("literal %d decoder is not called through a proxy selector: %T", i, call.Fun)
		}
		values = append(values, call)
	}
	file.Decls = append(file.Decls, &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{
			Names: []*ast.Ident{ast.NewIdent("values")},
			Values: []ast.Expr{&ast.CompositeLit{
				Type: &ast.ArrayType{Elt: ast.NewIdent("string")},
				Elts: values,
			}},
		}},
	})
	file.Decls = append(file.Decls, obfRand.liftedFuncs...)

	if got := len(obfRand.proxyDispatcher.roots); got < 2 {
		t.Fatalf("proxy roots=%d, want at least 2", got)
	}
	rootNames := make(map[string]bool, len(obfRand.proxyDispatcher.roots))
	for _, root := range obfRand.proxyDispatcher.roots {
		rootNames[root.root.name] = true
	}
	for _, decl := range obfRand.liftedFuncs {
		fn := decl.(*ast.FuncDecl)
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if ok && rootNames[ident.Name] {
				t.Errorf("lifted function %s depends on proxy root %s", fn.Name.Name, ident.Name)
			}
			return true
		})
	}

	obfRand.proxyDispatcher.AddToFile(file)
	var source bytes.Buffer
	if err := format.Node(&source, token.NewFileSet(), file); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "indirect.go", source.Bytes(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&types.Config{}).Check("p", fset, []*ast.File{parsed}, nil); err != nil {
		t.Fatalf("generated package has an initialization dependency error: %v", err)
	}
}
