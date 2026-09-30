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

func applyPartialFlattening(fn *ssa.Function, rnd *mathrand.Rand) []dispatcherInfo {
	var edges []regionEdge
	for _, block := range fn.Blocks {
		for i, target := range block.Succs {
			// Keep all edges within cycles direct, rather than charging dispatch on loop iterations.
			if !reaches(target, block, make(map[*ssa.BasicBlock]bool)) {
				edges = append(edges, regionEdge{block, target, i})
			}
		}
	}
	if len(edges) < 8 {
		return nil
	}
	rnd.Shuffle(len(edges), func(i, j int) { edges[i], edges[j] = edges[j], edges[i] })
	edges = edges[:min(len(edges)/2, 32)]
	var infos []dispatcherInfo
	for group := 0; group < 2; group++ {
		phi := &ssa.Phi{Comment: "ctrflow.region"}
		setType(phi, types.Typ[types.Int])
		entry := &ssa.BasicBlock{Comment: "ctrflow.region.entry", Instrs: []ssa.Instruction{phi, &ssa.Jump{}}}
		setBlockParent(entry, fn)
		setBlock(phi, entry)
		fn.Blocks = append(fn.Blocks, entry)
		var info dispatcherInfo
		var comparisons []*ssa.BasicBlock
		for i := group; i < len(edges); i += 2 {
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
