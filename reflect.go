package main

import (
	"fmt"
	"go/types"
	"path/filepath"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"golang.org/x/tools/go/ssa"
)

type reflectInspector struct {
	pkg *types.Package

	checkedAPIs map[string]bool

	propagatedInstr map[ssa.Instruction]bool

	result pkgCache
}

// Record all instances of reflection use, and don't obfuscate types which are used in reflection.
func (ri *reflectInspector) recordReflection(ssaPkg *ssa.Package) {
	if reflectSkipPkg[ssaPkg.Pkg.Path()] {
		return
	}

	prevDone := len(ri.result.ReflectAPIs) + len(ri.result.ReflectObjects)

	// find all unchecked APIs to add them to checkedAPIs after the pass
	notCheckedAPIs := make(map[string]bool)
	for _, knownAPI := range maps.Keys(ri.result.ReflectAPIs) {
		if !ri.checkedAPIs[knownAPI] {
			notCheckedAPIs[knownAPI] = true
		}
	}

	ri.ignoreReflectedTypes(ssaPkg)

	// all previously unchecked APIs have now been checked add them to checkedAPIs,
	// to avoid checking them twice
	maps.Copy(ri.checkedAPIs, notCheckedAPIs)

	// if a new reflectAPI is found we need to Re-evaluate all functions which might be using that API
	newDone := len(ri.result.ReflectAPIs) + len(ri.result.ReflectObjects)
	if newDone > prevDone {
		ri.recordReflection(ssaPkg) // TODO: avoid recursing
	}
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

	for _, memb := range ssaPkg.Members {
		switch x := memb.(type) {
		case *ssa.Type:
			// methods aren't package members only their reciever types are
			// so some logic is required to find the methods a type has

			method := func(mset *types.MethodSet) {
				for i, n := 0, mset.Len(); i < n; i++ {
					at := mset.At(i)

					if m := ssaPkg.Prog.MethodValue(at); m != nil {
						ri.checkFunction(m)
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
		}
	}
}

// Exported methods with unnamed structs as paramters may be "used" in interface declarations
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

	params := sig.Params()
	for i := 0; i < params.Len(); i++ {
		if reflectParams[i] {
			continue
		}

		ignore := false
		param := params.At(i)

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
	}
}

// Checks the signature of an interface method for potential reflection use.
func (ri *reflectInspector) checkInterfaceMethod(m *types.Func) {
	reflectParams := make(map[int]bool)

	maps.Copy(reflectParams, ri.result.ReflectAPIs[m.FullName()])

	sig := m.Type().(*types.Signature)
	if m.Exported() {
		ri.checkMethodSignature(reflectParams, sig)
	}

	if len(reflectParams) > 0 {
		ri.result.ReflectAPIs[m.FullName()] = reflectParams

		/* fmt.Printf("curPkgCache.ReflectAPIs: %v\n", curPkgCache.ReflectAPIs) */
	}
}

// Checks all callsites in a function declaration for use of reflection.
func (ri *reflectInspector) checkFunction(fun *ssa.Function) {
	// if fun != nil && fun.Synthetic != "loaded from gc object file" {
	// 	// fun.WriteTo crashes otherwise
	// 	fun.WriteTo(os.Stdout)
	// }

	f, _ := fun.Object().(*types.Func)

	reflectParams := make(map[int]bool)
	if f != nil {
		maps.Copy(reflectParams, ri.result.ReflectAPIs[f.FullName()])

		if f.Exported() {
			ri.checkMethodSignature(reflectParams, fun.Signature)
		}
	}

	// fmt.Printf("f: %v\n", f)
	// fmt.Printf("fun: %v\n", fun)

	for _, block := range fun.Blocks {
		for _, inst := range block.Instrs {
			if ri.propagatedInstr[inst] {
				break // already done
			}

			// fmt.Printf("inst: %v, t: %T\n", inst, inst)
			switch inst := inst.(type) {
			case *ssa.Store:
				obj := typeToObj(inst.Addr.Type())
				if usedForReflect(ri.result, obj) {
					ri.recordArgReflected(inst.Val, make(map[ssa.Value]bool))
					ri.propagatedInstr[inst] = true
				}
			case *ssa.ChangeType:
				obj := typeToObj(inst.X.Type())
				if usedForReflect(ri.result, obj) {
					ri.recursivelyRecordUsedForReflect(inst.Type())
					ri.propagatedInstr[inst] = true
				}
			case *ssa.Call:
				callName := inst.Call.Value.String()
				if m := inst.Call.Method; m != nil {
					callName = inst.Call.Method.FullName()
				}

				if ri.checkedAPIs[callName] {
					// only check apis which were not already checked
					continue
				}

				/* fmt.Printf("callName: %v\n", callName) */

				// record each call argument passed to a function parameter which is used in reflection
				knownParams := ri.result.ReflectAPIs[callName]
				for knownParam := range knownParams {
					if len(inst.Call.Args) <= knownParam {
						continue
					}

					arg := inst.Call.Args[knownParam]

					/* fmt.Printf("flagging arg: %v\n", arg) */

					reflectedParam := ri.recordArgReflected(arg, make(map[ssa.Value]bool))
					if reflectedParam == nil {
						continue
					}

					pos := slices.Index(fun.Params, reflectedParam)
					if pos < 0 {
						continue
					}

					/* fmt.Printf("recorded param: %v func: %v\n", pos, fun) */

					reflectParams[pos] = true
				}
			}
		}
	}

	if len(reflectParams) > 0 {
		ri.result.ReflectAPIs[f.FullName()] = reflectParams

		/* fmt.Printf("curPkgCache.ReflectAPIs: %v\n", curPkgCache.ReflectAPIs) */
	}
}

// recordArgReflected finds the type(s) of a function argument, which is being used in reflection
// and excludes these types from obfuscation
// It also checks if this argument has any relation to a function paramter and returns it if found.
func (ri *reflectInspector) recordArgReflected(val ssa.Value, visited map[ssa.Value]bool) *ssa.Parameter {
	// make sure we visit every val only once, otherwise there will be infinite recursion
	if visited[val] {
		return nil
	}

	/* fmt.Printf("val: %v %T %v\n", val, val, val.Type()) */
	visited[val] = true

	switch val := val.(type) {
	case *ssa.IndexAddr:
		for _, ref := range *val.Referrers() {
			if store, ok := ref.(*ssa.Store); ok {
				ri.recordArgReflected(store.Val, visited)
			}
		}
		return ri.recordArgReflected(val.X, visited)
	case *ssa.Slice:
		return ri.recordArgReflected(val.X, visited)
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
	case *ssa.Const:
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

// relatedParam checks if a route to a function paramter can be constructed
// from a ssa.Value, and returns the paramter if it found one.
func relatedParam(val ssa.Value, visited map[ssa.Value]bool) *ssa.Parameter {
	// every val should only be visited once to prevent infinite recursion
	if visited[val] {
		return nil
	}

	/* fmt.Printf("related val: %v %T %v\n", val, val, val.Type()) */

	visited[val] = true

	switch x := val.(type) {
	case *ssa.Parameter:
		// a paramter has been found
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
// Only the names declared in the current package are recorded. This is to ensure
// that reflection detection only happens within the package declaring a type.
// Detecting it in downstream packages could result in inconsistencies.
func (ri *reflectInspector) recursivelyRecordUsedForReflect(t types.Type) {
	switch t := t.(type) {
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() == nil || obj.Pkg() != ri.pkg {
			return // not from the specified package
		}
		if usedForReflect(ri.result, obj) {
			return // prevent endless recursion
		}
		ri.recordUsedForReflect(obj)

		// Record the underlying type, too.
		ri.recursivelyRecordUsedForReflect(t.Underlying())

	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			field := t.Field(i)

			// This check is similar to the one in *types.Named.
			// It's necessary for unnamed struct types,
			// as they aren't named but still have named fields.
			if field.Pkg() == nil || field.Pkg() != ri.pkg {
				return // not from the specified package
			}

			// Record the field itself, too.
			ri.recordUsedForReflect(field)

			ri.recursivelyRecordUsedForReflect(field.Type())
		}

	case interface{ Elem() types.Type }:
		// Get past pointers, slices, etc.
		ri.recursivelyRecordUsedForReflect(t.Elem())
	}
}

// TODO: consider caching recordedObjectString via a map,
// if that shows an improvement in our benchmark
func recordedObjectString(obj types.Object) objectString {
	if obj == nil {
		return ""
	}
	pkg := obj.Pkg()
	if pkg == nil {
		return ""
	}
	// Names which are not at the package level still need to avoid obfuscation in some cases:
	//
	// 1. Field names on global types, which can be reached via reflection.
	// 2. Field names on anonymous types can also be reached via reflection.
	// 3. Local named types can be embedded in a local struct, becoming a field name as well.
	//
	// For now, a hack: assume that packages don't declare the same field
	// more than once in the same line. This works in practice, but one
	// could craft Go code to break this assumption.
	// Also note that the compiler's object files include filenames and line
	// numbers, but not column numbers nor byte offsets.
	if pkg.Scope() != obj.Parent() {
		switch obj := obj.(type) {
		case *types.Var: // struct fields; cases 1 and 2 above
			if !obj.IsField() {
				return "" // local variables don't need to be recorded
			}
		case *types.TypeName: // local named types; case 3 above
		default:
			return "" // other objects (labels, consts, etc) don't need to be recorded
		}
		pos := fset.Position(obj.Pos())
		return fmt.Sprintf("%s.%s - %s:%d", pkg.Path(), obj.Name(),
			filepath.Base(pos.Filename), pos.Line)
	}
	// For top-level names, "pkgpath.Name" is unique.
	return pkg.Path() + "." + obj.Name()
}

// recordUsedForReflect records the objects whose names we cannot obfuscate due to reflection.
// We currently record named types and fields.
func (ri *reflectInspector) recordUsedForReflect(obj types.Object) {
	if obj.Pkg().Path() != ri.pkg.Path() {
		panic("called recordUsedForReflect with a foreign object")
	}
	objStr := recordedObjectString(obj)
	if objStr == "" {
		// If the object can't be described via a qualified string,
		// do we need to record it at all?
		return
	}
	ri.result.ReflectObjects[objStr] = struct{}{}
}

func usedForReflect(cache pkgCache, obj types.Object) bool {
	objStr := recordedObjectString(obj)
	if objStr == "" {
		return false
	}
	_, ok := cache.ReflectObjects[objStr]
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
