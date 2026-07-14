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

	ah "mvdan.cc/garble/internal/asthelper"
)

func TestProxyPoolBound(t *testing.T) {
	seq := 0
	d := newProxyDispatcher(mathrand.New(mathrand.NewSource(1)), func(_ *mathrand.Rand, base string) string {
		seq++
		return fmt.Sprintf("%s_%d", base, seq)
	})
	for i := range 2*maxProxyValuesPerRoot + 1 {
		d.HideValue(ah.IntLit(i), ast.NewIdent("uint64"))
	}

	if got, want := len(d.roots), 3; got != want {
		t.Fatalf("roots=%d, want %d", got, want)
	}
	wantCounts := []int{maxProxyValuesPerRoot, maxProxyValuesPerRoot, 1}
	for i, root := range d.roots {
		if got := root.hiddenCount; got != wantCounts[i] {
			t.Fatalf("root %d hiddenCount=%d, want %d", i, got, wantCounts[i])
		}
	}

	file := &ast.File{Name: ast.NewIdent("p")}
	d.AddToFile(file)
	var roots, maxFields int
	for _, decl := range file.Decls {
		gen := decl.(*ast.GenDecl)
		switch gen.Tok {
		case token.VAR:
			roots++
		case token.TYPE:
			st := gen.Specs[0].(*ast.TypeSpec).Type.(*ast.StructType)
			maxFields = max(maxFields, len(st.Fields.List))
		}
	}
	if roots != len(d.roots) {
		t.Fatalf("root vars=%d, want %d", roots, len(d.roots))
	}
	if maxFields > maxProxyValuesPerRoot+maxJunkValueCount+maxChildCount {
		t.Fatalf("max fields=%d, want <=%d", maxFields, maxProxyValuesPerRoot+maxJunkValueCount+maxChildCount)
	}

	var src bytes.Buffer
	if err := format.Node(&src, token.NewFileSet(), file); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "pool.go", src.Bytes(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&types.Config{}).Check("p", fset, []*ast.File{parsed}, nil); err != nil {
		t.Fatal(err)
	}
}
