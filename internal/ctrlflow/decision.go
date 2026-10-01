package ctrlflow

import (
	"go/constant"
	"go/token"
	"go/types"
	"math/rand"
	"sort"

	"golang.org/x/tools/go/ssa"
)

type decisionCase struct {
	key    *ssa.Const
	target *ssa.BasicBlock
}

// Accept only a parameter compared to distinct integer constants in a pure
// entry chain. No case expression or tag expression is evaluated by this pass.
func lowerDecision(fn *ssa.Function, rnd *rand.Rand) bool {
	if len(fn.Blocks) < 4 || len(fn.AnonFuncs) > 0 {
		return false
	}
	for _, b := range fn.Blocks {
		for _, i := range b.Instrs {
			switch i.(type) {
			case *ssa.Phi, *ssa.Defer, *ssa.RunDefers, *ssa.Call, *ssa.Alloc:
				return false
			}
		}
	}
	var checks []*ssa.BasicBlock
	var cases []decisionCase
	var value *ssa.Parameter
	current := fn.Blocks[0]
	seen := make(map[*ssa.BasicBlock]bool)
	keys := make(map[int64]bool)
	for len(current.Instrs) == 2 {
		cmp, ok := current.Instrs[0].(*ssa.BinOp)
		if !ok || cmp.Op != token.EQL {
			break
		}
		branch, ok := current.Instrs[1].(*ssa.If)
		if !ok || branch.Cond != cmp {
			break
		}
		p, ok := cmp.X.(*ssa.Parameter)
		if !ok {
			return false
		}
		if value == nil {
			value = p
		} else if value != p {
			return false
		}
		c, ok := cmp.Y.(*ssa.Const)
		if !ok || c.Value.Kind() != constant.Int {
			return false
		}
		n, ok := constant.Int64Val(c.Value)
		if !ok || keys[n] {
			return false
		}
		keys[n] = true
		if len(checks) > 0 && (len(current.Preds) != 1 || current.Preds[0] != checks[len(checks)-1]) {
			return false
		}
		if seen[current] {
			return false
		}
		seen[current] = true
		checks = append(checks, current)
		cases = append(cases, decisionCase{c, current.Succs[0]})
		current = current.Succs[1]
	}
	if len(checks) < 3 || len(checks) > 16 {
		return false
	}
	fallback := current
	if _, ok := fallback.Instrs[len(fallback.Instrs)-1].(*ssa.Return); !ok {
		return false
	}
	for _, c := range cases {
		if seen[c.target] {
			return false
		}
	}
	if rnd.Intn(2) == 0 {
		rnd.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
		for i, b := range checks {
			b.Instrs[0].(*ssa.BinOp).Y = cases[i].key
			b.Succs[0] = cases[i].target
			if i+1 < len(checks) {
				b.Succs[1] = checks[i+1]
			} else {
				b.Succs[1] = fallback
			}
		}
	} else {
		sort.Slice(cases, func(i, j int) bool { return constant.Compare(cases[i].key.Value, token.LSS, cases[j].key.Value) })
		next := 0
		var build func([]decisionCase) *ssa.BasicBlock
		build = func(cases []decisionCase) *ssa.BasicBlock {
			if len(cases) == 0 {
				return fallback
			}
			mid := len(cases) / 2
			block := checks[next]
			next++
			block.Instrs[0].(*ssa.BinOp).Y = cases[mid].key
			block.Succs[0] = cases[mid].target
			if len(cases) == 1 {
				block.Succs[1] = fallback
				return block
			}
			cmp := &ssa.BinOp{X: value, Op: token.LSS, Y: cases[mid].key}
			setType(cmp, types.Typ[types.Bool])
			branch := &ssa.If{Cond: cmp}
			group := &ssa.BasicBlock{Instrs: []ssa.Instruction{cmp, branch}}
			setBlockParent(group, fn)
			setBlock(cmp, group)
			setBlock(branch, group)
			*value.Referrers() = append(*value.Referrers(), cmp)
			*cmp.Referrers() = append(*cmp.Referrers(), branch)
			block.Succs[1] = group
			group.Succs = []*ssa.BasicBlock{build(cases[:mid]), build(cases[mid+1:])}
			fn.Blocks = append(fn.Blocks, group)
			return block
		}
		build(cases)
	}
	for _, b := range fn.Blocks {
		b.Preds = nil
	}
	for _, b := range fn.Blocks {
		for _, s := range b.Succs {
			s.Preds = append(s.Preds, b)
		}
	}
	return true
}
