// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import "fmt"

var globalVar = "global value"

func globalFunc() { fmt.Println("global func body") }

func main() {
	fmt.Println(globalVar)
	globalFunc()
}
