// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"os"
	"strings"
)

// tinyFuncPkgPathSource keeps runtime.funcpkgpath from slicing an empty
// function name.
//
// Tiny mode replaces the name of every unexported function in the name table
// with one shared empty entry, so funcname returns "" for a function such as
// main.init. funcpkgpath starts one byte before the end of that name and walks
// it looking for the package separator, which indexes the string at -1 and
// panics inside the runtime. GODEBUG=inittrace=1 takes exactly that path once
// per package in doInit1; the panic is then converted into a throw because it
// is raised in runtime code, and tiny mode has already stubbed the fatal error
// prints, so the program left with status 2 without writing anything at all.
//
// There is no package path to report for a nameless function, so the trace
// reports an empty one. The trace itself still cannot be printed in tiny mode,
// since its print calls are stubbed like every other runtime print.
func tinyFuncPkgPathSource(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	src := string(data)
	const sig = "func funcpkgpath(f funcInfo) string {"
	start := strings.Index(src, sig)
	if start < 0 {
		return "", fmt.Errorf("tiny funcpkgpath patch did not match funcpkgpath")
	}
	// The guard has to follow the name declaration rather than the signature,
	// because a covered build compiles cmd/cover's copy of the file, where a
	// counter statement precedes the body's first line.
	const nameDecl = "\n	name := funcNameForPrint(funcname(f))\n"
	decl := strings.Index(src[start:], nameDecl)
	if decl < 0 {
		return "", fmt.Errorf("tiny funcpkgpath patch did not match funcpkgpath")
	}
	insertAt := start + decl + len(nameDecl)
	const guard = `	if name == "" {
		// Tiny mode empties the name table for unexported functions, and an
		// invalid funcInfo has no name either.
		return ""
	}
`
	return src[:insertAt] + guard + src[insertAt:], nil
}
