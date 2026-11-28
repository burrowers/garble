package main

import "slices"

type reflectParam struct {
	funcName string
	pos      int
}

// solveReflectCallEdges computes the least fixed point of the cached forwarding
// constraints: if the callee parameter requires reflection, so does the caller
// parameter forwarded to it. Both positions exclude the receiver.
func solveReflectCallEdges(apis map[string]map[int]bool, edges []reflectCallEdge) {
	require := func(param reflectParam) bool {
		positions := apis[param.funcName]
		if positions[param.pos] {
			return false
		}
		if positions == nil {
			positions = make(map[int]bool)
			apis[param.funcName] = positions
		}
		positions[param.pos] = true
		return true
	}

	// Merged summaries can already satisfy the constraints, or settle in
	// one sweep. Only build the worklist if an applicable edge was missed.
	changed := false
	for _, edge := range edges {
		if apis[edge.Callee][edge.CalleeParam] && require(reflectParam{edge.Caller, edge.CallerParam}) {
			changed = true
		}
	}
	if !changed || !slices.ContainsFunc(edges, func(edge reflectCallEdge) bool {
		return apis[edge.Callee][edge.CalleeParam] && !apis[edge.Caller][edge.CallerParam]
	}) {
		return
	}

	dependents := make(map[reflectParam][]reflectParam)
	for _, edge := range edges {
		if !apis[edge.Caller][edge.CallerParam] {
			callee := reflectParam{edge.Callee, edge.CalleeParam}
			dependents[callee] = append(dependents[callee], reflectParam{edge.Caller, edge.CallerParam})
		}
	}
	var work []reflectParam
	for callee := range dependents {
		if apis[callee.funcName][callee.pos] {
			work = append(work, callee)
		}
	}
	for i := 0; i < len(work); i++ {
		for _, caller := range dependents[work[i]] {
			if require(caller) {
				work = append(work, caller)
			}
		}
	}
}
