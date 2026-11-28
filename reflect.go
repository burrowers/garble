package main

import (
	_ "embed"
	"go/token"
	"go/types"
	"iter"
	"log"
	"maps"
	"os"
	"slices"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// reflectSignatureKey encodes the parameters and results of a signature, so
// that a call whose callee is not statically known can be matched with the
// functions which are known to reflect on some of their parameters.
//
// Go requires a function value call and an interface method implementation to
// have the same signature as the target it is called through, so a signature
// carries the parameter positions over to every call site of that shape. The
// receiver and any parameter names are left out, since they don't affect the
// parameter positions.
func reflectSignatureKey(sig *types.Signature) string {
	if sig == nil {
		return ""
	}
	unnamedVars := func(tuple *types.Tuple) []*types.Var {
		vars := make([]*types.Var, tuple.Len())
		for i := range vars {
			vars[i] = types.NewVar(token.NoPos, nil, "", tuple.At(i).Type())
		}
		return vars
	}
	bare := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(unnamedVars(sig.Params())...),
		types.NewTuple(unnamedVars(sig.Results())...),
		sig.Variadic(),
	)
	return types.TypeString(bare, nil)
}

//go:embed reflect_abi_code.go
var reflectAbiCode string

func abiNamePatch(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	find := `return unsafe.String(n.DataChecked(1+i, "non-empty string"), l)`
	replace := `return _originalNames(unsafe.String(n.DataChecked(1+i, "non-empty string"), l))`

	str := strings.Replace(string(data), find, replace, 1)

	_, code, _ := strings.Cut(reflectAbiCode, "// Injected code below this line.")
	return str + code + `
//go:linkname _originalNameData go:garbleReflectNames
var _originalNameData string

func init() { _originalNamesInit(_originalNameData) }
`, nil
}

type reflectInspector struct {
	lpkg *listedPackage
	pkg  *types.Package

	propagatedInstr map[ssa.Instruction]bool
	seenEdges       map[reflectCallEdge]bool
	work            reflectWorklist
	// Function literals have no types.Func name to put in the package cache.
	localReflectAPIs map[*ssa.Function]map[int]bool
	// walkedInstantiations holds the instantiations whose members were already
	// walked, so repeated passes over the package do not repeat that work.
	walkedInstantiations map[types.Type]bool
	// walkedTypes holds the named types whose members were already recorded.
	// A package can declare several distinct types with one name: a
	// package-level type and a local one, or locals in different functions.
	// They share one obfuscated name, so a recorded name says nothing about
	// the members of another type object with that name; only the object
	// itself does.
	walkedTypes map[types.Object]bool
	// globalStores holds the values written into each package-level variable.
	// SSA does not track referrers of a global, so the initializing writes are
	// indexed up front; a reflected global exposes whatever was stored in it.
	globalStores map[*ssa.Global][]ssa.Value
	// closureBindings holds, for every anonymous function of the package, the
	// values the MakeClosure sites bind to its free variables. A FreeVar names
	// no operand, so this is the only route from a captured value back to the
	// value the enclosing function produced.
	closureBindings map[*ssa.Function][][]ssa.Value

	result pkgCache
}

// Record all instances of reflection use, and don't obfuscate types which are used in reflection.
func (ri *reflectInspector) recordReflection(ssaPkg *ssa.Package) {
	if reflectSkipPkg[ssaPkg.Pkg.Path()] {
		return
	}

	ri.indexGlobalStores(ssaPkg)
	ri.indexClosureBindings(ssaPkg)
	ri.seedReflectInternals()
	ri.solvePackageReflection(ssaPkg)
}

