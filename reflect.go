package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/types"
	"log"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
)

//go:embed reflect_abi_code.go
var reflectAbiCode string
var reflectPatchFile = ""

func abiNamePatch(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	find := `return unsafe.String(n.DataChecked(1+i, "non-empty string"), l)`
	replace := `return _originalNames(unsafe.String(n.DataChecked(1+i, "non-empty string"), l))`

	str := strings.Replace(string(data), find, replace, 1)

	originalNames := `
//go:linkname _originalNames
func _originalNames(name string) string

//go:linkname _originalNamesInit
func _originalNamesInit()

func init() { _originalNamesInit() }
`

	return str + originalNames, nil
}

// reflectMainPrePatch adds the initial empty name mapping and _originalNames implementation
// to a file in the main package. The name mapping will be populated later after
// analyzing the main package, since we need to know all obfuscated names that need mapping.
// We split this into pre/post steps so that all variable names in the generated code
// can be properly obfuscated - if we added the filled map directly, the obfuscated names
// would appear as plain strings in the binary.
func reflectMainPrePatch(path string) (string, error) {
	if reflectPatchFile != "" {
		// already patched another file in main
		return "", nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	_, code, _ := strings.Cut(reflectAbiCode, "// Injected code below this line.")
	code = strings.ReplaceAll(code, "//disabledgo:", "//go:")
	// This constant is declared in our hash.go file.
	code = strings.ReplaceAll(code, "minHashLength", strconv.Itoa(minHashLength))
	return string(content) + code, nil
}

// reflectMainPostPatch populates the name mapping with the final obfuscated->real name
// mappings after all packages have been analyzed.
func reflectMainPostPatch(file []byte, lpkg *listedPackage, pkg pkgCache) []byte {
	// A generated test main can remain unobfuscated. Match the name chosen
	// by source transformation rather than assuming it was always hashed.
	obfVarName := obfuscatedPackageObjectName(lpkg, "_originalNamePairs")
	namePairs := fmt.Appendf(nil, "%s = []string{", obfVarName)

	keys := slices.Sorted(maps.Keys(pkg.ReflectObjectNames))
	namePairsFilled := bytes.Clone(namePairs)
	for _, obf := range keys {
		namePairsFilled = fmt.Appendf(namePairsFilled, "%q, %q,", obf, pkg.ReflectObjectNames[obf])
	}

	return bytes.Replace(file, namePairs, namePairsFilled, 1)
}

type reflectInspector struct {
	lpkg *listedPackage
	pkg  *types.Package

	propagatedInstr map[ssa.Instruction]bool
	seenEdges       map[reflectCallEdge]bool
	// Function literals have no types.Func name to put in the package cache.
	localReflectAPIs map[*ssa.Function]map[int]bool

	result pkgCache
}

// Record all instances of reflection use, and don't obfuscate types which are used in reflection.
func (ri *reflectInspector) recordReflection(ssaPkg *ssa.Package) {
	if reflectSkipPkg[ssaPkg.Pkg.Path()] {
		return
	}

	// One pass only propagates reflection use to the functions analyzed after
	// the ones it learns from, so a chain of reflect-tainted calls can need one
	// pass per link. Repeat until a pass records nothing new.
	for {
		prev := ri.recordedCount()
		ri.ignoreReflectedTypes(ssaPkg)
		if ri.recordedCount() == prev {
			return
		}
	}
}

// recordedCount measures how much has been recorded so far. Recording only ever
// adds, so [reflectInspector.recordReflection] is done once this stops growing.
// Note that reflected parameters count too: an API keeps taking new ones as
// more of its callees are found to use reflection.
func (ri *reflectInspector) recordedCount() int {
	n := len(ri.result.ReflectObjectNames)
	for _, params := range ri.result.ReflectAPIs {
		n += 1 + len(params)
	}
	for _, params := range ri.localReflectAPIs {
		n += 1 + len(params)
	}
	return n
}

// find all functions, methods and interface declarations of a package and record their
// reflection use
func (ri *reflectInspector) ignoreReflectedTypes(ssaPkg *ssa.Package) {
	// Some packages reach into reflect internals, like go-spew.
	// It's not particularly right of them to do that,
	// and it's entirely unsupported, but try to accomodate for now.
	// At least it's enough to leave the rtype and Value types intact.
	if ri.pkg.Path() == "reflect" {
		scope := ri.pkg.Scope()
		ri.recursivelyRecordUsedForReflect(scope.Lookup("rtype").Type())
		ri.recursivelyRecordUsedForReflect(scope.Lookup("Value").Type())
	}

	// Iterate in a stable order; ssa.Package.Members is a map, and the order
	// functions are analyzed in decides how many passes a chain of
	// reflect-tainted calls needs to settle.
	for _, membName := range slices.Sorted(maps.Keys(ssaPkg.Members)) {
		memb := ssaPkg.Members[membName]
		switch x := memb.(type) {
		case *ssa.Type:
			// methods aren't package members only their reciever types are
			// so some logic is required to find the methods a type has

			method := func(mset *types.MethodSet) {
				for at := range mset.Methods() {
					if m := ssaPkg.Prog.MethodValue(at); m != nil {
						ri.checkFunction(m)
						ri.checkAnonFunctions(m)
					} else {
						m := at.Obj().(*types.Func)
						// handle interface declarations
						ri.checkInterfaceMethod(m)
					}
				}
			}

			// yes, finding all methods really only works with both calls
			mset := ssaPkg.Prog.MethodSets.MethodSet(x.Type())
			method(mset)

			mset = ssaPkg.Prog.MethodSets.MethodSet(types.NewPointer(x.Type()))
			method(mset)

		case *ssa.Function:
			// these not only include top level functions, but also synthetic
			// functions like the initialization of global variables

			ri.checkFunction(x)
			ri.checkAnonFunctions(x)
		}
	}
}

func (ri *reflectInspector) checkAnonFunctions(fun *ssa.Function) {
	for _, anon := range fun.AnonFuncs {
		ri.checkFunction(anon)
		ri.checkAnonFunctions(anon)
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

	i := 0
	for param := range sig.Params().Variables() {
		if reflectParams[i] {
			i++
			continue
		}

		ignore := false
		switch x := param.Type().(type) {
		case *types.Struct:
			ignore = true
		case *types.Array:
			if _, ok := x.Elem().(*types.Struct); ok {
				ignore = true
			}
		case *types.Slice:
			if _, ok := x.Elem().(*types.Struct); ok {
				ignore = true
			}
		}

		if ignore {
			reflectParams[i] = true
			ri.recursivelyRecordUsedForReflect(param.Type())
		}
		i++
	}
}

// Checks the signature of an interface method for potential reflection use.
func (ri *reflectInspector) checkInterfaceMethod(m *types.Func) {
	reflectParams := make(map[int]bool)
	methodName, _ := stripTypeArgs(m.FullName())

	maps.Copy(reflectParams, ri.result.ReflectAPIs[methodName])

	sig := m.Signature()
	if m.Exported() {
		ri.checkMethodSignature(reflectParams, sig)
	}

	if len(reflectParams) > 0 {
		ri.result.ReflectAPIs[methodName] = reflectParams

		/* fmt.Printf("curPkgCache.ReflectAPIs: %v\n", curPkgCache.ReflectAPIs) */
	}
}

// Checks all callsites in a function declaration for use of reflection.
func (ri *reflectInspector) checkFunction(fun *ssa.Function) {
	// if fun != nil && fun.Synthetic != "loaded from gc object file" {
	// 	// fun.WriteTo crashes otherwise
	// 	fun.WriteTo(os.Stdout)
	// }

	f, _ := ssaFuncOrigin(fun).Object().(*types.Func)
	var funcName string
	genericFunc := false
	if f != nil {
		funcName, genericFunc = stripTypeArgs(f.FullName())
	}

	reflectParams := make(map[int]bool)
	// A template can invoke methods on reflected data. If such a method
	// returns an interface, its concrete result type is invisible at the
	// template.Execute call and must be found at the method's return.
	reflectedReceiver := false
	if recv := fun.Signature.Recv(); recv != nil && f != nil && f.Exported() {
		if obj := typeToObj(recv.Type()); obj != nil {
			reflectedReceiver = ri.usedForReflect(obj)
		}
	}
	if funcName != "" {
		maps.Copy(reflectParams, ri.result.ReflectAPIs[funcName])

		if f.Exported() {
			ri.checkMethodSignature(reflectParams, fun.Signature)
		}
	} else {
		maps.Copy(reflectParams, ri.localReflectAPIs[fun])
	}

	// fmt.Printf("f: %v\n", f)
	// fmt.Printf("fun: %v\n", fun)

	for _, block := range fun.Blocks {
		for _, inst := range block.Instrs {
			if ri.propagatedInstr[inst] {
				// Already propagated in an earlier pass; the rest of the block
				// may still hold calls to APIs discovered since then.
				continue
			}

			// fmt.Printf("inst: %v, t: %T\n", inst, inst)
			switch inst := inst.(type) {
			case *ssa.Return:
				if reflectedReceiver {
					for _, result := range inst.Results {
						if _, ok := result.Type().Underlying().(*types.Interface); ok {
							ri.recordArgReflected(result, make(map[ssa.Value]bool))
						}
					}
				}
			case *ssa.Store:
				obj := typeToObj(inst.Addr.Type())
				if obj != nil && ri.usedForReflect(obj) {
					ri.recordArgReflected(inst.Val, make(map[ssa.Value]bool))
					ri.propagatedInstr[inst] = true
				}
			case *ssa.ChangeType:
				obj := typeToObj(inst.X.Type())
				if obj != nil && ri.usedForReflect(obj) {
					ri.recursivelyRecordUsedForReflect(inst.Type())
					ri.propagatedInstr[inst] = true
				}
			case *ssa.Call:
				callName := ""
				if callee := inst.Call.StaticCallee(); callee != nil {
					if obj, ok := ssaFuncOrigin(callee).Object().(*types.Func); ok && obj != nil {
						callName = obj.FullName()
					}
				}
				if callName == "" && inst.Call.Method != nil {
					callName = inst.Call.Method.FullName()
				}
				if callName == "" {
					callName = inst.Call.Value.String()
				}
				rawCallName := callName
				callName, genericCall := stripTypeArgs(callName)
				if flagDebug && genericCall {
					log.Printf("reflect: normalized call %q to %q", rawCallName, callName)
				}

				/* fmt.Printf("callName: %v\n", callName) */

				if funcName != "" && ((inst.Call.StaticCallee() != nil && inst.Call.StaticCallee().Object() != nil) || inst.Call.Method != nil) {
					ri.recordForwardedCall(fun, funcName, callName, inst.Call.Args, inst.Call.Signature())
				}
				// record each call argument passed to a function parameter which is used in reflection
				knownParams := ri.result.ReflectAPIs[callName]
				for _, callee := range localCallees(inst.Call.Value) {
					if params := ri.localReflectAPIs[callee]; len(params) > 0 {
						knownParams = maps.Clone(knownParams)
						if knownParams == nil {
							knownParams = make(map[int]bool)
						}
						maps.Copy(knownParams, params)
					}
				}
				// Iterate in a stable order too, like the member loop above.
				for _, knownParam := range slices.Sorted(maps.Keys(knownParams)) {
					sig := inst.Call.Signature()
					if sig == nil {
						continue
					}
					// SSA call arguments can include synthetic leading values
					// before the declared parameters. Use the signature to find
					// where the real parameters start.
					//
					// Example for method M(x):
					//   - direct call `t.M(x)` often has Call.Args = [t, x]
					//   - bound call  `f := t.M; f(x)` has Call.Args = [x]
					// In both cases Params().Len() == 1, so firstParamArg is
					// len(Args)-1, and parameter x resolves correctly.
					firstParamArg := len(inst.Call.Args) - sig.Params().Len()
					argPos := firstParamArg + knownParam
					if argPos < 0 || argPos >= len(inst.Call.Args) {
						continue
					}

					arg := inst.Call.Args[argPos]
					/* fmt.Printf("flagging arg: %v\n", arg) */

					reflectedParam := ri.recordArgReflected(arg, make(map[ssa.Value]bool))
					if reflectedParam == nil {
						continue
					}

					pos := slices.Index(fun.Params, reflectedParam)
					if genericFunc {
						// Generic functions may include synthetic parameters.
						extra := len(fun.Params) - fun.Signature.Params().Len()
						if extra > 0 {
							pos -= extra
						}
					}
					if pos < 0 {
						continue
					}

					/* fmt.Printf("recorded param: %v func: %v\n", pos, fun) */

					reflectParams[pos] = true
					if fun.Signature.Recv() != nil && pos > 0 {
						// Methods may be called with or without the receiver in
						// Call.Args depending on SSA form. Record both indexes.
						reflectParams[pos-1] = true
					}
					if flagDebug {
						log.Printf("reflect: %s marks param %d reflected via %s argument %T", fun, pos, callName, arg)
					}
				}
			}
		}
	}

	if len(reflectParams) > 0 {
		if funcName == "" {
			if ri.localReflectAPIs == nil {
				ri.localReflectAPIs = make(map[*ssa.Function]map[int]bool)
			}
			ri.localReflectAPIs[fun] = reflectParams
			return
		}
		ri.result.ReflectAPIs[funcName] = reflectParams
		if f != nil && fun.Signature.Recv() != nil {
			ri.recordImplementedInterfaceMethods(f, reflectParams)
		}
		if flagDebug {
			log.Printf("reflect: function %s has reflected params %v", funcName, reflectParams)
		}

		/* fmt.Printf("curPkgCache.ReflectAPIs: %v\n", curPkgCache.ReflectAPIs) */
	}
}

// recordImplementedInterfaceMethods carries reflection use from a concrete method
// to interface dispatches, including interfaces declared in an imported package.
func (ri *reflectInspector) recordImplementedInterfaceMethods(method *types.Func, params map[int]bool) {
	recv := method.Signature().Recv().Type()
	for _, pkg := range append([]*types.Package{ri.pkg}, ri.pkg.Imports()...) {
		for _, name := range pkg.Scope().Names() {
			obj, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			iface, ok := obj.Type().Underlying().(*types.Interface)
			if !ok || !types.Implements(recv, iface) {
				continue
			}
			for i := range iface.NumMethods() {
				im := iface.Method(i)
				if im.Name() != method.Name() {
					continue
				}
				key := im.FullName()
				if ri.result.ReflectAPIs[key] == nil {
					ri.result.ReflectAPIs[key] = make(map[int]bool)
				}
				// A method's receiver occupies SSA parameter zero. Interface
				// calls carry only declared parameters, so discard that slot.
				for pos := range params {
					if pos > 0 {
						ri.result.ReflectAPIs[key][pos-1] = true
					}
				}
			}
		}
	}
}

// matchReflectedInterfaceMethods resolves implementation relationships after
// all dependency summaries have been merged. A concrete type need not import
// the interface it implements; only the final build sees both packages.
func matchReflectedInterfaceMethods(pkg *types.Package, result *pkgCache) {
	targets := make(map[string]bool)
	for _, edge := range result.ReflectCallEdges {
		targets[edge.Callee] = true
	}
	if len(targets) == 0 {
		return
	}
	packages := make(map[string]*types.Package)
	var visit func(*types.Package)
	visit = func(p *types.Package) {
		if p == nil || packages[p.Path()] != nil {
			return
		}
		packages[p.Path()] = p
		for _, imp := range p.Imports() {
			visit(imp)
		}
	}
	visit(pkg)

	type interfaceMethod struct {
		iface  *types.Interface
		method *types.Func
	}
	var interfaces []interfaceMethod
	names := make(map[string]bool)
	paths := slices.Sorted(maps.Keys(packages))
	for _, path := range paths {
		p := packages[path]
		for _, name := range p.Scope().Names() {
			obj, ok := p.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			iface, ok := obj.Type().Underlying().(*types.Interface)
			if !ok {
				continue
			}
			for method := range iface.Methods() {
				if targets[method.FullName()] {
					interfaces = append(interfaces, interfaceMethod{iface, method})
					names[method.Name()] = true
				}
			}
		}
	}
	if len(interfaces) == 0 {
		return
	}

	type reflectedMethod struct {
		receiver types.Type
		params   map[int]bool
	}
	candidates := make(map[string][]reflectedMethod)
	for _, path := range paths {
		p := packages[path]
		for _, name := range p.Scope().Names() {
			obj, ok := p.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			if _, ok := obj.Type().Underlying().(*types.Interface); ok {
				continue
			}
			receiver := types.NewPointer(obj.Type())
			methods := types.NewMethodSet(receiver)
			for selection := range methods.Methods() {
				method := selection.Obj().(*types.Func)
				if params := result.ReflectAPIs[method.FullName()]; names[method.Name()] && len(params) > 0 {
					candidates[method.Name()] = append(candidates[method.Name()], reflectedMethod{receiver, params})
				}
			}
		}
	}
	for _, target := range interfaces {
		for _, concrete := range candidates[target.method.Name()] {
			if !types.Implements(concrete.receiver, target.iface) {
				continue
			}
			for pos := range concrete.params {
				if pos > 0 {
					if result.ReflectAPIs[target.method.FullName()] == nil {
						result.ReflectAPIs[target.method.FullName()] = make(map[int]bool)
					}
					result.ReflectAPIs[target.method.FullName()][pos-1] = true
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
		pos := slices.Index(fun.Params, param)
		// Methods have a receiver in SSA; generic functions may also have
		// synthetic leading parameters. Edges use declared parameter indexes.
		pos -= len(fun.Params) - fun.Signature.Params().Len()
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

// recordArgReflected finds the type(s) of a function argument, which is being used in reflection
// and excludes these types from obfuscation
// It also checks if this argument has any relation to a function parameter and returns it if found.
func (ri *reflectInspector) recordArgReflected(val ssa.Value, visited map[ssa.Value]bool) *ssa.Parameter {
	// make sure we visit every val only once, otherwise there will be infinite recursion
	if visited[val] {
		return nil
	}

	/* fmt.Printf("val: %v %T %v\n", val, val, val.Type()) */
	visited[val] = true

	// The static type is reflected even when the value's SSA origin is not
	// traceable (for example, indexing a slice returned by append).
	ri.recursivelyRecordUsedForReflect(val.Type())

	switch val := val.(type) {
	case *ssa.Phi:
		var param *ssa.Parameter
		for _, edge := range val.Edges {
			p := ri.recordArgReflected(edge, visited)
			if param == nil {
				param = p
			}
		}
		return param
	case *ssa.IndexAddr:
		for _, ref := range *val.Referrers() {
			if store, ok := ref.(*ssa.Store); ok {
				ri.recordArgReflected(store.Val, visited)
			}
		}
		return ri.recordArgReflected(val.X, visited)
	case *ssa.Slice:
		return ri.recordArgReflected(val.X, visited)
	case *ssa.Call:
		// A returned slice can contain the input models after reordering.
		// Only follow same-typed arguments rather than tainting every input.
		if _, ok := val.Type().(*types.Slice); ok {
			for _, arg := range val.Call.Args {
				if types.Identical(arg.Type(), val.Type()) {
					if param := ri.recordArgReflected(arg, visited); param != nil {
						return param
					}
				}
			}
		}
	case *ssa.MakeInterface:
		return ri.recordArgReflected(val.X, visited)
	case *ssa.UnOp:
		for _, ref := range *val.Referrers() {
			if idx, ok := ref.(ssa.Value); ok {
				ri.recordArgReflected(idx, visited)
			}
		}
		return ri.recordArgReflected(val.X, visited)
	case *ssa.FieldAddr:
		return ri.recordArgReflected(val.X, visited)

	case *ssa.Alloc:
		/* fmt.Printf("recording val %v \n", *val.Referrers()) */
		ri.recursivelyRecordUsedForReflect(val.Type())

		for _, ref := range *val.Referrers() {
			if idx, ok := ref.(ssa.Value); ok {
				ri.recordArgReflected(idx, visited)
			}
		}

		// relatedParam needs to revisit nodes so create an empty map
		visited := make(map[ssa.Value]bool)

		// check if the found alloc gets tainted by function parameters
		return relatedParam(val, visited)

	case *ssa.ChangeType:
		ri.recursivelyRecordUsedForReflect(val.X.Type())
		return ri.recordArgReflected(val.X, visited)
	case *ssa.MakeSlice, *ssa.MakeMap, *ssa.MakeChan, *ssa.Const:
		ri.recursivelyRecordUsedForReflect(val.Type())
	case *ssa.Global:
		ri.recursivelyRecordUsedForReflect(val.Type())

		// TODO: this might need similar logic to *ssa.Alloc, however
		// reassigning a function param to a global variable and then reflecting
		// it is probably unlikely to occur
	case *ssa.Parameter:
		// this only finds the parameters who want to be found,
		// otherwise relatedParam is used for more in depth analysis

		ri.recursivelyRecordUsedForReflect(val.Type())
		return val
	}

	return nil
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
		if ri.usedForReflect(obj) {
			return // prevent endless recursion
		}
		ri.recordUsedForReflect(obj, nil)
		// Match [computeFieldToStruct]: use the generic/origin struct, not an
		// instantiated underlying, so field identities line up with [hashWithStruct].
		ri.recursivelyRecordUsedForReflectImpl(t.Origin().Underlying(), visited)

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

	case interface{ Elem() types.Type }:
		// Get past pointers, slices, etc.
		ri.recursivelyRecordUsedForReflectImpl(t.Elem(), visited)
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

	lpkg := ri.lpkg
	if pkg != ri.pkg {
		var ok bool
		lpkg, ok = sharedCache.ListedPackages.get(pkg.Path())
		if !ok {
			panic("missing listed package for foreign reflected object: " + pkg.Path())
		}
	}
	return hashWithPackage(lpkg, obj.Name())
}

// recordUsedForReflect records the objects whose names we cannot obfuscate due to reflection.
// We currently record named types and fields.
func (ri *reflectInspector) recordUsedForReflect(obj types.Object, parent *types.Struct) {
	obfName := ri.obfuscatedObjectName(obj, parent)
	if obfName == "" {
		return
	}
	ri.result.ReflectObjectNames[obfName] = obj.Name()
	if flagDebug {
		log.Printf("reflect: preserving object %s as %q -> %q", obj, obfName, obj.Name())
	}
}

func (ri *reflectInspector) usedForReflect(obj types.Object) bool {
	obfName := ri.obfuscatedObjectName(obj, nil)
	if obfName == "" {
		return false
	}
	// TODO: Note that this does an object lookup by obfuscated name.
	// We should probably use unique object identifiers or strings,
	// such as go/types/objectpath.
	_, ok := ri.result.ReflectObjectNames[obfName]
	return ok
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
