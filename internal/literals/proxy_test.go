package literals

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"
	"slices"
	"testing"
)

func TestProxyLayoutPadding(t *testing.T) {
	// These fields have enough interchangeable orders to pack without fixing
	// either the decoder or key to a particular offset.
	var total int64
	for seed := int64(0); seed < 64; seed++ {
		counter := 0
		d := newProxyDispatcher(rand.New(rand.NewSource(seed)), func(_ *rand.Rand, name string) string {
			counter++
			return fmt.Sprintf("%s%d", name, counter)
		})
		root := &proxyStruct{name: "root", typeName: "Root", values: []*proxyValue{
			{name: "small", typ: ast.NewIdent("uint8"), val: ast.NewIdent("small")},
			{name: "key", typ: ast.NewIdent("uint64"), val: ast.NewIdent("key")},
			{name: "decode", typ: &ast.FuncType{Params: &ast.FieldList{}}, val: ast.NewIdent("decode")},
			{name: "medium", typ: ast.NewIdent("uint16"), val: ast.NewIdent("medium")},
		}}
		d.roots = []*proxyRoot{{root: root, flattenStructs: []*proxyStruct{root}}}
		file := &ast.File{Name: ast.NewIdent("p")}
		d.AddToFile(file)
		// Only type declarations are needed to measure the layout.
		file.Decls = file.Decls[:len(file.Decls)-1]
		pkg, err := new(types.Config).Check("p", token.NewFileSet(), []*ast.File{file}, nil)
		if err != nil {
			t.Fatal(err)
		}
		total += types.SizesFor("gc", "amd64").Sizeof(pkg.Scope().Lookup("Root").Type())
	}
	// Random ordering on the parent uses 2344 bytes. Retain every field but
	// select lower-padding random candidates instead.
	if total > 2100 {
		t.Fatalf("proxy layout padding remains high: %d bytes across 64 seeds", total)
	}
	t.Logf("64-seed proxy bytes: %d", total)
}

func TestProxyLayoutPreservesFields(t *testing.T) {
	const source = `package p
 type Root struct { a byte; child Child; decode func(); p *Child; key uint64; junk [7]byte }
 type Child struct { x byte; f func(uint32) []byte; y uint16; z [3]byte; k uint64 }
 `
	arches := []string{"386", "amd64", "amd64p32", "arm", "arm64", "loong64", "mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le", "riscv64", "s390x", "sparc64", "wasm"}
	orders := make(map[string]bool)
	positions := make(map[int]bool)
	var saved int64
	for seed := int64(0); seed < 64; seed++ {
		file, err := parser.ParseFile(token.NewFileSet(), "", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewSource(seed))
		originalFields := make(map[*ast.Field]bool)
		for _, decl := range file.Decls {
			fields := decl.(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List
			rng.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
			for _, f := range fields {
				originalFields[f] = true
			}
		}
		before := proxyTestSizes(t, file, arches)
		var serialized bytes.Buffer
		if err := format.Node(&serialized, token.NewFileSet(), file); err != nil {
			t.Fatal(err)
		}
		repeat, err := parser.ParseFile(token.NewFileSet(), "", serialized.Bytes(), 0)
		if err != nil {
			t.Fatal(err)
		}
		compactProxyLayouts(file.Decls)
		compactProxyLayouts(repeat.Decls)
		serialized.Reset()
		var repeated bytes.Buffer
		format.Node(&serialized, token.NewFileSet(), file)
		format.Node(&repeated, token.NewFileSet(), repeat)
		if !bytes.Equal(serialized.Bytes(), repeated.Bytes()) {
			t.Fatal("same layout is not reproducible")
		}
		after := proxyTestSizes(t, file, arches)
		for i := range before {
			if after[i] > before[i] {
				t.Fatalf("seed %d: layout grew on %s: %d > %d", seed, arches[i/2], after[i], before[i])
			}
			saved += before[i] - after[i]
		}
		for _, decl := range file.Decls {
			fields := decl.(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List
			for _, f := range fields {
				if !originalFields[f] {
					t.Fatal("field replaced or duplicated")
				}
				delete(originalFields, f)
			}
		}
		if len(originalFields) != 0 {
			t.Fatal("field removed")
		}
		fields := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List
		var names []string
		for i, f := range fields {
			names = append(names, f.Names[0].Name)
			if f.Names[0].Name == "decode" {
				positions[i] = true
			}
		}
		orders[fmt.Sprint(names)] = true
	}
	if saved == 0 {
		t.Fatal("no padding removed")
	}
	if len(orders) < 20 || len(positions) < 3 {
		t.Fatalf("layout diversity too low: %d orders, %d decoder positions", len(orders), len(positions))
	}
	t.Logf("64 seeds: %d distinct root orders, %d decoder positions", len(orders), len(positions))
}

func proxyTestSizes(t *testing.T, file *ast.File, arches []string) []int64 {
	t.Helper()
	fset := token.NewFileSet()
	fset.AddFile("proxy.go", -1, int(file.End()))
	pkg, err := new(types.Config).Check("p", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result []int64
	for _, arch := range arches {
		sizes := types.SizesFor("gc", arch)
		for _, name := range []string{"Root", "Child"} {
			result = append(result, sizes.Sizeof(pkg.Scope().Lookup(name).Type()))
		}
	}
	return result
}

func TestProxyLayoutUnknownType(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "", "package p; type Root struct { a byte; unknown Missing; b uint64 }", 0)
	if err != nil {
		t.Fatal(err)
	}
	fields := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields
	before := slices.Clone(fields.List)
	compactProxyLayouts(file.Decls)
	if !slices.Equal(before, fields.List) {
		t.Fatal("unknown type changed layout")
	}
}
