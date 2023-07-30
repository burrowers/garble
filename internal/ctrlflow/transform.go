package ctrlflow

import (
	"go/token"
	"go/types"
	mathrand "math/rand"
	"strconv"

	"golang.org/x/tools/go/ssa"
)

type blockMapping struct {
	Fake, Target *ssa.BasicBlock
}

type cfgInfo struct {
	CompareVar ssa.Value
	StoreVar   ssa.Value
}

type dispatcherInfo []cfgInfo

// applyFlattening adds a dispatcher block and uses ssa.Phi to redirect all ssa.Jump and ssa.If to the dispatcher,
// additionally shuffle all blocks
func applyFlattening(ssaFunc *ssa.Function, obfRand *mathrand.Rand) dispatcherInfo {
	if len(ssaFunc.Blocks) < 3 {
		return nil
	}

	phiInstr := &ssa.Phi{Comment: "ctrflow.phi"}
	setType(phiInstr, types.Typ[types.Int])

	entryBlock := &ssa.BasicBlock{
		Comment: "ctrflow.entry",
		Instrs:  []ssa.Instruction{phiInstr},
	}
	setBlockParent(entryBlock, ssaFunc)

	makeJumpBlock := func(from *ssa.BasicBlock) *ssa.BasicBlock {
		jumpBlock := &ssa.BasicBlock{
			Comment: "ctrflow.jump",
			Instrs:  []ssa.Instruction{&ssa.Jump{}},
			Preds:   []*ssa.BasicBlock{from},
			Succs:   []*ssa.BasicBlock{entryBlock},
		}
		setBlockParent(jumpBlock, ssaFunc)
		return jumpBlock
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
			// control flow flattening is not applicable
		default:
			panic("unreachable")
		}
	}

	phiIdxs := obfRand.Perm(len(blocksMapping))
	for i := range phiIdxs {
		phiIdxs[i]++ // 0 reserved for real entry block
	}

	var info dispatcherInfo

	var entriesBlocks []*ssa.BasicBlock
	obfuscatedBlocks := ssaFunc.Blocks
	for i, m := range blocksMapping {
		entryBlock.Preds = append(entryBlock.Preds, m.Fake)
		val := phiIdxs[i]
		cfg := cfgInfo{StoreVar: makeSsaInt(val), CompareVar: makeSsaInt(val)}
		info = append(info, cfg)

		phiInstr.Edges = append(phiInstr.Edges, cfg.StoreVar)
		obfuscatedBlocks = append(obfuscatedBlocks, m.Fake)

		cond := &ssa.BinOp{X: phiInstr, Op: token.EQL, Y: cfg.CompareVar}
		setType(cond, types.Typ[types.Bool])

		*phiInstr.Referrers() = append(*phiInstr.Referrers(), cond)

		ifInstr := &ssa.If{Cond: cond}
		*cond.Referrers() = append(*cond.Referrers(), ifInstr)

		ifBlock := &ssa.BasicBlock{
			Instrs: []ssa.Instruction{cond, ifInstr},
			Succs:  []*ssa.BasicBlock{m.Target, nil}, // false branch fulfilled in next iteration or linked to real entry block
		}
		setBlockParent(ifBlock, ssaFunc)

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
	return info
}

// addJunkBlocks adds junk jumps into random blocks. Can create chains of junk jumps.
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

	if len(candidates) == 0 {
		return
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
		setBlockParent(fakeBlock, ssaFunc)
		targetBlock.Succs[succsIdx] = fakeBlock

		ssaFunc.Blocks = append(ssaFunc.Blocks, fakeBlock)
		candidates = append(candidates, fakeBlock)
	}
}

// applySplitting splits biggest block into 2 parts of random size.
// Returns false if no block large enough for splitting is found
func applySplitting(ssaFunc *ssa.Function, obfRand *mathrand.Rand) bool {
	var targetBlock *ssa.BasicBlock
	for _, block := range ssaFunc.Blocks {
		if targetBlock == nil || len(block.Instrs) > len(targetBlock.Instrs) {
			targetBlock = block
		}
	}

	const minInstrCount = 1 + 1 // 1 exit instruction + 1 any instruction
	if targetBlock == nil || len(targetBlock.Instrs) <= minInstrCount {
		return false
	}

	splitIdx := 1 + obfRand.Intn(len(targetBlock.Instrs)-2)

	firstPart := make([]ssa.Instruction, splitIdx+1)
	copy(firstPart, targetBlock.Instrs)
	firstPart[len(firstPart)-1] = &ssa.Jump{}

	secondPart := targetBlock.Instrs[splitIdx:]
	targetBlock.Instrs = firstPart

	newBlock := &ssa.BasicBlock{
		Comment: "ctrflow.split." + strconv.Itoa(targetBlock.Index),
		Instrs:  secondPart,
		Preds:   []*ssa.BasicBlock{targetBlock},
		Succs:   targetBlock.Succs,
	}
	setBlockParent(newBlock, ssaFunc)
	for _, instr := range newBlock.Instrs {
		setBlock(instr, newBlock)
	}

	// Fix preds for ssa.Phi working
	for _, succ := range targetBlock.Succs {
		for i, pred := range succ.Preds {
			if pred == targetBlock {
				succ.Preds[i] = newBlock
			}
		}
	}

	ssaFunc.Blocks = append(ssaFunc.Blocks, newBlock)
	targetBlock.Succs = []*ssa.BasicBlock{newBlock}
	return true
}

func fixBlockIndexes(ssaFunc *ssa.Function) {
	for i, block := range ssaFunc.Blocks {
		block.Index = i
	}
}
