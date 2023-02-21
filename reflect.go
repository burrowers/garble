package main

import (
	"fmt"
	"go/types"
	"path/filepath"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"golang.org/x/tools/go/ssa"
)

var checkedAPIs = make(map[string]bool)

// Record all instances of reflection use, and don't obfuscate types which are used in reflection.
func (tf *transformer) recordReflection(ssaPkg *ssa.Package) {
	if reflectSkipPkg[ssaPkg.Pkg.Name()] {
		return
	}

	lenPrevKnownReflectAPIs := len(cachedOutput.KnownReflectAPIs)

	// find all unchecked APIs to add them to checkedAPIs after the pass
	notCheckedAPIs := make(map[string]bool)
	for _, knownAPI := range maps.Keys(cachedOutput.KnownReflectAPIs) {
		if !checkedAPIs[knownAPI] {
			notCheckedAPIs[knownAPI] = true
		}
	}

	tf.ignoreReflectedTypes(ssaPkg)

	// all previously unchecked APIs have now been checked add them to checkedAPIs,
	// to avoid checking them twice
	maps.Copy(checkedAPIs, notCheckedAPIs)

	// if a new reflectAPI is found we need to Re-evaluate all functions which might be using that API
	if len(cachedOutput.KnownReflectAPIs) > lenPrevKnownReflectAPIs {
		tf.recordReflection(ssaPkg)
	}
}

func (tf *transformer) ignoreReflectedTypes(ssaPkg *ssa.Package) {
	for _, memb := range ssaPkg.Members {
		if t, ok := memb.(*ssa.Type); ok {
			// methods aren't package members only their reciever types are
			// so some logic is required to find the methods a type has

			// yes, finding all methods really only works with both loops
			mset := ssaPkg.Prog.MethodSets.MethodSet(t.Type())
			for i, n := 0, mset.Len(); i < n; i++ {
				fun := ssaPkg.Prog.MethodValue(mset.At(i))
				if fun != nil {
					tf.checkCalls(fun)
				}
			}

			mset = ssaPkg.Prog.MethodSets.MethodSet(types.NewPointer(t.Type()))
			for i, n := 0, mset.Len(); i < n; i++ {
				fun := ssaPkg.Prog.MethodValue(mset.At(i))
				if fun != nil {
					tf.checkCalls(fun)
				}
			}
		}

		if fun, ok := memb.(*ssa.Function); ok {
			// these not only include top level functions, but also synthetic
			// functions like the initialization of global variables
			tf.checkCalls(fun)
		}
	}
}

func (tf *transformer) checkCalls(fun *ssa.Function) {
	if fun.Synthetic == "loaded from gc object file" {
		// fun.WriteTo crashes otherwise
		return
	}

	/* fun.WriteTo(os.Stdout) */

	var reflectParams []int

	for _, block := range fun.Blocks {
		for _, inst := range block.Instrs {
			/* 	fmt.Printf("inst: %v, t: %T\n", inst, inst) */
			call, ok := inst.(*ssa.Call)
			if !ok {
				continue
			}
			/* fmt.Printf("call: %v\n", call) */

			callName := call.Call.Value.String()
			if checkedAPIs[callName] {
				// only check apis which were not already checked
				continue
			}

			// record each call argument passed to a function parameter which is used in reflection
			knownParams := cachedOutput.KnownReflectAPIs[callName]
			for _, knownParam := range knownParams {
				if len(call.Call.Args) <= knownParam {
					continue
				}

				arg := call.Call.Args[knownParam]

				/* fmt.Printf("flagging arg: %v\n", arg) */

				visited := make(map[ssa.Value]bool)
				reflectedParam := tf.recordArgReflected(arg, visited)
				if reflectedParam == nil {
					continue
				}

				pos := slices.Index(fun.Params, reflectedParam)
				if pos < 0 {
					continue
				}

				/* fmt.Printf("recorded param: %v func: %v\n", pos, fun) */

				if !slices.Contains(reflectParams, pos) {
					reflectParams = append(reflectParams, pos)
				}
			}
		}
	}

	if len(reflectParams) > 0 {
		cachedOutput.KnownReflectAPIs[fun.String()] = reflectParams

		/* 	fmt.Printf("cachedOutput.KnownReflectAPIs: %v\n", cachedOutput.KnownReflectAPIs) */
	}
}

