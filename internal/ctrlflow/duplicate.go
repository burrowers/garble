package ctrlflow

import (
	"go/token"
	"go/types"
	mathrand "math/rand"

	"golang.org/x/tools/go/ssa"
)

func duplicateReturnBlocks(fn *ssa.Function, budget int, rnd *mathrand.Rand) {
	original := append([]*ssa.BasicBlock(nil), fn.Blocks...)
	for _, block := range original {
		if budget == 0 {
			break
		}
		if len(block.Preds) < 2 || len(block.Instrs) > 8 {
			continue
		}
		eligible := true
		for i, instr := range block.Instrs {
			switch v := instr.(type) {
			case *ssa.BinOp:
				if v.Op != token.ADD && v.Op != token.MUL && v.Op != token.XOR && v.Op != token.AND && v.Op != token.OR {
					eligible = false
				}
				basic, ok := v.Type().Underlying().(*types.Basic)
				if !ok || basic.Info()&types.IsInteger == 0 {
					eligible = false
				}
				for _, ref := range *v.Referrers() {
					if ref.Block() != block {
						eligible = false
					}
				}
			case *ssa.Return:
				if i != len(block.Instrs)-1 {
					eligible = false
				}
			default:
				eligible = false
			}
		}
		if !eligible {
			continue
		}
		predIndex := rnd.Intn(len(block.Preds))
		pred := block.Preds[predIndex]
		clone := &ssa.BasicBlock{Comment: "ctrflow.duplicate", Preds: []*ssa.BasicBlock{pred}}
		setBlockParent(clone, fn)
		values := make(map[ssa.Value]ssa.Value)
		remap := func(v ssa.Value) ssa.Value {
			if replacement, ok := values[v]; ok {
				return replacement
			}
			return v
		}
		for _, instr := range block.Instrs {
			var copied ssa.Instruction
			switch v := instr.(type) {
			case *ssa.BinOp:
				op := &ssa.BinOp{Op: v.Op, X: remap(v.Y), Y: remap(v.X)}
				setType(op, v.Type())
				values[v] = op
				copied = op
			case *ssa.Return:
				ret := &ssa.Return{}
				for _, value := range v.Results {
					ret.Results = append(ret.Results, remap(value))
				}
				copied = ret
			}
			setBlock(copied, clone)
			clone.Instrs = append(clone.Instrs, copied)
			for _, operand := range copied.Operands(nil) {
				if operand != nil && *operand != nil && (*operand).Referrers() != nil {
					refs := (*operand).Referrers()
					*refs = append(*refs, copied)
				}
			}
		}
		for i, succ := range pred.Succs {
			if succ == block {
				pred.Succs[i] = clone
			}
		}
		block.Preds = append(block.Preds[:predIndex], block.Preds[predIndex+1:]...)
		fn.Blocks = append(fn.Blocks, clone)
		budget--
	}
}
