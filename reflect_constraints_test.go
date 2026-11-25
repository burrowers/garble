package main

import (
	"maps"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSolveReflectCallEdges(t *testing.T) {
	// The edges are in the opposite order from the flow of facts. Only the
	// second argument of iface.M uses reflection, and one cycle has no seed.
	edges := []reflectCallEdge{
		{Caller: "entry", Callee: "outer", CalleeParam: 2},
		{Caller: "outer", CallerParam: 2, Callee: "middle", CalleeParam: 0},
		{Caller: "middle", Callee: "outer", CalleeParam: 2},
		{Caller: "outer", CallerParam: 2, Callee: "middle", CalleeParam: 0},
		{Caller: "middle", Callee: "iface.M", CalleeParam: 1},
		{Caller: "middle", CallerParam: 1, Callee: "iface.M"},
		{Caller: "left", Callee: "right"},
		{Caller: "right", Callee: "left"},
	}
	apis := map[string]map[int]bool{"iface.M": {0: false, 1: true}, "outer": {1: true}}
	want := map[string]map[int]bool{
		"entry":   {0: true},
		"iface.M": {0: false, 1: true},
		"middle":  {0: true},
		"outer":   {1: true, 2: true},
	}
	solveReflectCallEdges(apis, edges)
	if diff := cmp.Diff(want, apis); diff != "" {
		t.Fatalf("reflection requirements (-want +got):\n%s", diff)
	}
}

func FuzzSolveReflectCallEdges(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{
		0x00, 0x20, 0x00, // a[0] <- b[0]
		0x20, 0x40, 0x00, // b[0] <- c[0]
		0x40, 0x00, 0xc0, // c[0] is required
	})
	f.Add([]byte{
		0x00, 0x20, 0x00, // a[0] <- b[0]
		0x20, 0x00, 0x00, // b[0] <- a[0]
		0x20, 0x00, 0x80, // b[0] is not required
	})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256 {
			data = data[:256]
		}
		names := [...]string{"a", "b", "c", "d", "e", "f", "g", "h"}
		seed := make(map[string]map[int]bool)
		var edges []reflectCallEdge
		for len(data) >= 3 {
			caller, callee, kind := data[0], data[1], data[2]
			data = data[3:]
			edge := reflectCallEdge{
				Caller: names[caller>>5], CallerParam: int(caller & 3),
				Callee: names[callee>>5], CalleeParam: int(callee & 3),
			}
			if kind&0x80 != 0 {
				if seed[edge.Caller] == nil {
					seed[edge.Caller] = make(map[int]bool)
				}
				seed[edge.Caller][edge.CallerParam] = kind&0x40 != 0
			} else {
				edges = append(edges, edge)
			}
		}
		want := make(map[string]map[int]bool, len(seed))
		for name, positions := range seed {
			want[name] = maps.Clone(positions)
		}
		sweepReflectCallEdges(want, edges)
		solveReflectCallEdges(seed, edges)
		if diff := cmp.Diff(want, seed); diff != "" {
			t.Fatalf("reflection requirements (-sweep +worklist):\n%s", diff)
		}
	})
}

// sweepReflectCallEdges is the pre-worklist fixed-point loop. Keep it as the
// differential oracle, without sharing the solver's bookkeeping.
func sweepReflectCallEdges(apis map[string]map[int]bool, edges []reflectCallEdge) {
	for changed := true; changed; {
		changed = false
		for _, edge := range edges {
			if !apis[edge.Callee][edge.CalleeParam] {
				continue
			}
			params := apis[edge.Caller]
			if params == nil {
				params = make(map[int]bool)
				apis[edge.Caller] = params
			}
			if !params[edge.CallerParam] {
				params[edge.CallerParam] = true
				changed = true
			}
		}
	}
}
