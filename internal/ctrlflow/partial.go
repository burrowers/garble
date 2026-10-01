package ctrlflow

import (
	"go/token"
	"go/types"
	mathrand "math/rand"

	"golang.org/x/tools/go/ssa"
)

type regionEdge struct {
	from, target *ssa.BasicBlock
	index        int
}

func reaches(from, target *ssa.BasicBlock, seen map[*ssa.BasicBlock]bool) bool {
	if from == target {
		return true
	}
	if seen[from] {
		return false
	}
	seen[from] = true
	for _, succ := range from.Succs {
		if reaches(succ, target, seen) {
			return true
		}
	}
	return false
}

type partialRegion struct {
	root   *ssa.BasicBlock
	blocks map[*ssa.BasicBlock]bool
	edges  []regionEdge
}

// Grow only when every incoming edge is already inside the region. The root
// is its only possible entry, and cycle blocks never incur dispatch overhead.
func selectPartialRegions(fn *ssa.Function, rnd *mathrand.Rand) []partialRegion {
	cyclic := make(map[*ssa.BasicBlock]bool)
	for _, b := range fn.Blocks {
		for _, s := range b.Succs {
			if reaches(s, b, make(map[*ssa.BasicBlock]bool)) {
				cyclic[b] = true
				break
			}
		}
	}
	var candidates []partialRegion
	for _, root := range fn.Blocks {
		if cyclic[root] {
			continue
		}
		r := partialRegion{root: root, blocks: map[*ssa.BasicBlock]bool{root: true}}
		limit := 3 + rnd.Intn(2)
		for len(r.blocks) < limit {
			var next []*ssa.BasicBlock
			for _, b := range fn.Blocks {
				if cyclic[b] || r.blocks[b] || len(b.Preds) == 0 {
					continue
				}
				eligible := true
				for _, p := range b.Preds {
					if !r.blocks[p] {
						eligible = false
						break
					}
				}
				if eligible {
					next = append(next, b)
				}
			}
			if len(next) == 0 {
				break
			}
			r.blocks[next[rnd.Intn(len(next))]] = true
		}
		for _, b := range fn.Blocks {
			if !r.blocks[b] {
				continue
			}
			for i, s := range b.Succs {
				if r.blocks[s] {
					r.edges = append(r.edges, regionEdge{b, s, i})
				}
			}
		}
		if len(r.edges) >= 2 {
			candidates = append(candidates, r)
		}
	}
	rnd.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	for i, a := range candidates {
		for _, b := range candidates[i+1:] {
			disjoint := true
			for block := range a.blocks {
				if b.blocks[block] {
					disjoint = false
					break
				}
			}
			if disjoint {
				return []partialRegion{a, b}
			}
		}
	}
	return nil
}

func applyPartialFlattening(fn *ssa.Function, rnd *mathrand.Rand) []dispatcherInfo {
	regions := selectPartialRegions(fn, rnd)
	if len(regions) == 0 {
		return nil
	}
	var infos []dispatcherInfo
	for _, region := range regions {
		edges := region.edges
		rnd.Shuffle(len(edges), func(i, j int) { edges[i], edges[j] = edges[j], edges[i] })
		phi := &ssa.Phi{Comment: "ctrflow.region"}
		setType(phi, types.Typ[types.Int])
		entry := &ssa.BasicBlock{Comment: "ctrflow.region.entry", Instrs: []ssa.Instruction{phi, &ssa.Jump{}}}
		setBlockParent(entry, fn)
		setBlock(phi, entry)
		fn.Blocks = append(fn.Blocks, entry)
		var info dispatcherInfo
		var comparisons []*ssa.BasicBlock
		for i := range edges {
			edge := edges[i]
			key := makeSsaInt(len(info) + 1)
			cfg := cfgInfo{StoreVar: key, CompareVar: makeSsaInt(len(info) + 1)}
			adapter := &ssa.BasicBlock{Comment: "ctrflow.region.jump", Instrs: []ssa.Instruction{&ssa.Jump{}}, Preds: []*ssa.BasicBlock{edge.from}, Succs: []*ssa.BasicBlock{entry}}
			setBlockParent(adapter, fn)
			edge.from.Succs[edge.index] = adapter
			entry.Preds = append(entry.Preds, adapter)
			phi.Edges = append(phi.Edges, key)
			compare := &ssa.BinOp{X: phi, Op: token.EQL, Y: cfg.CompareVar}
			setType(compare, types.Typ[types.Bool])
			branch := &ssa.If{Cond: compare}
			check := &ssa.BasicBlock{Comment: "ctrflow.region.check", Instrs: []ssa.Instruction{compare, branch}, Succs: []*ssa.BasicBlock{edge.target, edge.target}}
			setBlockParent(check, fn)
			setBlock(compare, check)
			setBlock(branch, check)
			*phi.Referrers() = append(*phi.Referrers(), compare)
			*compare.Referrers() = append(*compare.Referrers(), branch)
			// Original Phi edges stay assigned in the original predecessor, before its adapter.
			edge.target.Preds = append(edge.target.Preds, check)
			if len(comparisons) == 0 {
				entry.Succs = []*ssa.BasicBlock{check}
				check.Preds = []*ssa.BasicBlock{entry}
			} else {
				previous := comparisons[len(comparisons)-1]
				previous.Succs[1] = check
				check.Preds = []*ssa.BasicBlock{previous}
			}
			comparisons = append(comparisons, check)
			fn.Blocks = append(fn.Blocks, adapter, check)
			info = append(info, cfg)
		}
		infos = append(infos, info)
	}
	rnd.Shuffle(len(fn.Blocks)-1, func(i, j int) { fn.Blocks[i+1], fn.Blocks[j+1] = fn.Blocks[j+1], fn.Blocks[i+1] })
	return infos
}
