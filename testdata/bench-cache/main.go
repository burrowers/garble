// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

// A simple main package with some names to obfuscate.
// With relatively heavy dependencies, as benchmark iterations use the build cache.

package main

import (
	"fmt"
	"net/http"
)

var globalVar = "global value"

func globalFunc() { fmt.Println("global func body") }

func main() {
	fmt.Println(globalVar)
	globalFunc()
	http.ListenAndServe("", nil)
}