// recordArgReflected finds the type(s) of a function argument, which is being used in reflection
// and excludes these types from obfuscation
// It also checks if this argument has any relation to a function paramter and returns it if found.
func (tf *transformer) recordArgReflected(val ssa.Value, visited map[ssa.Value]bool) *ssa.Parameter {
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
				tf.recordArgReflected(store.Val, visited)
			}
		}
		return tf.recordArgReflected(val.X, visited)
	case *ssa.Slice:
		return tf.recordArgReflected(val.X, visited)
	case *ssa.MakeInterface:
		return tf.recordArgReflected(val.X, visited)
	case *ssa.UnOp:
		return tf.recordArgReflected(val.X, visited)
	case *ssa.FieldAddr:
		return tf.recordArgReflected(val.X, visited)

	case *ssa.Alloc:
		/* fmt.Printf("recording val %v \n", *val.Referrers()) */
		tf.recursivelyRecordAsNotObfuscated(val.Type())

		for _, ref := range *val.Referrers() {
			if idx, ok := ref.(*ssa.IndexAddr); ok {
				tf.recordArgReflected(idx, visited)
			}
		}

		// relatedParam needs to revisit nodes so create an empty map
		visited := make(map[ssa.Value]bool)

		// check if the found alloc gets tainted by function parameters
		return relatedParam(val, visited)

	case *ssa.Const:
		tf.recursivelyRecordAsNotObfuscated(val.Type())
	case *ssa.Global:
		tf.recursivelyRecordAsNotObfuscated(val.Type())

		// TODO: this might need similar logic to *ssa.Alloc, however
		// reassigning a function param to a global variable and then reflecting
		// it is probably unlikely to occur
	case *ssa.Parameter:
		// this only finds the parameters who want to be found,
		// otherwise relatedParam is used for more in depth analysis
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

	if param, ok := val.(*ssa.Parameter); ok {
		// a paramter has been found
		return param
	}

	if unop, ok := val.(*ssa.UnOp); ok {
		if param := relatedParam(unop.X, visited); param != nil {
			return param
		}
	}

	if addr, ok := val.(*ssa.FieldAddr); ok {
		/* fmt.Printf("addr: %v\n", addr)
		fmt.Printf("addr.X: %v %T\n", addr.X, addr.X) */

		if param := relatedParam(addr.X, visited); param != nil {
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

// recursivelyRecordAsNotObfuscated calls recordAsNotObfuscated on any named
// types and fields under typ.
//
// Only the names declared in the current package are recorded. This is to ensure
// that reflection detection only happens within the package declaring a type.
// Detecting it in downstream packages could result in inconsistencies.
func (tf *transformer) recursivelyRecordAsNotObfuscated(t types.Type) {
	switch t := t.(type) {
	case *types.Named:
		obj := t.Obj()

		// TODO: the transformer is only needed in this function, there is
		// probably a way to do this with only the ssa information.
		if obj.Pkg() == nil || obj.Pkg() != tf.pkg {
			return // not from the specified package
		}
		if recordedAsNotObfuscated(obj) {
			return // prevent endless recursion
		}
		recordAsNotObfuscated(obj)

		// Record the underlying type, too.
		tf.recursivelyRecordAsNotObfuscated(t.Underlying())

	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			field := t.Field(i)

			// This check is similar to the one in *types.Named.
			// It's necessary for unnamed struct types,
			// as they aren't named but still have named fields.
			if field.Pkg() == nil || field.Pkg() != tf.pkg {
				return // not from the specified package
			}

			// Record the field itself, too.
			recordAsNotObfuscated(field)

			tf.recursivelyRecordAsNotObfuscated(field.Type())
		}

	case interface{ Elem() types.Type }:
		// Get past pointers, slices, etc.
		tf.recursivelyRecordAsNotObfuscated(t.Elem())
	}
}

// TODO: consider caching recordedObjectString via a map,
// if that shows an improvement in our benchmark

func recordedObjectString(obj types.Object) objectString {
	pkg := obj.Pkg()
	if obj, ok := obj.(*types.Var); ok && obj.IsField() {
		// For exported fields, "pkgpath.Field" is not unique,
		// because two exported top-level types could share "Field".
		//
		// Moreover, note that not all fields belong to named struct types;
		// an API could be exposing:
		//
		//   var usedInReflection = struct{Field string}
		//
		// For now, a hack: assume that packages don't declare the same field
		// more than once in the same line. This works in practice, but one
		// could craft Go code to break this assumption.
		// Also note that the compiler's object files include filenames and line
		// numbers, but not column numbers nor byte offsets.
		// TODO(mvdan): give this another think, and add tests involving anon types.
		pos := fset.Position(obj.Pos())
		return fmt.Sprintf("%s.%s - %s:%d", pkg.Path(), obj.Name(),
			filepath.Base(pos.Filename), pos.Line)
	}
	// Names which are not at the top level cannot be imported,
	// so we don't need to record them either.
	// Note that this doesn't apply to fields, which are never top-level.
	if pkg.Scope() != obj.Parent() {
		return ""
	}
	// For top-level exported names, "pkgpath.Name" is unique.
	return pkg.Path() + "." + obj.Name()
}

// recordAsNotObfuscated records all the objects whose names we cannot obfuscate.
// An object is any named entity, such as a declared variable or type.
//
// As of June 2022, this only records types which are used in reflection.
// TODO(mvdan): If this is still the case in a year's time,
// we should probably rename "not obfuscated" and "cannot obfuscate" to be
// directly about reflection, e.g. "used in reflection".
func recordAsNotObfuscated(obj types.Object) {
	if obj.Pkg().Path() != curPkg.ImportPath {
		panic("called recordedAsNotObfuscated with a foreign object")
	}
	if !obj.Exported() {
		// Unexported names will never be used by other packages,
		// so we don't need to bother recording them in cachedOutput.
		knownCannotObfuscateUnexported[obj] = true
		return
	}

	objStr := recordedObjectString(obj)
	if objStr == "" {
		// If the object can't be described via a qualified string,
		// then other packages can't use it.
		// TODO: should we still record it in knownCannotObfuscateUnexported?
		return
	}
	cachedOutput.KnownCannotObfuscate[objStr] = struct{}{}
}

func recordedAsNotObfuscated(obj types.Object) bool {
	if knownCannotObfuscateUnexported[obj] {
		return true
	}
	objStr := recordedObjectString(obj)
	if objStr == "" {
		return false
	}
	_, ok := cachedOutput.KnownCannotObfuscate[objStr]
	return ok
}
