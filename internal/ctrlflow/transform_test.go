// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package ctrlflow

import (
	"testing"

	qt "github.com/go-quicktest/qt"
)

// TestBlockSplittingPhiBoundary checks that splitting never separates a block's
// leading phis from the block itself. A phi's edges are indexed by the
// predecessor list of its own block, so a phi moved into a new block with a
// single predecessor would keep edges for predecessors it no longer has.
// ssa2ast indexes that predecessor list per edge and panics when the counts
// differ.
func TestBlockSplittingPhiBoundary(t *testing.T) {
	const src = `package main

//garble:controlflow block_splits=max flatten_passes=1 junk_jumps=0
func classify(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += "x"
	}
	return out
}

func main() {
	_ = classify(3)
}
`

	for seed := int64(0); seed < 64; seed++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("seed %d: Obfuscate panicked: %v", seed, r)
				}
			}()
			_, newFile, _ := obfuscateSource(t, src, seed)
			qt.Assert(t, qt.IsNotNil(newFile))
		}()
	}
}
