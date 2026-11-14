// Copyright (c) 2026 The Garble Authors.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"log"
	"os"

	"mvdan.cc/garble/internal/patcher"
)

func main() {
	goRoot, goVersion := os.Args[1], os.Args[2]
	goSrc := os.Getenv(patcher.GoSrcEnv)
	cache, tmp := os.Getenv("GARBLE_CACHE"), os.Getenv("TMPDIR")
	tools := make(map[string]string)
	for _, tool := range []string{"compile", "asm", "link"} {
		var path string
		var err error
		switch tool {
		case "compile":
			path, err = patcher.PatchCompiler(goSrc, goRoot, goVersion, cache, tmp)
		case "asm":
			path, err = patcher.PatchAssembler(goSrc, goRoot, goVersion, cache, tmp)
		case "link":
			path, err = patcher.PatchLinker(goRoot, goVersion, cache, tmp)
		}
		if err != nil {
			log.Fatal(err)
		}
		tools[tool] = path
	}
	data, err := json.Marshal(tools)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(os.Args[3], data, 0o600); err != nil {
		log.Fatal(err)
	}
}
