package main

import "testing"

func TestPkgCacheCopyFromReflectionParams(t *testing.T) {
	const method = "test.Interface.Inspect"
	left := pkgCache{
		ReflectAPIs:        map[string]map[int]bool{method: {0: true}},
		ReflectObjectNames: map[string]string{},
	}
	right := pkgCache{
		ReflectAPIs:        map[string]map[int]bool{method: {1: true}},
		ReflectObjectNames: map[string]string{},
	}
	left.CopyFrom(right)
	for _, pos := range []int{0, 1} {
		if !left.ReflectAPIs[method][pos] {
			t.Errorf("reflection parameter %d was lost when merging package caches", pos)
		}
	}
}
