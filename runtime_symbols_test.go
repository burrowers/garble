// Copyright 2026 The Garble Authors
// SPDX-License-Identifier: BSD-3-Clause

package main

import "testing"

func TestRuntimeGoexitToolchainDependency(t *testing.T) {
	if !isToolchainNameDependency("runtime", "goexit") {
		t.Fatal("runtime.goexit must keep its assembly name for runtime stack metadata")
	}
	found := false
	for _, name := range builtinSymbols["runtime"] {
		if name == "goexit" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("runtime.goexit must be included in the linker symbol map")
	}
}

func TestStructsHostLayoutToolchainDependency(t *testing.T) {
	if !isToolchainNameDependency("structs", "HostLayout") {
		t.Fatal("structs.HostLayout must keep its name for go:wasmimport validation")
	}
}
