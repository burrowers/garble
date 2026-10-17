package literals

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"math/rand"
	"strings"
	"testing"
)

func occupancyDispatcher(seed int64) *proxyDispatcher {
	return newProxyDispatcher(rand.New(rand.NewSource(seed)), func(r *rand.Rand, base string) string {
		return fmt.Sprintf("%s_%x", base, r.Uint64())
	})
}

func TestProxyHiddenValueIdentity(t *testing.T) {
	d := occupancyDispatcher(7)
	const count = 513
	values := make([]ast.Expr, count)
	paths := make([]*ast.SelectorExpr, count)
	fields := make([]*ast.Ident, count)
	file := &ast.File{Name: ast.NewIdent("p")}
	for i := range values {
		values[i] = &ast.BasicLit{Kind: token.INT, Value: fmt.Sprint(i)}
		paths[i] = d.HideValue(values[i], ast.NewIdent("int")).(*ast.SelectorExpr)
		fields[i] = paths[i].Sel
		withPos(paths[i], token.Pos(10))
		file.Decls = append(file.Decls, &ast.GenDecl{Tok: token.VAR, Specs: []ast.Spec{&ast.ValueSpec{
			Names: []*ast.Ident{ast.NewIdent(fmt.Sprintf("result%d", i))}, Values: []ast.Expr{paths[i]},
		}}})
	}
	d.AddToFile(file)
	for i, path := range paths {
		if path.Sel != fields[i] || path.Sel.Pos() != 10 {
			t.Fatal("selector field identity or position changed")
		}
		for node := range ast.Preorder(path) {
			if ident, ok := node.(*ast.Ident); ok && ident.Pos() != fields[i].Pos() {
				t.Fatalf("path identifier %s lost its source position", ident.Name)
			}
		}
		root := d.roots[i/maxProxyValuesPerRoot]
		var selectors []string
		var expr ast.Expr = path
		for {
			sel, ok := expr.(*ast.SelectorExpr)
			if !ok {
				break
			}
			selectors = append(selectors, sel.Sel.Name)
			expr = sel.X
		}
		if expr.(*ast.Ident).Name != root.root.name {
			t.Fatalf("value %d moved to a different root", i)
		}
		strct := root.root
		for j := len(selectors) - 1; j > 0; j-- {
			var child *proxyStruct
			for _, candidate := range strct.children {
				if candidate.name == selectors[j] {
					child = candidate
					break
				}
			}
			if child == nil {
				t.Fatalf("value %d has invalid child path", i)
			}
			strct = child
		}
		found := false
		for _, v := range strct.values {
			if v.name == selectors[0] {
				found = v.val == values[i]
				break
			}
		}
		if !found {
			t.Fatalf("value %d lost its identity", i)
		}
	}
	if _, err := new(types.Config).Check("p", token.NewFileSet(), []*ast.File{file}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestProxyClonedPlaceholderPaths(t *testing.T) {
	d := occupancyDispatcher(7)
	original := d.HideValue(&ast.BasicLit{Kind: token.INT, Value: "42"}, ast.NewIdent("int"))
	first := d.clonePath(original)
	second := d.clonePath(first)
	paths := []ast.Expr{original, first, second}
	file := &ast.File{Name: ast.NewIdent("p")}
	for i, path := range paths {
		withPos(path, token.Pos(10+i))
		file.Decls = append(file.Decls, &ast.GenDecl{Tok: token.VAR, Specs: []ast.Spec{&ast.ValueSpec{
			Names: []*ast.Ident{ast.NewIdent(fmt.Sprintf("result%d", i))}, Values: []ast.Expr{path},
		}}})
	}
	d.AddToFile(file)
	seen := make(map[ast.Node]bool)
	var expected string
	for i, path := range paths {
		var printed strings.Builder
		if err := printer.Fprint(&printed, token.NewFileSet(), path); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			expected = printed.String()
		} else if printed.String() != expected {
			t.Fatalf("clone path %s differs from %s", &printed, expected)
		}
		for node := range ast.Preorder(path) {
			if seen[node] {
				t.Fatal("clones share mutable AST nodes")
			}
			seen[node] = true
			if ident, ok := node.(*ast.Ident); ok && ident.Pos() != token.Pos(10+i) {
				t.Fatalf("path identifier %s lost its clone's position", ident.Name)
			}
		}
	}
	if _, err := new(types.Config).Check("p", token.NewFileSet(), []*ast.File{file}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestProxyPrintedDirective(t *testing.T) {
	const source = `package p

var value = 42

//go:noinline
func next() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	d := occupancyDispatcher(7)
	spec := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.ValueSpec)
	spec.Values[0] = withPos(d.HideValue(spec.Values[0], ast.NewIdent("int")), spec.Values[0].Pos()).(ast.Expr)
	d.AddToFile(file)
	var printed strings.Builder
	config := printer.Config{Mode: printer.RawFormat}
	if err := config.Fprint(&printed, fset, file); err != nil {
		t.Fatal(err)
	}
	reparsed, err := parser.ParseFile(token.NewFileSet(), "output.go", printed.String(), parser.ParseComments)
	if err != nil {
		t.Fatalf("invalid printed source: %v\n%s", err, &printed)
	}
	next := reparsed.Decls[1].(*ast.FuncDecl)
	if next.Doc == nil || len(next.Doc.List) != 1 || next.Doc.List[0].Text != "//go:noinline" {
		t.Fatalf("compiler directive detached from next function:\n%s", &printed)
	}
}

func TestProxyRandomLayout(t *testing.T) {
	layouts := make(map[string]bool)
	for seed := int64(0); seed < 16; seed++ {
		build := func() (string, string) {
			d := occupancyDispatcher(seed)
			var paths []ast.Expr
			for i := range 256 {
				paths = append(paths, d.HideValue(&ast.BasicLit{Kind: token.INT, Value: fmt.Sprint(i)}, ast.NewIdent("int")))
			}
			file := &ast.File{Name: ast.NewIdent("p")}
			d.AddToFile(file)
			var shape, source strings.Builder
			for _, s := range d.roots[0].flattenStructs {
				fmt.Fprintf(&shape, "%t/%d/%d;", s.isPointer, len(s.children), len(s.values))
			}
			for _, path := range paths {
				depth := 0
				for sel, ok := path.(*ast.SelectorExpr); ok; sel, ok = sel.X.(*ast.SelectorExpr) {
					depth++
				}
				fmt.Fprintf(&shape, "%d,", depth)
			}
			if err := printer.Fprint(&source, token.NewFileSet(), file); err != nil {
				t.Fatal(err)
			}
			return shape.String(), source.String()
		}
		shape, source := build()
		againShape, againSource := build()
		if shape != againShape || source != againSource {
			t.Fatal("fixed seed is not reproducible")
		}
		layouts[shape] = true
	}
	if len(layouts) < 2 {
		t.Fatal("different seeds produced identical layouts")
	}
}

func TestProxyOccupancy(t *testing.T) {
	for _, count := range []int{0, 1, 8, 64, 65, 128, 256, 257, 513} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d := occupancyDispatcher(1)
			for range count {
				d.HideValue(ast.NewIdent("value"), ast.NewIdent("int"))
			}
			file := &ast.File{Name: ast.NewIdent("p")}
			d.AddToFile(file)
			if count == 0 {
				if len(file.Decls) != 0 || len(d.roots) != 0 {
					t.Fatal("empty owner generated proxy declarations")
				}
				return
			}
			if len(d.roots) != (count+255)/256 {
				t.Fatalf("root count = %d", len(d.roots))
			}
			for _, root := range d.roots {
				n := len(root.flattenStructs)
				base := (root.hiddenCount + proxyValuesPerStruct - 1) / proxyValuesPerStruct
				if n < 1+base || n > 1+base+min(base, 2) {
					t.Errorf("occupancy %d uses %d structs outside its slack budget", root.hiddenCount, n)
				}
				if root.hiddenCount <= 8 && (n < 2 || n > 3) {
					t.Errorf("small occupancy %d uses %d structs; want 2..3", root.hiddenCount, n)
				}
				if root.hiddenCount == 256 && (n < 5 || n > 7) {
					t.Errorf("full root uses %d structs; want 5..7", n)
				}
				if root.hiddenCount > 256 {
					t.Fatal("root occupancy exceeded bound")
				}
				for _, s := range root.flattenStructs {
					junk := 0
					for _, v := range s.values {
						if _, ok := v.val.(*ast.CompositeLit); ok {
							junk++
						}
					}
					if junk == 0 {
						t.Error("struct has no junk fields")
					}
				}
			}
		})
	}
}
