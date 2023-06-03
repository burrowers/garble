package ctrlflow

import (
	mathrand "math/rand"
	"strconv"

	"golang.org/x/tools/go/ssa"
)

func addJunkBlocks(ssaFunc *ssa.Function, count int, obfRand *mathrand.Rand) {
	if count == 0 {
		return
	}
	var candidates []*ssa.BasicBlock
	for _, block := range ssaFunc.Blocks {
		if len(block.Succs) > 0 {
			candidates = append(candidates, block)
		}
	}

	for i := 0; i < count; i++ {
		targetBlock := candidates[obfRand.Intn(len(candidates))]
		succsIdx := obfRand.Intn(len(targetBlock.Succs))
		succs := targetBlock.Succs[succsIdx]

		fakeBlock := &ssa.BasicBlock{
			Comment: "ctrflow.fake." + strconv.Itoa(i),
			Instrs:  []ssa.Instruction{&ssa.Jump{}},
			Preds:   []*ssa.BasicBlock{targetBlock},
			Succs:   []*ssa.BasicBlock{succs},
		}
		targetBlock.Succs[succsIdx] = fakeBlock

		ssaFunc.Blocks = append(ssaFunc.Blocks, fakeBlock)
		candidates = append(candidates, fakeBlock)
	}
}
