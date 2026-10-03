package ctrlflow

import (
	"go/constant"
	"go/token"
	"go/types"
	mathrand "math/rand"

	"golang.org/x/tools/go/ssa"
)

func substituteLiveIntegers(fn *ssa.Function, rnd *mathrand.Rand) {
	for _, block := range fn.Blocks {
		var instrs []ssa.Instruction
		for _, instr := range block.Instrs {
			add, ok := instr.(*ssa.BinOp)
			if !ok || add.Op != token.ADD {
				instrs = append(instrs, instr)
				continue
			}
			basic, ok := add.Type().Underlying().(*types.Basic)
			if !ok || (basic.Kind() != types.Uint8 && basic.Kind() != types.Uint16 && basic.Kind() != types.Uint32 && basic.Kind() != types.Uint64) {
				instrs = append(instrs, instr)
				continue
			}
			x, y := add.X, add.Y
			if rnd.Intn(2) == 0 {
				x, y = y, x
			}
			xor := &ssa.BinOp{Op: token.XOR, X: x, Y: y}
			and := &ssa.BinOp{Op: token.AND, X: x, Y: y}
			carry := &ssa.BinOp{Op: token.SHL, X: and, Y: ssa.NewConst(constant.MakeInt64(1), types.Typ[types.Uint])}
			for _, op := range []*ssa.BinOp{xor, and, carry} {
				setType(op, add.Type())
				setBlock(op, block)
			}
			// Keep the original result object so consumers retain their SSA identity.
			add.X, add.Y = xor, carry
			instrs = append(instrs, xor, and, carry, add)
		}
		block.Instrs = instrs
	}
	// The converter uses referrers to decide which values must cross block scope.
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if v, ok := instr.(ssa.Value); ok && v.Referrers() != nil {
				*v.Referrers() = nil
			}
		}
	}
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			for _, operand := range instr.Operands(nil) {
				if operand != nil && *operand != nil && (*operand).Referrers() != nil {
					refs := (*operand).Referrers()
					*refs = append(*refs, instr)
				}
			}
		}
	}
}
