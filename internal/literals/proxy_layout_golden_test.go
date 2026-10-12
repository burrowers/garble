package literals

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func TestProxyLayoutCanonicalContent(t *testing.T) {
	var content bytes.Buffer
	for _, seed := range []int64{1, 2, 3} {
		count := 0
		d := newProxyDispatcher(rand.New(rand.NewSource(seed)), func(_ *rand.Rand, name string) string {
			count++
			return fmt.Sprintf("field%d", count)
		})
		for i := range 512 {
			if i%3 == 0 {
				d.HideValue(ast.NewIdent("decoder"), &ast.FuncType{Params: &ast.FieldList{}})
			} else {
				d.HideValue(ast.NewIdent(fmt.Sprintf("key%d", i)), ast.NewIdent("uint64"))
			}
		}
		file := &ast.File{Name: ast.NewIdent("p")}
		d.AddToFile(file)
		for _, decl := range file.Decls {
			gen := decl.(*ast.GenDecl)
			if gen.Tok != token.TYPE {
				continue
			}
			fields := gen.Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List
			slices.SortFunc(fields, func(a, b *ast.Field) int { return strings.Compare(a.Names[0].Name, b.Names[0].Name) })
		}
		if err := format.Node(&content, token.NewFileSet(), file); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&content, "next dispatcher random value: %d\n", d.rand.Int63())
	}
	got := fmt.Sprintf("%x", sha256.Sum256(content.Bytes()))
	// The digest was captured from upstream 9fb7029 after sorting only type
	// fields. Initializers, tree edges, pointer choices, keys, junk bytes and
	// the next primary RNG value must match that baseline exactly.
	const want = "cb268d1279458fb743e1ed64fda80c2e2147e7e49840640912db43c2ace52eee"
	if got != want {
		t.Fatalf("proxy content or dispatcher randomness changed: %s", got)
	}
}
