// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

// A simple main package with some names to obfuscate.
// No dependencies, since each benchmark iteration will rebuild all deps.

package main

var globalVar = "global value"

func globalFunc() { println("global func body") }

func main() {
	println(globalVar)
	globalFunc()
}
