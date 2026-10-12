package literals

import (
	"go/ast"
	"go/token"
	"go/types"
	"hash/fnv"
	"math/rand"
)

// compactProxyLayouts samples field orders rather than sorting by alignment.
// The original shuffle still consumes exactly the same dispatcher randomness,
// so topology, pointer choices, hidden values and junk remain unchanged.
func compactProxyLayouts(decls []ast.Decl) {
	file := &ast.File{Name: ast.NewIdent("proxy"), Decls: decls}
	info := &types.Info{Defs: make(map[*ast.Ident]types.Object)}
	fset := token.NewFileSet()
	fset.AddFile("proxy.go", -1, int(file.End()))
	if _, err := new(types.Config).Check("proxy", fset, []*ast.File{file}, info); err != nil {
		return // Leave unfamiliar generated types alone.
	}
	structs := make(map[string]*ast.TypeSpec)
	for _, decl := range decls {
		spec := decl.(*ast.GenDecl).Specs[0].(*ast.TypeSpec)
		structs[spec.Name.Name] = spec
	}
	// These cover gc's word-size/max-alignment combinations, including 32-bit
	// targets where uint64 aligns to four bytes and amd64p32's eight-byte alignment.
	sizes := []types.Sizes{types.SizesFor("gc", "386"), types.SizesFor("gc", "amd64"), types.SizesFor("gc", "amd64p32")}
	done := make(map[string]bool)
	var visit func(*ast.TypeSpec)
	visit = func(spec *ast.TypeSpec) {
		if done[spec.Name.Name] {
			return
		}
		done[spec.Name.Name] = true
		fields := spec.Type.(*ast.StructType).Fields.List
		for _, field := range fields {
			typ := field.Type
			if ptr, ok := typ.(*ast.StarExpr); ok {
				typ = ptr.X
			}
			if id, ok := typ.(*ast.Ident); ok && structs[id.Name] != nil {
				visit(structs[id.Name])
			}
		}
		named := info.Defs[spec.Name].Type().(*types.Named)
		original := named.Underlying().(*types.Struct)
		vars := make([]*types.Var, original.NumFields())
		for i := range vars {
			vars[i] = original.Field(i)
		}
		hash := fnv.New64a()
		for _, field := range fields {
			hash.Write([]byte(field.Names[0].Name))
			hash.Write([]byte{0})
		}
		rng := rand.New(rand.NewSource(int64(hash.Sum64())))
		best := make([]int64, len(sizes))
		for i, size := range sizes {
			best[i] = size.Sizeof(types.NewStruct(vars, nil))
		}
		for range 48 {
			order := rng.Perm(len(fields))
			candidateVars := make([]*types.Var, len(vars))
			for i, j := range order {
				candidateVars[i] = vars[j]
			}
			candidate := types.NewStruct(candidateVars, nil)
			smaller, larger := false, false
			for i, size := range sizes {
				n := size.Sizeof(candidate)
				smaller = smaller || n < best[i]
				larger = larger || n > best[i]
			}
			if !smaller || larger {
				continue
			}
			candidateFields := make([]*ast.Field, len(fields))
			for i, j := range order {
				candidateFields[i] = fields[j]
			}
			fields, vars = candidateFields, candidateVars
			for i, size := range sizes {
				best[i] = size.Sizeof(candidate)
			}
		}
		spec.Type.(*ast.StructType).Fields.List = fields
		named.SetUnderlying(types.NewStruct(vars, nil))
	}
	for _, decl := range decls {
		visit(decl.(*ast.GenDecl).Specs[0].(*ast.TypeSpec))
	}
}
