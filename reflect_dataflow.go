package main

import (
	"container/heap"
	"go/types"
	"log"
	"maps"
	"slices"

	"golang.org/x/tools/go/ssa"
)

type reflectFactKind uint8

const (
	reflectAPIFact reflectFactKind = iota
	reflectLocalFact
	reflectTypeParamFact
	reflectObjectFact
)

// A fact key identifies a set of parameter positions, or a reflected name.
// Closures only have package-local identity and never enter the package cache.
type reflectFact struct {
	kind  reflectFactKind
	name  string
	local *ssa.Function
}

type reflectWorklist struct {
	watchers map[reflectFact][]int
	index    int
	first    bool
	queued   []bool
	now      reflectOrder
	next     reflectOrder
}

func (w *reflectWorklist) watch(fact reflectFact, step int) {
	steps := w.watchers[fact]
	if len(steps) == 0 || steps[len(steps)-1] != step {
		w.watchers[fact] = append(steps, step)
	}
}

func (w *reflectWorklist) changed(fact reflectFact) {
	for _, step := range w.watchers[fact] {
		if w.queued[step] {
			continue
		}
		if step <= w.index {
			w.next = append(w.next, step)
		} else if w.first {
			continue // every later step is already due in the first pass
		} else {
			heap.Push(&w.now, step)
		}
		w.queued[step] = true
	}
}

// Keep the old sweep's visit order, but only revisit consumers of new facts.
// A dynamic call's signature fallback is suppressed once a named requirement
// exists; visiting a consumer earlier or later can change what it retains.
func (w *reflectWorklist) run(count int, visit func(int)) {
	w.queued = make([]bool, count)
	w.first = true
	for i := range count {
		w.index = i
		visit(i)
	}
	w.first = false
	for len(w.next) > 0 {
		w.now, w.next = w.next, w.now[:0]
		heap.Init(&w.now)
		for len(w.now) > 0 {
			w.index = heap.Pop(&w.now).(int)
			w.queued[w.index] = false
			visit(w.index)
		}
	}
}

type reflectOrder []int

func (o reflectOrder) Len() int           { return len(o) }
func (o reflectOrder) Less(i, j int) bool { return o[i] < o[j] }
func (o reflectOrder) Swap(i, j int)      { o[i], o[j] = o[j], o[i] }
func (o *reflectOrder) Push(x any)        { *o = append(*o, x.(int)) }
func (o *reflectOrder) Pop() any {
	i := len(*o) - 1
	x := (*o)[i]
	*o = (*o)[:i]
	return x
}

// The solver visits a function at a time so that its parameter requirements
// become visible together, as they did in the sweep. Sites keep SSA order:
// object names and generic requirements become visible as soon as discovered.
type reflectFunctionFlow struct {
	fun     *ssa.Function
	obj     *types.Func
	name    string
	generic bool
	sigKey  string
	sites   []reflectSite
}

type reflectSite struct {
	// A return seeds a value requirement. A store or conversion is gated by
	// the hashed name of its source type and fires only once per instruction.
	gate  string
	instr ssa.Instruction
	value ssa.Value
	typ   types.Type
	call  *reflectCallSite
}

type reflectCallSite struct {
	name       string
	fallback   string
	locals     []*ssa.Function
	args       []ssa.Value
	firstParam int
	typeArgs   []types.Type
	typeFor    []types.Type
}

type reflectStep struct {
	fn    *reflectFunctionFlow
	iface *types.Func
}

func (ri *reflectInspector) solvePackageReflection(pkg *ssa.Package) {
	ri.work.watchers = make(map[reflectFact][]int)
	var steps []reflectStep
	reflectAnalysisSteps(pkg, func(fn *ssa.Function) {
		steps = append(steps, reflectStep{fn: ri.extractReflectFunction(fn, len(steps))})
	}, func(method *types.Func) {
		steps = append(steps, reflectStep{iface: method})
	})
	ri.work.run(len(steps), func(i int) {
		if step := steps[i]; step.fn != nil {
			ri.applyReflectFunction(step.fn)
		} else {
			ri.checkInterfaceMethod(step.iface)
		}
	})
	ri.work = reflectWorklist{}
}

