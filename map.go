// Copyright (c) 2025, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/tools/go/types/objectpath"
)

// mapPackage describes how a single obfuscated package's names are obfuscated.
type mapPackage struct {
	// Path is the obfuscated import path of the package.
	Path string `json:"path"`

	// Objects maps each obfuscated object to its obfuscated name, keyed by its
	// [objectpath]. Non-obfuscated objects, and those with no object path as
	// they are not part of the public API, are omitted.
	Objects map[string]string `json:"objects"`
}

// commandMap implements "garble map". As the inverse of "garble reverse", it
// describes how each original name is obfuscated, as JSON keyed by import path.
// It is meant for devtools that predict garble's output, such as binding
// generators for garbled builds.
//
// The output covers names only, so it cannot reproduce a build, and it
// deobfuscates the public API; prefer piping it over writing it to disk.
func commandMap(args []string) error {
	flags, args := splitFlagsFromArgs(args)
	if hasHelpFlag(flags) || len(args) == 0 {
		fmt.Fprint(os.Stderr, `
usage: garble [garble flags] map [build flags] packages...

For example, to describe how the names in a module are obfuscated:

	garble map ./...

The output is a JSON object keyed by original import path:

	{
		"example.com/foo": {
			"path": "obfuscated/import/path",
			"objects": {
				"Type":          "obfuscatedName",
				"Type.Field":    "obfuscatedName",
				"Type.Method":   "obfuscatedName",
				"Func":          "obfuscatedName"
			}
		}
	}

Object keys follow golang.org/x/tools/go/types/objectpath, so only obfuscated
objects which are part of a package's public API are listed.

Run "garble map" with the same garble flags used to build, since flags such as
-seed and -tiny change the obfuscated names.
`[1:])
		return errJustExit(2)
	}

	// We don't run a real build, just "go list" to fill sharedCache.ListedPackages.
	_, err := toolexecCmd("list", append(flags, args...))
	defer os.RemoveAll(os.Getenv("GARBLE_SHARED"))
	if err != nil {
		return err
	}

	if err := rejectUnknownBuildFlags(flags); err != nil {
		return err
	}

	result := make(map[string]mapPackage)
	for _, lpkg := range sharedCache.ListedPackages.all() {
		if !lpkg.ToObfuscate {
			continue
		}
		tf, _, err := transformerForListedPackage(lpkg)
		if err != nil {
			return err
		}

		// info.Defs holds every object defined in the package; objectpath keeps
		// only those reachable through the public API.
		var enc objectpath.Encoder
		objects := make(map[string]string)
		for _, obj := range tf.info.Defs {
			if obj == nil {
				continue
			}
			// Skip function parameters, results, and type parameters; only
			// package-level objects, fields, and methods have meaningful names.
			if parent := obj.Parent(); parent != nil && parent != tf.pkg.Scope() {
				continue
			}
			newName, ok := tf.obfuscatedObjectName(obj)
			if !ok {
				continue // not obfuscated
			}
			// Computing the object path isn't cheap, so do it only once obfuscated.
			path, err := enc.For(obj)
			if err != nil {
				continue // not part of the public API
			}
			objects[string(path)] = newName
		}

		result[lpkg.ImportPath] = mapPackage{
			Path:    lpkg.obfuscatedImportPath(),
			Objects: objects,
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "\t")
	return enc.Encode(result)
}