// indexGlobalStores records the values written into package-level variables.
// SSA does not maintain referrers for a *ssa.Global, so the writes have to be
// collected by walking the package's functions once.
func (ri *reflectInspector) indexGlobalStores(ssaPkg *ssa.Package) {
	if ri.globalStores != nil {
		return
	}
	ri.globalStores = make(map[*ssa.Global][]ssa.Value)
	var scan func(fn *ssa.Function)
	scan = func(fn *ssa.Function) {
		for _, anon := range fn.AnonFuncs {
			scan(anon)
		}
		for _, block := range fn.Blocks {
			for _, inst := range block.Instrs {
				// A map and a channel are reference types, so a write into a
				// package-level one never stores to the global's address and
				// would otherwise be invisible here.
				switch inst := inst.(type) {
				case *ssa.Store:
					if global := globalOfAddr(inst.Addr); global != nil {
						ri.globalStores[global] = append(ri.globalStores[global], inst.Val)
					}
				case *ssa.MapUpdate:
					if global := globalOfAddr(inst.Map); global != nil {
						ri.globalStores[global] = append(ri.globalStores[global], inst.Value)
					}
				case *ssa.Send:
					if global := globalOfAddr(inst.Chan); global != nil {
						ri.globalStores[global] = append(ri.globalStores[global], inst.X)
					}
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(ssaPkg.Members)) {
		if fn, ok := ssaPkg.Members[name].(*ssa.Function); ok {
			scan(fn)
		}
	}
}

// globalOfAddr returns the package-level variable an address belongs to,
// looking through the field and element selections which address its content.
func globalOfAddr(addr ssa.Value) *ssa.Global {
	switch addr := addr.(type) {
	case *ssa.Global:
		return addr
	case *ssa.FieldAddr:
		return globalOfAddr(addr.X)
	case *ssa.IndexAddr:
		return globalOfAddr(addr.X)
	case *ssa.ChangeType:
		return globalOfAddr(addr.X)
	case *ssa.UnOp:
		// A map or a channel is reached through a load of the variable which
		// holds it, so the writes into it name the load rather than the global.
		return globalOfAddr(addr.X)
	}
	return nil
}

// indexClosureBindings records which values each MakeClosure instruction binds
// to the free variables of the anonymous function it builds. A *ssa.FreeVar
// has no operand, so without this index a value read from a captured variable
// cannot be traced back to the value the enclosing function produced.
func (ri *reflectInspector) indexClosureBindings(ssaPkg *ssa.Package) {
	if ri.closureBindings != nil {
		return
	}
	ri.closureBindings = make(map[*ssa.Function][][]ssa.Value)
	var scan func(fn *ssa.Function)
	scan = func(fn *ssa.Function) {
		for _, anon := range fn.AnonFuncs {
			scan(anon)
		}
		for _, block := range fn.Blocks {
			for _, inst := range block.Instrs {
				mc, ok := inst.(*ssa.MakeClosure)
				if !ok {
					continue
				}
				closure, ok := mc.Fn.(*ssa.Function)
				if !ok {
					continue
				}
				ri.closureBindings[closure] = append(ri.closureBindings[closure], mc.Bindings)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(ssaPkg.Members)) {
		switch memb := ssaPkg.Members[name].(type) {
		case *ssa.Function:
			scan(memb)
		case *ssa.Type:
			// A method body is not a package member, so a closure created
			// there is not reached through Members and its bindings would be
			// indexed nowhere.
			for _, typ := range []types.Type{memb.Type(), types.NewPointer(memb.Type())} {
				for method := range ssaPkg.Prog.MethodSets.MethodSet(typ).Methods() {
					fn := ssaPkg.Prog.MethodValue(method)
					if fn == nil {
						fn = ssaPkg.Prog.FuncValue(method.Obj().(*types.Func))
					}
					if fn != nil {
						scan(fn)
					}
				}
			}
		}
	}
}

func (ri *reflectInspector) seedReflectInternals() {
	// Some packages reach into reflect internals, like go-spew.
	// It's not particularly right of them to do that,
	// and it's entirely unsupported, but try to accomodate for now.
	// At least it's enough to leave the rtype and Value types intact.
	if ri.pkg.Path() == "reflect" {
		scope := ri.pkg.Scope()
		ri.recursivelyRecordUsedForReflect(scope.Lookup("rtype").Type())
		ri.recursivelyRecordUsedForReflect(scope.Lookup("Value").Type())
	}
}

func reflectAnalysisSteps(ssaPkg *ssa.Package, function func(*ssa.Function), iface func(*types.Func)) {
	var walk func(*ssa.Function)
	walk = func(fun *ssa.Function) {
		for _, anon := range fun.AnonFuncs {
			walk(anon)
		}
		function(fun)
	}
	for _, membName := range slices.Sorted(maps.Keys(ssaPkg.Members)) {
		memb := ssaPkg.Members[membName]
		switch x := memb.(type) {
		case *ssa.Type:
			// Methods are not package members. Check both value and pointer
			// method sets to include all methods of this type.
			for _, typ := range []types.Type{x.Type(), types.NewPointer(x.Type())} {
				for method := range ssaPkg.Prog.MethodSets.MethodSet(typ).Methods() {
					obj := method.Obj().(*types.Func)
					// MethodValue returns nil for a method of a generic type,
					// even though its body was built with the receiver's type
					// parameters as the method's own. Such a method is not a
					// member, but FuncValue still finds it, and analyzing it is
					// the only way to learn what it reflects on.
					m := ssaPkg.Prog.MethodValue(method)
					if m == nil {
						m = ssaPkg.Prog.FuncValue(obj)
					}
					if m != nil {
						walk(m)
					} else {
						iface(obj)
					}
				}
			}

		case *ssa.Function:
			// these not only include top level functions, but also synthetic
			// functions like the initialization of global variables

			walk(x)
		}
	}
}

// Exported methods with unnamed structs as parameters may be "used" in interface declarations
// elsewhere, these interfaces will break if any method uses reflection on the same parameter.
//
// Therefore never obfuscate unnamed structs which are used as a method parameter
// and treat them like a parameter which is actually used in reflection.
//
// See "UnnamedStructMethod" in the reflect.txtar test for an example.
func (ri *reflectInspector) checkMethodSignature(reflectParams map[int]bool, sig *types.Signature) {
	if sig.Recv() == nil {
		return
	}

	for i := range sig.Params().Len() {
		if reflectParams[i] {
			continue
		}

		param := sig.Params().At(i)
		typ := param.Type()
		switch t := typ.(type) {
		case *types.Array:
			typ = t.Elem()
		case *types.Slice:
			typ = t.Elem()
		}
		if _, ok := typ.(*types.Struct); ok {
			reflectParams[i] = true
			ri.recursivelyRecordUsedForReflect(param.Type())
		}
	}
}

// Checks the signature of an interface method for potential reflection use.
func (ri *reflectInspector) checkInterfaceMethod(m *types.Func) {
	reflectParams := make(map[int]bool)
	methodName, _ := stripTypeArgs(reflectFuncName(m))

	maps.Copy(reflectParams, ri.result.ReflectAPIs[methodName])

	sig := m.Signature()
	if m.Exported() {
		ri.checkMethodSignature(reflectParams, sig)
	}

	if len(reflectParams) > 0 {
		ri.addNamedReflectParams(reflectAPIFact, methodName, reflectParams)
	}
}

func mergeReflectParams(apis map[string]map[int]bool, name string, params map[int]bool) {
	if apis[name] == nil {
		apis[name] = make(map[int]bool)
	}
	maps.Copy(apis[name], params)
}

func packageTypes(pkg *types.Package) iter.Seq[types.Type] {
	return func(yield func(types.Type) bool) {
		for _, name := range pkg.Scope().Names() {
			if obj, ok := pkg.Scope().Lookup(name).(*types.TypeName); ok && !yield(obj.Type()) {
				return
			}
		}
	}
}

func interfaceMethods(pkg *types.Package) iter.Seq2[*types.Interface, *types.Func] {
	return func(yield func(*types.Interface, *types.Func) bool) {
		for typ := range packageTypes(pkg) {
			if iface, ok := typ.Underlying().(*types.Interface); ok {
				for method := range iface.Methods() {
					if !yield(iface, method) {
						return
					}
				}
			}
		}
	}
}

// Imported interfaces can dispatch to a reflective method in this package.
func (ri *reflectInspector) recordImplementedInterfaceMethods(method *types.Func, params map[int]bool) {
	for _, pkg := range append([]*types.Package{ri.pkg}, ri.pkg.Imports()...) {
		for iface, im := range interfaceMethods(pkg) {
			if im.Name() == method.Name() && types.Implements(method.Signature().Recv().Type(), iface) {
				ri.addNamedReflectParams(reflectAPIFact, reflectFuncName(im), params)
			}
		}
	}
}

// A concrete type need not import the interface it implements; only the final
// build sees both packages and can match their merged reflection summaries.
func matchReflectedInterfaceMethods(pkg *types.Package, result *pkgCache) {
	targets := make(map[string]bool)
	for _, edge := range result.ReflectCallEdges {
		targets[edge.Callee] = true
	}
	if len(targets) == 0 {
		return
	}
	var packages []*types.Package
	seen := make(map[*types.Package]bool)
	var visit func(*types.Package)
	visit = func(p *types.Package) {
		if seen[p] {
			return
		}
		seen[p] = true
		packages = append(packages, p)
		for _, imp := range p.Imports() {
			visit(imp)
		}
	}
	visit(pkg)

	type interfaceMethod struct {
		iface  *types.Interface
		method *types.Func
	}
	interfaces := make(map[string][]interfaceMethod)
	for _, p := range packages {
		for iface, method := range interfaceMethods(p) {
			if targets[reflectFuncName(method)] {
				interfaces[method.Name()] = append(interfaces[method.Name()], interfaceMethod{iface, method})
			}
		}
	}
	if len(interfaces) == 0 {
		return
	}
	for _, p := range packages {
		for typ := range packageTypes(p) {
			if _, ok := typ.Underlying().(*types.Interface); ok {
				continue
			}
			receiver := types.NewPointer(typ)
			for selection := range types.NewMethodSet(receiver).Methods() {
				method := selection.Obj().(*types.Func)
				params := result.ReflectAPIs[reflectFuncName(method)]
				if len(params) == 0 {
					continue
				}
				for _, target := range interfaces[method.Name()] {
					if types.Implements(receiver, target.iface) {
						mergeReflectParams(result.ReflectAPIs, reflectFuncName(target.method), params)
					}
				}
			}
		}
	}
}

// localCallees resolves direct closures and closures assigned to a local
// function variable. A recursive closure must be stored in an allocation so
// it can refer to itself; StaticCallee does not resolve calls through that
// variable. Collect every possible assignment conservatively.
func localCallees(value ssa.Value) []*ssa.Function {
	if fn, ok := value.(*ssa.Function); ok {
		return []*ssa.Function{fn}
	}
	seen := make(map[ssa.Value]bool)
	functions := make(map[*ssa.Function]bool)
	var visit func(ssa.Value)
	visit = func(v ssa.Value) {
		if v == nil || seen[v] {
			return
		}
		seen[v] = true
		switch v := v.(type) {
		case *ssa.Function:
			functions[v] = true
		case *ssa.MakeClosure:
			visit(v.Fn)
		case *ssa.UnOp:
			visit(v.X)
		case *ssa.Alloc:
			if refs := v.Referrers(); refs != nil {
				for _, ref := range *refs {
					if store, ok := ref.(*ssa.Store); ok && store.Addr == v {
						visit(store.Val)
					}
				}
			}
		case *ssa.Phi:
			for _, edge := range v.Edges {
				visit(edge)
			}
		case *ssa.ChangeType:
			visit(v.X)
		}
	}
	visit(value)
	result := slices.Collect(maps.Keys(functions))
	slices.SortFunc(result, func(a, b *ssa.Function) int { return strings.Compare(a.String(), b.String()) })
	return result
}

// forwardedParameter follows simple SSA wrappers without marking any type as
// reflected. This preserves forwarding through packages whose callees only
// become known to use reflection later in the build.
func forwardedParameter(v ssa.Value) *ssa.Parameter {
	switch v := v.(type) {
	case *ssa.Parameter:
		return v
	case *ssa.Slice:
		return forwardedParameter(v.X)
	case *ssa.MakeInterface:
		return forwardedParameter(v.X)
	case *ssa.ChangeType:
		return forwardedParameter(v.X)
	case *ssa.ChangeInterface:
		return forwardedParameter(v.X)
	case *ssa.UnOp:
		return forwardedParameter(v.X)
	}
	return nil
}

func (ri *reflectInspector) recordForwardedCall(fun *ssa.Function, caller, callee string, args []ssa.Value, sig *types.Signature) {
	if sig == nil || len(args) < sig.Params().Len() {
		return
	}
	first := len(args) - sig.Params().Len()
	for i := range sig.Params().Len() {
		param := forwardedParameter(args[first+i])
		if param == nil {
			continue
		}
		pos := reflectParamPosition(fun, param)
		if pos < 0 {
			continue
		}
		edge := reflectCallEdge{Caller: caller, CallerParam: pos, Callee: callee, CalleeParam: i}
		if ri.seenEdges == nil {
			ri.seenEdges = make(map[reflectCallEdge]bool)
			for _, existing := range ri.result.ReflectCallEdges {
				ri.seenEdges[existing] = true
			}
		}
		if !ri.seenEdges[edge] {
			ri.seenEdges[edge] = true
			ri.result.ReflectCallEdges = append(ri.result.ReflectCallEdges, edge)
		}
	}
}

// reflectTypeForFunc is the standard library function whose result exposes the
// reflection information of its type argument. Unlike reflect.TypeOf it has no
// value whose static type could be recorded instead.
const reflectTypeForFunc = "reflect.TypeFor"

// typeForTypeArgs returns the type arguments a call passes to
// reflect.TypeFor, or nil when the call is not to that function. The call can
// be direct or through a function value, which has no static callee.
func typeForTypeArgs(call *ssa.CallCommon) []types.Type {
	matches := func(fn *ssa.Function) bool {
		obj, _ := fn.Object().(*types.Func)
		return obj != nil && obj.FullName() == reflectTypeForFunc
	}
	if callee := call.StaticCallee(); callee != nil && matches(callee) {
		return callee.TypeArgs()
	}
	if fn, ok := call.Value.(*ssa.Function); ok && matches(fn) {
		return fn.TypeArgs()
	}
	return nil
}

// recordReflectType records a type which reflection exposes from a call in fun,
// such as the type argument of reflect.TypeFor.
//
// A type which only exists as a type parameter cannot be recorded: nothing in
// the declaring package knows what instantiates it. Record the positions of the
// parameters it mentions for the callers to complete, and record the named
// types it spells out, which an instantiation keeps.
func (ri *reflectInspector) recordReflectType(fun *ssa.Function, typ types.Type) {
	ri.recursivelyRecordUsedForReflect(typ)
	for name, positions := range typeParamUses(fun, typ) {
		ri.addNamedReflectParams(reflectTypeParamFact, name, positions)
		if flagDebug {
			log.Printf("reflect: %s reflects type parameters of %s: %v", fun, name, positions)
		}
	}
}

// typeParamUses returns, per generic function in fun's enclosing chain, the
// positions of its type parameters which typ mentions. Call sites are keyed by
// the named function that declares them, so a closure reports its enclosing
// function's parameters.
func typeParamUses(fun *ssa.Function, typ types.Type) map[string]map[int]bool {
	uses := make(map[string]map[int]bool)
	seen := make(map[types.Type]bool)
	var walk func(types.Type)
	walk = func(t types.Type) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true

		switch t := t.(type) {
		case *types.TypeParam:
			name, pos, ok := declaringGenericFunction(fun, t)
			if !ok {
				return
			}
			if uses[name] == nil {
				uses[name] = make(map[int]bool)
			}
			uses[name][pos] = true

		case *types.Alias:
			walk(t.Rhs())

		case *types.Named:
			// The origin's members are recorded by the caller; only the type
			// arguments it substitutes can still mention a type parameter.
			if targs := t.TypeArgs(); targs != nil {
				for i := range targs.Len() {
					walk(targs.At(i))
				}
			}

		case *types.Map:
			walk(t.Key())
			walk(t.Elem())

		case *types.Signature:
			for _, tuple := range [...]*types.Tuple{t.Params(), t.Results()} {
				for i := range tuple.Len() {
					walk(tuple.At(i).Type())
				}
			}

		case *types.Struct:
			for i := range t.NumFields() {
				walk(t.Field(i).Type())
			}

		case *types.Interface:
			for i := range t.NumEmbeddeds() {
				walk(t.EmbeddedType(i))
			}

		case interface{ Elem() types.Type }:
			walk(t.Elem())
		}
	}
	walk(typ)
	return uses
}

// declaringGenericFunction finds the named function which declares tp among the
// type parameters of fun or one of its enclosing functions.
func declaringGenericFunction(fun *ssa.Function, tp *types.TypeParam) (string, int, bool) {
	for fn := fun; fn != nil; fn = fn.Parent() {
		origin := ssaFuncOrigin(fn)
		tparams := origin.TypeParams()
		if tparams == nil {
			continue
		}
		for i := range tparams.Len() {
			if tparams.At(i) != tp {
				continue
			}
			obj, _ := origin.Object().(*types.Func)
			if obj == nil {
				// A closure has no name a call site could look up, so keep
				// looking for the function that declares the parameter.
				continue
			}
			name, _ := stripTypeArgs(reflectFuncName(obj))
			return name, i, true
		}
	}
	return "", 0, false
}

// recordArgReflected finds the type(s) of a function argument, which is being used in reflection
// and excludes these types from obfuscation
// It also checks if this argument has any relation to a function parameter and returns it if found.
func (ri *reflectInspector) recordArgReflected(fun *ssa.Function, val ssa.Value, visited map[ssa.Value]bool) *ssa.Parameter {
	// make sure we visit every val only once, otherwise there will be infinite recursion
	if visited[val] {
		return nil
	}

	/* fmt.Printf("val: %v %T %v\n", val, val, val.Type()) */
	visited[val] = true

	// The static type is reflected even when the value's SSA origin is not
	// traceable (for example, indexing a slice returned by append).
	//
	// A type parameter is not a type anything can be recorded for: only a call
	// site knows what instantiates it, so record the positions the callers have
	// to complete instead, as for the type argument of reflect.TypeFor.
	ri.recordReflectType(fun, val.Type())

	// A channel of interface values hands the concrete types sent through it
	// to whoever reflects on a received value. The send need not be in this
	// function: it can sit behind a channel parameter or a closure.
	if _, ok := val.Type().(*types.Chan); ok {
		ri.recordChannelSends(fun, val, visited)
	}

	switch val := val.(type) {
	case *ssa.Phi:
		var param *ssa.Parameter
		for _, edge := range val.Edges {
			p := ri.recordArgReflected(fun, edge, visited)
			if param == nil {
				param = p
			}
		}
		return param
	case *ssa.IndexAddr:
		for _, ref := range *val.Referrers() {
			if store, ok := ref.(*ssa.Store); ok {
				ri.recordArgReflected(fun, store.Val, visited)
			}
		}
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.Slice:
		// Slicing a backing array or another slice keeps the same elements,
		// and their addresses are derived from this value, so the values
		// stored through its index are part of what reflection sees.
		ri.recordReferrersReflected(fun, val, visited)
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.Call:
		// A container whose body belongs to another package keeps the
		// interface values it was given, and whoever reflects on one of them
		// reads back the concrete type that was boxed at a call site here.
		// A sync.Map is the canonical case: Store and Load are both calls on
		// the same receiver, and only this package sees what went in.
		ri.recordContainerStores(fun, callReceiver(&val.Call), visited)
		// A returned slice can contain the input models after reordering.
		// Only follow same-typed arguments rather than tainting every input.
		if _, ok := val.Type().(*types.Slice); ok {
			for _, arg := range val.Call.Args {
				if types.Identical(arg.Type(), val.Type()) {
					if param := ri.recordArgReflected(fun, arg, visited); param != nil {
						return param
					}
				}
			}
		}
	case *ssa.MakeInterface:
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.UnOp:
		ri.recordReferrersReflected(fun, val, visited)
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.FieldAddr:
		// The field of a reflected value is part of that value, so a value
		// stored into the field is reachable through reflection. This is the
		// route a concrete type takes into an interface-typed field, which
		// the field's own static type does not spell out.
		ri.recordReferrersReflected(fun, val, visited)
		return ri.recordArgReflected(fun, val.X, visited)

	case *ssa.Alloc:
		/* fmt.Printf("recording val %v \n", *val.Referrers()) */
		ri.recursivelyRecordUsedForReflect(val.Type())
		ri.recordReferrersReflected(fun, val, visited)

		// relatedParam needs to revisit nodes so create an empty map
		visited := make(map[ssa.Value]bool)

		// check if the found alloc gets tainted by function parameters
		return relatedParam(val, visited)

	case *ssa.ChangeType:
		ri.recursivelyRecordUsedForReflect(val.X.Type())
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.ChangeInterface:
		// Converting a value to a wider interface keeps the same concrete
		// value, so a caller which marks this parameter reflected reaches it.
		ri.recursivelyRecordUsedForReflect(val.X.Type())
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.Lookup:
		// A map lookup returns the map's element type, so the concrete value
		// behind an interface element is only reachable through the map.
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.MakeMap:
		ri.recursivelyRecordUsedForReflect(val.Type())
		// The entries of a map are written by MapUpdate, which a reflected
		// lookup reads back as values of the element type.
		if refs := val.Referrers(); refs != nil {
			for _, ref := range *refs {
				if update, ok := ref.(*ssa.MapUpdate); ok && update.Map == val {
					ri.recordArgReflected(fun, update.Value, visited)
				}
			}
		}
	case *ssa.MakeChan:
		ri.recursivelyRecordUsedForReflect(val.Type())
	case *ssa.MakeSlice:
		// The elements of a slice are reachable through its index, so the
		// values stored into the addresses derived from it are part of what
		// reflection sees, as for a map's entries.
		ri.recursivelyRecordUsedForReflect(val.Type())
		ri.recordReferrersReflected(fun, val, visited)
	case *ssa.Const:
		ri.recursivelyRecordUsedForReflect(val.Type())
	case *ssa.Global:
		ri.recursivelyRecordUsedForReflect(val.Type())

		// A global's content is what reflection sees. SSA has no referrers for
		// a global, so follow the values the package's initialization writes
		// into it; a table of interface values is only reachable this way.
		for _, stored := range ri.globalStores[val] {
			ri.recordArgReflected(fun, stored, visited)
		}
	case *ssa.Parameter:
		// this only finds the parameters who want to be found,
		// otherwise relatedParam is used for more in depth analysis

		ri.recursivelyRecordUsedForReflect(val.Type())
		return val
	case *ssa.Extract:
		// A multi-value call returns a tuple, and each result is an Extract
		// which names no operand of its own. Follow the call so a container
		// lookup's element stays reachable, as for a single-value call.
		if tuple, ok := val.Tuple.(*ssa.Call); ok {
			ri.recordArgReflected(fun, tuple, visited)
		}
		// The values of a range over a map arrive as an Extract of the
		// iterator's Next instruction, which names no map of its own, so
		// follow the iterator back to the map being ranged over.
		if next, ok := val.Tuple.(*ssa.Next); ok && !next.IsString {
			return ri.recordArgReflected(fun, next.Iter, visited)
		}
	case *ssa.Range:
		// An iterator reads the map its range instruction was given, so the
		// values an iteration produces are the ones stored into that map.
		return ri.recordArgReflected(fun, val.X, visited)
	case *ssa.FreeVar:
		// A closure reads a captured variable through a FreeVar, which names
		// no operand of its own. Follow the MakeClosure sites which build this
		// function, so a value reflected inside a closure is traced back to
		// the producer in the enclosing function.
		for i, freeVar := range val.Parent().FreeVars {
			if freeVar != val {
				continue
			}
			for _, bindings := range ri.closureBindings[val.Parent()] {
				if i < len(bindings) {
					ri.recordArgReflected(fun, bindings[i], visited)
				}
			}
			break
		}
	}

	return nil
}

// callReceiver returns the receiver a call passes explicitly, or nil when the
// call has no receiver or binds it into a function value first.
func callReceiver(call *ssa.CallCommon) ssa.Value {
	sig := call.Signature()
	if sig == nil || sig.Recv() == nil || len(call.Args) == 0 {
		return nil
	}
	return call.Args[0]
}

// recordContainerStores records the interface values this package hands to a
// container a reflected value is read out of. The container's methods live in
// another package, so the walker can neither see what they store nor follow a
// load back to the store that filled the slot; the boxed types exist only at
// the call sites of this package, which are found through the shared receiver.
func (ri *reflectInspector) recordContainerStores(fun *ssa.Function, recv ssa.Value, visited map[ssa.Value]bool) {
	if recv == nil {
		return
	}
	refs := recv.Referrers()
	if refs == nil {
		return
	}
	for _, ref := range *refs {
		inst, ok := ref.(ssa.CallInstruction)
		if !ok {
			continue
		}
		call := inst.Common()
		if len(call.Args) == 0 || call.Args[0] != recv {
			continue
		}
		if sig := call.Signature(); sig == nil || sig.Recv() == nil {
			continue
		}
		for _, arg := range call.Args[1:] {
			if hasInterfaceData(arg.Type()) {
				ri.recordArgReflected(fun, arg, visited)
			}
		}
	}
}

// recordChannelSends records the values sent into a channel which reflection
// reads from. A channel value only refers to the sends of the function that
// made it: a send in another function of the package receives the channel as a
// parameter, and a send in a closure receives it as a free variable. Both still
// belong to this package, so the calls and the closure bindings which hand the
// channel on are followed here.
func (ri *reflectInspector) recordChannelSends(fun *ssa.Function, ch ssa.Value, visited map[ssa.Value]bool) {
	walked := make(map[ssa.Value]bool)
	var walk func(fun *ssa.Function, ch ssa.Value)
	// walkLoad follows a load of a captured variable, which a send takes as
	// its channel once the cell holds one.
	walkLoad := func(fun *ssa.Function, load *ssa.UnOp) {
		if load.Op != token.MUL {
			return
		}
		if _, ok := load.Type().(*types.Chan); ok {
			walk(fun, load)
		}
	}
	// walkBound follows the free variable a closure reads a bound value
	// through, such as the cell holding a captured channel.
	walkBound := func(closure *ssa.Function, bindings []ssa.Value, bound ssa.Value) {
		for i, binding := range bindings {
			if binding == bound && i < len(closure.FreeVars) {
				walk(closure, closure.FreeVars[i])
			}
		}
	}
	walk = func(fun *ssa.Function, ch ssa.Value) {
		// A function can pass the channel back to itself, directly or
		// through another one, and the parameter value would be the same.
		if walked[ch] {
			return
		}
		walked[ch] = true
		refs := ch.Referrers()
		if refs == nil {
			return
		}
		for _, ref := range *refs {
			switch ref := ref.(type) {
			case *ssa.Send:
				if ref.Chan == ch {
					ri.recordArgReflected(fun, ref.X, visited)
				}
			case *ssa.MakeClosure:
				if closure, ok := ref.Fn.(*ssa.Function); ok {
					walkBound(closure, ref.Bindings, ch)
				}
			case *ssa.UnOp:
				walkLoad(fun, ref)
			case *ssa.ChangeType:
				// Passing the channel where only one direction is allowed
				// converts it, and the send uses the converted value.
				if _, ok := ref.Type().(*types.Chan); ok {
					walk(fun, ref)
				}
			case *ssa.Store:
				// A captured variable lives in a cell, so the channel is
				// stored into it before a closure loads it again.
				if ref.Val == ch {
					if cell, ok := ref.Addr.(*ssa.Alloc); ok {
						walk(fun, cell)
					}
				}
			case *ssa.Alloc:
				// The cell of a captured variable: its loads reach the
				// send and its bindings hand the cell to the closure.
				cellRefs := ref.Referrers()
				if cellRefs == nil {
					continue
				}
				for _, cellRef := range *cellRefs {
					switch cellRef := cellRef.(type) {
					case *ssa.MakeClosure:
						if closure, ok := cellRef.Fn.(*ssa.Function); ok {
							walkBound(closure, cellRef.Bindings, ch)
						}
					case *ssa.UnOp:
						walkLoad(fun, cellRef)
					}
				}
			case ssa.CallInstruction:
				call := ref.Common()
				callee := call.StaticCallee()
				if callee == nil || callee.Blocks == nil ||
					callee.Pkg == nil || callee.Pkg.Pkg != ri.pkg {
					continue
				}
				for i, arg := range call.Args {
					if arg == ch && i < len(callee.Params) {
						walk(callee, callee.Params[i])
					}
				}
			}
		}
	}
	walk(fun, ch)
}

// recordReferrersReflected records the values derived from a reflected value,
// such as its fields or elements, and the values stored into it. Other
// referrers, like calls taking the value as an argument, produce values which
// are not part of the reflected one.
func (ri *reflectInspector) recordReferrersReflected(fun *ssa.Function, val ssa.Value, visited map[ssa.Value]bool) {
	refs := val.Referrers()
	if refs == nil {
		return
	}
	for _, ref := range *refs {
		switch ref := ref.(type) {
		case *ssa.FieldAddr, *ssa.IndexAddr, *ssa.Slice, *ssa.UnOp,
			*ssa.Phi, *ssa.ChangeType, *ssa.ChangeInterface, *ssa.MakeInterface:
			ri.recordArgReflected(fun, ref.(ssa.Value), visited)
		case *ssa.Store:
			// Writing into the value writes part of what reflection can see.
			// The stored value's static type may be an interface while its
			// concrete type is what reflection exposes, as in a table of
			// any values.
			ri.recordArgReflected(fun, ref.Val, visited)
		}
	}
}

// relatedParam checks if a route to a function parameter can be constructed
// from a ssa.Value, and returns the parameter if it found one.
func relatedParam(val ssa.Value, visited map[ssa.Value]bool) *ssa.Parameter {
	// every val should only be visited once to prevent infinite recursion
	if visited[val] {
		return nil
	}

	/* fmt.Printf("related val: %v %T %v\n", val, val, val.Type()) */

	visited[val] = true

	switch x := val.(type) {
	case *ssa.Parameter:
		// a parameter has been found
		return x
	case *ssa.UnOp:
		if param := relatedParam(x.X, visited); param != nil {
			return param
		}
	case *ssa.FieldAddr:
		/* fmt.Printf("addr: %v\n", x)
		fmt.Printf("addr.X: %v %T\n", x.X, x.X) */

		if param := relatedParam(x.X, visited); param != nil {
			return param
		}
	}

	refs := val.Referrers()
	if refs == nil {
		return nil
	}

	for _, ref := range *refs {
		/* fmt.Printf("ref: %v %T\n", ref, ref) */

		var param *ssa.Parameter
		switch ref := ref.(type) {
		case *ssa.FieldAddr:
			param = relatedParam(ref, visited)

		case *ssa.UnOp:
			param = relatedParam(ref, visited)

		case *ssa.Store:
			if param := relatedParam(ref.Val, visited); param != nil {
				return param
			}

			param = relatedParam(ref.Addr, visited)

		}

		if param != nil {
			return param
		}
	}
	return nil
}

// hasInterfaceData reports whether typ is an interface type or contains one, so
// that a value of typ can hold a concrete type which reflection exposes even
// though the reflecting call site only sees an interface.
//
// The walk is structural and bounded by the type graph, so a cyclic type stops
// at its first repetition.
func hasInterfaceData(t types.Type) bool {
	return hasInterfaceDataImpl(t, make(map[types.Type]bool))
}

func hasInterfaceDataImpl(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch t := t.(type) {
	case *types.Interface:
		return true
	case *types.Alias:
		return hasInterfaceDataImpl(t.Rhs(), seen)
	case *types.Named:
		return hasInterfaceDataImpl(t.Underlying(), seen)
	case *types.Pointer:
		return hasInterfaceDataImpl(t.Elem(), seen)
	case *types.Slice:
		return hasInterfaceDataImpl(t.Elem(), seen)
	case *types.Array:
		return hasInterfaceDataImpl(t.Elem(), seen)
	case *types.Chan:
		return hasInterfaceDataImpl(t.Elem(), seen)
	case *types.Map:
		return hasInterfaceDataImpl(t.Key(), seen) || hasInterfaceDataImpl(t.Elem(), seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if hasInterfaceDataImpl(t.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// recursivelyRecordUsedForReflect calls recordUsedForReflect on any named
// types and fields under typ.
//
// Named types and fields reachable via reflection are recorded.
// Foreign named types use the declaring package's hash salt, while fields use
// [hashWithStruct], which is consistent across packages.
func (ri *reflectInspector) recursivelyRecordUsedForReflect(t types.Type) {
	ri.recursivelyRecordUsedForReflectImpl(t, make(map[types.Type]bool))
}

func (ri *reflectInspector) recursivelyRecordUsedForReflectImpl(t types.Type, visited map[types.Type]bool) {
	if t == nil || visited[t] {
		return
	}
	visited[t] = true

	switch t := t.(type) {
	case *types.Alias:
		ri.recursivelyRecordUsedForReflectImpl(t.Rhs(), visited)

	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() == nil {
			return
		}
		if ri.walkedTypes[obj] {
			// This object's members were recorded already. A new instantiation
			// of the same origin still substitutes its own type arguments into
			// them, so only that part can still add anything.
			if t.TypeArgs().Len() > 0 && !ri.walkedInstantiations[t] {
				ri.walkedInstantiations[t] = true
				ri.recordInstantiationMembers(t, visited)
			}
			return
		}
		if ri.walkedTypes == nil {
			ri.walkedTypes = make(map[types.Object]bool)
		}
		ri.walkedTypes[obj] = true
		ri.recordUsedForReflect(obj, nil)
		for i := range t.NumMethods() {
			method := t.Method(i)
			if method.Exported() {
				ri.recordUsedForReflect(method, nil)
			}
		}
		// Match [computeFieldToStruct]: use the generic/origin struct, not an
		// instantiated underlying, so field identities line up with [hashWithStruct].
		ri.recursivelyRecordUsedForReflectImpl(t.Origin().Underlying(), visited)
		ri.walkedInstantiations[t] = true
		ri.recordInstantiationMembers(t, visited)

	case *types.Struct:
		for i := range t.NumFields() {
			field := t.Field(i)
			if field.Pkg() != nil {
				// Preserve every field on a struct reached via reflection, including
				// fields declared in other packages
				originField := field.Origin()
				ri.recordUsedForReflect(originField, t)
			}
			ri.recursivelyRecordUsedForReflectImpl(field.Type(), visited)
		}
	case *types.Interface:
		for i := range t.NumMethods() {
			ri.recordUsedForReflect(t.Method(i), nil)
		}
		// reflect.Type.Method and MethodByName expose the method set of an
		// interface type, including the methods it embeds, so the types their
		// signatures mention are reachable too. A defined interface reaches
		// this walk through [reflectInspector.recordMethodSignatures], but an
		// unnamed one has no type object to record and only appears here.
		for method := range t.Methods() {
			ri.recursivelyRecordUsedForReflectImpl(method.Type(), visited)
		}

	case *types.Map:
		// reflect.Type.Key and Elem both expose component types.
		ri.recursivelyRecordUsedForReflectImpl(t.Key(), visited)
		ri.recursivelyRecordUsedForReflectImpl(t.Elem(), visited)

	case *types.Signature:
		// reflect.Type.In and Out expose the parameter and result types.
		for _, tuple := range [...]*types.Tuple{t.Params(), t.Results()} {
			for i := range tuple.Len() {
				ri.recursivelyRecordUsedForReflectImpl(tuple.At(i).Type(), visited)
			}
		}

	case interface{ Elem() types.Type }:
		// Get past pointers, slices, etc.
		ri.recursivelyRecordUsedForReflectImpl(t.Elem(), visited)
	}
}

// recordInstantiationMembers records the members that a generic instantiation
// substitutes with its type arguments. Recording the origin type object covers
// neither them nor the field types of its own underlying struct, because the
// origin's method set only mentions the type parameters and its struct only
// holds them as field types.
func (ri *reflectInspector) recordInstantiationMembers(t *types.Named, visited map[types.Type]bool) {
	ri.recordMethodSignatures(t, visited)
	// The origin struct's fields are already recorded with the origin as their
	// parent, matching [hashWithStruct], so only the substituted field types are
	// left to walk.
	if st, ok := t.Underlying().(*types.Struct); ok {
		for i := range st.NumFields() {
			ri.recursivelyRecordUsedForReflectImpl(st.Field(i).Type(), visited)
		}
	}
}

// recordMethodSignatures recurses into the parameter and result types of every
// method in the named type's method set. Both the value and the pointer method
// set are walked, since Garble does not track whether the program reflects the
// type or a pointer to it. Recording a name does not make a method reachable,
// so Garble's dead-method elimination is unaffected.
func (ri *reflectInspector) recordMethodSignatures(t *types.Named, visited map[types.Type]bool) {
	for _, recv := range [...]types.Type{t, types.NewPointer(t)} {
		for method := range types.NewMethodSet(recv).Methods() {
			ri.recursivelyRecordUsedForReflectImpl(method.Obj().Type(), visited)
		}
	}
}

func (ri *reflectInspector) recordGenericMethodNames(t types.Type, seen map[types.Type]bool) {
	if t == nil || seen[t] {
		return
	}
	seen[t] = true
	switch t := t.(type) {
	case *types.Alias:
		ri.recordGenericMethodNames(t.Rhs(), seen)
	case *types.Named:
		for i := range t.NumMethods() {
			method := t.Method(i)
			if method.Exported() {
				ri.recordUsedForReflect(method, nil)
			}
			ri.recordGenericMethodNames(method.Type(), seen)
		}
		ri.recordGenericMethodNames(t.Underlying(), seen)
	case *types.Interface:
		for i := range t.NumMethods() {
			ri.recordUsedForReflect(t.Method(i), nil)
			ri.recordGenericMethodNames(t.Method(i).Type(), seen)
		}
	case *types.Struct:
		for i := range t.NumFields() {
			ri.recordGenericMethodNames(t.Field(i).Type(), seen)
		}
	case *types.Signature:
		for _, tuple := range []*types.Tuple{t.Params(), t.Results()} {
			for i := range tuple.Len() {
				ri.recordGenericMethodNames(tuple.At(i).Type(), seen)
			}
		}
	case *types.Map:
		ri.recordGenericMethodNames(t.Key(), seen)
		ri.recordGenericMethodNames(t.Elem(), seen)
	case interface{ Elem() types.Type }:
		ri.recordGenericMethodNames(t.Elem(), seen)
	}
}

// obfuscatedObjectName returns the obfuscated name of a types.Object,
// parent is needed to correctly get the obfuscated name of struct fields
func (ri *reflectInspector) obfuscatedObjectName(obj types.Object, parent *types.Struct) string {
	pkg := obj.Pkg()
	if pkg == nil {
		return "" // builtin objects have no package or obfuscated name
	}
	if pkg.Path() == "$ssa" {
		// go/ssa creates these types for analysis, not from source Garble can rename.
		// There is no listed package or obfuscated name to record for them.
		return ""
	}

	if v, ok := obj.(*types.Var); ok && parent != nil {
		return hashWithStruct(parent, v)
	}
	if fn, ok := obj.(*types.Func); ok && fn.Signature().Recv() != nil && fn.Exported() {
		return obfuscatedMethodName(fn.Name())
	}

	lpkg := ri.lpkg
	if pkg != ri.pkg {
		var ok bool
		lpkg, ok = sharedCache.ListedPackages.get(pkg.Path())
		if !ok {
			// A test binary may only list the test variant of the package it
			// links, while the reflected object names the bare import path.
			if variantPath := ri.lpkg.testVariantPath(pkg.Path()); variantPath != "" {
				lpkg, ok = sharedCache.ListedPackages.get(variantPath)
			}
		}
		if !ok {
			panic("missing listed package for foreign reflected object: " + pkg.Path())
		}
	}
	return obfuscatedPackageObjectName(lpkg, obj.Name())
}

// recordUsedForReflect records the objects whose names we cannot obfuscate due to reflection.
// We currently record named types and fields.
func (ri *reflectInspector) recordUsedForReflect(obj types.Object, parent *types.Struct) {
	obfName := ri.obfuscatedObjectName(obj, parent)
	if obfName == "" {
		return
	}
	if !ri.result.ReflectedObjects[obfName] {
		ri.result.ReflectedObjects[obfName] = true
		ri.work.changed(reflectFact{kind: reflectObjectFact, name: obfName})
	}
	ri.result.ReflectObjectNames[obfName] = obj.Name()
	if flagDebug {
		log.Printf("reflect: preserving object %s as %q -> %q", obj, obfName, obj.Name())
	}
}

// We only mark named objects, so this function looks for a named object
// corresponding to a type.
func typeToObj(typ types.Type) types.Object {
	switch t := typ.(type) {
	case *types.Named:
		return t.Obj()
	case *types.Struct:
		if t.NumFields() > 0 {
			return t.Field(0)
		}
	case interface{ Elem() types.Type }:
		return typeToObj(t.Elem())
	}
	return nil
}

// reflectFuncName returns the name a function is keyed by in the reflection
// records: [pkgCache.ReflectAPIs], [pkgCache.ReflectTypeParams] and the caller
// and callee names of [pkgCache.ReflectCallEdges].
//
// A package rebuilt for a test binary is typechecked under its listing key,
// which spells the import path as "path [path.test]", but cmd/go gives the
// compiler the plain path and every package that imports it names its objects
// by that plain path. A key built from the typechecked package must therefore
// be spelled with the plain path as well. A key that keeps the variant spelling
// is never found again by a caller in another package, so the arguments such a
// caller passes to it go unrecorded and their reflected names stay obfuscated.
func reflectFuncName(obj *types.Func) string {
	name := obj.FullName()
	pkg := obj.Pkg()
	if pkg == nil || sharedCache == nil {
		return name
	}
	path := pkg.Path()
	lpkg, ok := sharedCache.ListedPackages.get(path)
	if !ok {
		return name
	}
	plain := lpkg.baseImportPath()
	if plain == path {
		return name
	}
	// The variant spelling qualifies the function itself and every named type
	// in its signature, so replace all of them.
	return strings.ReplaceAll(name, path, plain)
}

// stripTypeArgs removes generic type arguments from instantiated names like:
// "main.F[main.T]" -> "main.F"
// "(*pkg.Type[go.shape.int]).Method" -> "(*pkg.Type).Method"
// The second return value indicates whether any type arguments were stripped.
func stripTypeArgs(name string) (string, bool) {
	if !strings.Contains(name, "[") {
		return name, false
	}
	var b strings.Builder
	b.Grow(len(name))
	depth := 0
	for _, r := range name {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
				continue
			}
			b.WriteRune(r)
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String(), true
}

func ssaFuncOrigin(fn *ssa.Function) *ssa.Function {
	if orig := fn.Origin(); orig != nil {
		return orig
	}
	return fn
}
