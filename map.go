// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"os"

	"golang.org/x/tools/go/types/objectpath"
)

// commandMap implements "garble map".
func commandMap(args []string) error {
	flags, pkgs := splitFlagsFromArgs(args)
	if hasHelpFlag(flags) || len(args) == 0 {
		fmt.Fprint(os.Stderr, `
usage: garble [garble flags] map [build flags] packages...

For example, after building an obfuscated program as follows:

	garble -literals build -tags=mytag ./cmd/mycmd

One can obtain an obfuscation map as follows:

	garble -literals map -tags=mytag ./cmd/mycmd
`[1:])
		return errJustExit(2)
	}

	listArgs := []string{
		"-json",
		"-deps",
		"-export",
	}
	listArgs = append(listArgs, flags...)
	listArgs = append(listArgs, pkgs...)
	// TODO: We most likely no longer need this "list -toolexec" call, since
	// we use the original build IDs.
	_, err := toolexecCmd("list", listArgs)
	defer os.RemoveAll(os.Getenv("GARBLE_SHARED"))
	if err != nil {
		return err
	}

	// We don't actually run a main Go command with all flags,
	// so if the user gave a non-build flag,
	// we need this check to not silently ignore it.
	if _, firstUnknown := filterForwardBuildFlags(flags); firstUnknown != "" {
		// A bit of a hack to get a normal flag.Parse error.
		// Longer term, "map" might have its own FlagSet.
		return flag.NewFlagSet("", flag.ContinueOnError).Parse([]string{firstUnknown})
	}

	// A package's names are generally hashed with the action ID of its
	// obfuscated build. We recorded those action IDs above.
	// Note that we parse Go files directly to obtain the names, since the
	// export data only exposes exported names. Parsing Go files is cheap,
	// so it's unnecessary to try to avoid this cost.

	type obfuscatedPackageInfo struct {
		Path    string                     `json:"path"`
		Objects map[objectpath.Path]string `json:"objects"`
	}

	result := make(map[string]obfuscatedPackageInfo, len(sharedCache.ListedPackages))

	for _, lpkg := range sharedCache.ListedPackages {
		if !lpkg.ToObfuscate {
			continue
		}

		tf := transformer{
			curPkg:       lpkg,
			origImporter: importerForPkg(lpkg),
		}

		objectMap := make(map[objectpath.Path]string)
		result[lpkg.ImportPath] = obfuscatedPackageInfo{
			Path:    hashWithPackage(&tf, lpkg, lpkg.ImportPath),
			Objects: objectMap,
		}

		files, err := parseFiles(lpkg.Dir, lpkg.CompiledGoFiles)
		if err != nil {
			return err
		}

		tf.pkg, tf.info, err = typecheck(lpkg.ImportPath, files, tf.origImporter)
		if err != nil {
			return err
		}

		tf.curPkgCache, err = loadPkgCache(lpkg, tf.pkg, files, tf.info, nil)
		if err != nil {
			return err
		}

		tf.fieldToStruct = computeFieldToStruct(tf.info)

		var encoder objectpath.Encoder
		visited := make(map[types.Object]bool) // Avoid duplicated work.

		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				switch node := node.(type) {
				case ast.Stmt:
					// Skip statements as local objects have no object path.
					return false

				case *ast.Ident:
					obj := tf.info.ObjectOf(node)
					if obj == nil || obj.Pkg() != tf.pkg || visited[obj] {
						return true
					}

					visited[obj] = true

					obfuscated := tf.obfuscateObjectName(obj)
					if obfuscated == obj.Name() {
						return true
					}

					// This is probably costlier than obfuscation:
					// run it only when necessary.
					path, err := encoder.For(obj)
					if err != nil {
						return true
					}

					objectMap[path] = obfuscated

				default:
					return true
				}

				return true
			})
		}
	}

	return json.NewEncoder(os.Stdout).Encode(result)
}
