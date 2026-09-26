package main

import (
	"go/token"
	"go/types"
	"testing"
)

func TestDeferredInterfaceFirstMethodParameter(t *testing.T) {
	api := types.NewPackage("example/api", "api")
	impl := types.NewPackage("example/impl", "impl")
	mainPkg := types.NewPackage("example/main", "main")
	mainPkg.SetImports([]*types.Package{api, impl})

	param := func(pkg *types.Package) *types.Tuple {
		return types.NewTuple(
			types.NewVar(token.NoPos, pkg, "unrelated", types.Typ[types.String]),
			types.NewVar(token.NoPos, pkg, "model", types.Typ[types.String]),
		)
	}
	interfaceMethod := types.NewFunc(token.NoPos, api, "Inspect",
		types.NewSignatureType(nil, nil, nil, param(api), nil, false))
	iface := types.NewInterfaceType([]*types.Func{interfaceMethod}, nil).Complete()
	inspector := types.NewNamed(types.NewTypeName(token.NoPos, api, "Inspector", nil), iface, nil)
	api.Scope().Insert(inspector.Obj())

	worker := types.NewNamed(types.NewTypeName(token.NoPos, impl, "Worker", nil),
		types.NewStruct(nil, nil), nil)
	impl.Scope().Insert(worker.Obj())
	method := types.NewFunc(token.NoPos, impl, "Inspect",
		types.NewSignatureType(types.NewVar(token.NoPos, impl, "recv", worker), nil, nil,
			param(impl), nil, false))
	worker.AddMethod(method)

	for _, reflectedParam := range []int{0, 1} {
		result := pkgCache{
			ReflectAPIs:      map[string]map[int]bool{method.FullName(): {reflectedParam: true}},
			ReflectCallEdges: []reflectCallEdge{{Callee: interfaceMethod.FullName()}},
		}
		matchReflectedInterfaceMethods(mainPkg, &result)
		for pos := range 2 {
			if got := result.ReflectAPIs[interfaceMethod.FullName()][pos]; got != (pos == reflectedParam) {
				t.Errorf("reflected method parameter %d: interface parameter %d = %t", reflectedParam, pos, got)
			}
		}
	}
}
