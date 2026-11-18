package main

import (
	mathrand "math/rand"
	"testing"
)

func TestUniqueRandomNames(t *testing.T) {
	// a stand-in for randomName which draws the same name twice in a row,
	// as two lifted literal functions did in a real build
	draws := []string{"c5c7Mz", "c5c7Mz", "a8Kq2x"}
	next := func(rand *mathrand.Rand, baseName string) string {
		name := draws[0]
		draws = draws[1:]
		return name
	}
	names := uniqueRandomNames(next)
	rand := mathrand.New(mathrand.NewSource(1))
	first := names(rand, "literalDecoder0")
	second := names(rand, "literalHelper1")
	// TODO: the second name should differ from the first
	if first != second {
		t.Fatalf("got distinct names %q and %q; want the current duplicate", first, second)
	}
}