func (ri *reflectInspector) extractReflectFunction(fun *ssa.Function, step int) *reflectFunctionFlow {
	fn := &reflectFunctionFlow{fun: fun}
	fn.obj, _ = ssaFuncOrigin(fun).Object().(*types.Func)
	if fn.obj != nil {
		fn.name, fn.generic = stripTypeArgs(reflectFuncName(fn.obj))
	}
	if fn.name != "" {
		// A method or another SSA body for the same function can add to
		// its named summary; re-export those positions under its signature.
		ri.work.watch(reflectFact{kind: reflectAPIFact, name: fn.name}, step)
	}
	for _, block := range fun.Blocks {
		for _, instr := range block.Instrs {
			var gateType types.Type
			var site reflectSite
			switch instr := instr.(type) {
			case *ssa.Return:
				// Callers in another package cannot see the types boxed in
				// these results. Keep the producer-side seed.
				for _, value := range instr.Results {
					if hasInterfaceData(value.Type()) {
						fn.sites = append(fn.sites, reflectSite{value: value})
					}
				}
			case *ssa.Store:
				gateType = instr.Addr.Type()
				site.value = instr.Val
			case *ssa.ChangeType:
				gateType = instr.X.Type()
				site.typ = instr.Type()
			case ssa.CallInstruction:
				fn.sites = append(fn.sites, reflectSite{call: ri.extractReflectCall(fn, instr.Common(), step)})
			}
			if obj := typeToObj(gateType); obj != nil {
				if gate := ri.obfuscatedObjectName(obj, nil); gate != "" {
					site.instr, site.gate = instr, gate
					fn.sites = append(fn.sites, site)
					ri.work.watch(reflectFact{kind: reflectObjectFact, name: gate}, step)
				}
			}
		}
	}
	return fn
}

func (ri *reflectInspector) extractReflectCall(fn *reflectFunctionFlow, call *ssa.CallCommon, step int) *reflectCallSite {
	callee := call.StaticCallee()
	var name string
	if callee != nil {
		if obj, _ := ssaFuncOrigin(callee).Object().(*types.Func); obj != nil {
			name = reflectFuncName(obj)
		}
	}
	if name == "" && call.Method != nil {
		name = reflectFuncName(call.Method)
	}
	if name == "" {
		name = call.Value.String()
	}
	name, _ = stripTypeArgs(name)
	site := &reflectCallSite{name: name}
	sig := call.Signature()
	if len(call.Args) > 0 && sig != nil {
		site.args = call.Args
		// A receiver or another synthetic argument can precede the declared ones.
		site.firstParam = len(call.Args) - sig.Params().Len()
		ri.work.watch(reflectFact{kind: reflectAPIFact, name: name}, step)
		if callee == nil && len(ri.result.ReflectAPIs[name]) == 0 {
			site.fallback = reflectSignatureKey(sig)
			ri.work.watch(reflectFact{kind: reflectAPIFact, name: site.fallback}, step)
		}
		for _, local := range localCallees(call.Value) {
			if obj, _ := ssaFuncOrigin(local).Object().(*types.Func); obj == nil {
				site.locals = append(site.locals, local)
				ri.work.watch(reflectFact{kind: reflectLocalFact, local: local}, step)
			}
		}
	}
	if fn.name != "" && ((callee != nil && callee.Object() != nil) || call.Method != nil) {
		ri.recordForwardedCall(fn.fun, fn.name, name, call.Args, sig)
	}
	if name == reflectTypeForFunc {
		site.typeFor = typeForTypeArgs(call)
	}
	// A method instantiation carries the receiver's type arguments, which
	// are not indexed like a function's own declared type parameters.
	if callee != nil && len(callee.TypeArgs()) > 0 {
		if params := ssaFuncOrigin(callee).TypeParams(); params != nil && params.Len() == len(callee.TypeArgs()) {
			site.typeArgs = callee.TypeArgs()
			ri.work.watch(reflectFact{kind: reflectTypeParamFact, name: name}, step)
		}
	}
	return site
}

