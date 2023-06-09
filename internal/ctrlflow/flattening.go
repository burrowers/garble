package ctrlflow

import (
	"go/token"
	"go/types"
	mathrand "math/rand"
	_ "unsafe"

	"golang.org/x/tools/go/ssa"
)

type blockMapping struct {
	Fake, Target *ssa.BasicBlock
}

func applyControlFlowFlattening(ssaFunc *ssa.Function, obfRand *mathrand.Rand) {
	if len(ssaFunc.Blocks) < 3 {
		return
	}

	phiInstr := &ssa.Phi{Comment: "ctrflow.phi"}
	setType(phiInstr, types.Typ[types.Int])

	entryBlock := &ssa.BasicBlock{
		Comment: "ctrflow.entry",
		Instrs:  []ssa.Instruction{phiInstr},
	}

	makeJumpBlock := func(from *ssa.BasicBlock) *ssa.BasicBlock {
		return &ssa.BasicBlock{
			Comment: "ctrflow.jump",
			Instrs:  []ssa.Instruction{&ssa.Jump{}},
			Preds:   []*ssa.BasicBlock{from},
			Succs:   []*ssa.BasicBlock{entryBlock},
		}
	}

	// map for track fake block -> real block jump
	var blocksMapping []blockMapping
	for _, block := range ssaFunc.Blocks {
		existInstr := block.Instrs[len(block.Instrs)-1]
		switch existInstr.(type) {
		case *ssa.Jump:
			targetBlock := block.Succs[0]
			fakeBlock := makeJumpBlock(block)
			blocksMapping = append(blocksMapping, blockMapping{fakeBlock, targetBlock})
			block.Succs[0] = fakeBlock
		case *ssa.If:
			tblock, fblock := block.Succs[0], block.Succs[1]
			fakeTblock, fakeFblock := makeJumpBlock(tblock), makeJumpBlock(fblock)

			blocksMapping = append(blocksMapping, blockMapping{fakeTblock, tblock})
			blocksMapping = append(blocksMapping, blockMapping{fakeFblock, fblock})

			block.Succs[0] = fakeTblock
			block.Succs[1] = fakeFblock
		case *ssa.Return, *ssa.Panic:
			continue
		default:
			panic("unreachable")
		}
	}

	phiIdxs := obfRand.Perm(len(blocksMapping))
	for i := range phiIdxs {
		phiIdxs[i]++
	}

	var entriesBlocks []*ssa.BasicBlock
	obfuscatedBlocks := ssaFunc.Blocks
	for i, m := range blocksMapping {
		entryBlock.Preds = append(entryBlock.Preds, m.Fake)
		phiInstr.Edges = append(phiInstr.Edges, makeSsaInt(phiIdxs[i]))

		obfuscatedBlocks = append(obfuscatedBlocks, m.Fake)

		cond := &ssa.BinOp{X: phiInstr, Op: token.EQL, Y: makeSsaInt(phiIdxs[i])}
		setType(cond, types.Typ[types.Bool])

		*phiInstr.Referrers() = append(*phiInstr.Referrers(), cond)

		ifInstr := &ssa.If{Cond: cond}
		*cond.Referrers() = append(*cond.Referrers(), ifInstr)

		ifBlock := &ssa.BasicBlock{
			Instrs: []ssa.Instruction{cond, ifInstr},
			Succs:  []*ssa.BasicBlock{m.Target, nil}, // false branch fulfilled in next iteration or linked to real entry block
		}

		setBlock(cond, ifBlock)
		setBlock(ifInstr, ifBlock)
		entriesBlocks = append(entriesBlocks, ifBlock)

		if i == 0 {
			entryBlock.Instrs = append(entryBlock.Instrs, &ssa.Jump{})
			entryBlock.Succs = []*ssa.BasicBlock{ifBlock}
			ifBlock.Preds = append(ifBlock.Preds, entryBlock)
		} else {
			// link previous block to current
			entriesBlocks[i-1].Succs[1] = ifBlock
			ifBlock.Preds = append(ifBlock.Preds, entriesBlocks[i-1])
		}
	}

	lastFakeEntry := entriesBlocks[len(entriesBlocks)-1]

	realEntryBlock := ssaFunc.Blocks[0]
	lastFakeEntry.Succs[1] = realEntryBlock
	realEntryBlock.Preds = append(realEntryBlock.Preds, lastFakeEntry)

	obfuscatedBlocks = append(obfuscatedBlocks, entriesBlocks...)
	obfRand.Shuffle(len(obfuscatedBlocks), func(i, j int) {
		obfuscatedBlocks[i], obfuscatedBlocks[j] = obfuscatedBlocks[j], obfuscatedBlocks[i]
	})
	ssaFunc.Blocks = append([]*ssa.BasicBlock{entryBlock}, obfuscatedBlocks...)
	for i, block := range ssaFunc.Blocks {
		block.Index = i
	}
}