func (ri *reflectInspector) applyReflectFunction(fn *reflectFunctionFlow) {
	fun := fn.fun
	params := make(map[int]bool)
	if fn.name == "" {
		maps.Copy(params, ri.localReflectAPIs[fun])
	} else {
		maps.Copy(params, ri.result.ReflectAPIs[fn.name])
		if fn.obj.Exported() {
			ri.checkMethodSignature(params, fun.Signature)
		}
	}
	for _, site := range fn.sites {
		if site.gate != "" {
			if ri.propagatedInstr[site.instr] || !ri.result.ReflectedObjects[site.gate] {
				continue
			}
			ri.propagatedInstr[site.instr] = true
		}
		switch {
		case site.call != nil:
			ri.applyReflectCall(fun, site.call, params)
		case site.value != nil:
			ri.recordArgReflected(fun, site.value, make(map[ssa.Value]bool))
		case site.typ != nil:
			ri.recursivelyRecordUsedForReflect(site.typ)
		}
	}
	if len(params) == 0 {
		return
	}
	if fn.name == "" {
		if ri.localReflectAPIs == nil {
			ri.localReflectAPIs = make(map[*ssa.Function]map[int]bool)
		}
		if !maps.Equal(ri.localReflectAPIs[fun], params) {
			ri.localReflectAPIs[fun] = params
			ri.work.changed(reflectFact{kind: reflectLocalFact, local: fun})
		}
		return
	}
	ri.addNamedReflectParams(reflectAPIFact, fn.name, params)
	if !fn.generic {
		if fn.sigKey == "" {
			fn.sigKey = reflectSignatureKey(fun.Signature)
		}
		ri.addNamedReflectParams(reflectAPIFact, fn.sigKey, params)
	}
	if fun.Signature.Recv() != nil {
		ri.recordImplementedInterfaceMethods(fn.obj, params)
	}
}

func (ri *reflectInspector) applyReflectCall(fun *ssa.Function, site *reflectCallSite, params map[int]bool) {
	for _, typ := range site.typeFor {
		ri.recordReflectType(fun, typ)
	}
	if len(site.typeArgs) > 0 {
		for _, pos := range slices.Sorted(maps.Keys(ri.result.ReflectTypeParams[site.name])) {
			if pos >= 0 && pos < len(site.typeArgs) {
				ri.recordReflectType(fun, site.typeArgs[pos])
			}
		}
	}
	if len(site.args) == 0 {
		return
	}
	known := ri.result.ReflectAPIs[site.name]
	if len(known) == 0 && site.fallback != "" {
		known = ri.result.ReflectAPIs[site.fallback]
	}
	for _, local := range site.locals {
		if localParams := ri.localReflectAPIs[local]; len(localParams) > 0 {
			known = maps.Clone(known)
			if known == nil {
				known = make(map[int]bool)
			}
			maps.Copy(known, localParams)
		}
	}
	for _, pos := range slices.Sorted(maps.Keys(known)) {
		argPos := site.firstParam + pos
		if argPos < 0 || argPos >= len(site.args) {
			continue
		}
		arg := site.args[argPos]
		p := ri.recordArgReflected(fun, arg, make(map[ssa.Value]bool))
		if p == nil {
			continue
		}
		if pos := reflectParamPosition(fun, p); pos >= 0 {
			params[pos] = true
			if flagDebug {
				log.Printf("reflect: %s marks param %d reflected via %s argument %T", fun, pos, site.name, arg)
			}
		}
	}
}

func reflectParamPosition(fun *ssa.Function, p *ssa.Parameter) int {
	// Receiver and synthetic leading parameters are not summary keys.
	return slices.Index(fun.Params, p) - (len(fun.Params) - fun.Signature.Params().Len())
}

func (ri *reflectInspector) addNamedReflectParams(kind reflectFactKind, name string, params map[int]bool) {
	table := ri.result.ReflectAPIs
	if kind == reflectTypeParamFact {
		if ri.result.ReflectTypeParams == nil {
			ri.result.ReflectTypeParams = make(map[string]map[int]bool)
		}
		table = ri.result.ReflectTypeParams
	}
	if dst := table[name]; dst == nil {
		table[name] = maps.Clone(params)
	} else {
		changed := false
		for pos, value := range params {
			if old, ok := dst[pos]; !ok || old != value {
				dst[pos] = value
				changed = true
			}
		}
		if !changed {
			return
		}
	}
	ri.work.changed(reflectFact{kind: kind, name: name})
}
