// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"io"
	"os"
	"strings"
)

// commandReverse implements "garble reverse".
func commandReverse(args []string) error {
	flags, args := splitFlagsFromArgs(args)
	if hasHelpFlag(flags) || len(args) == 0 {
		fmt.Fprint(os.Stderr, `
usage: garble [garble flags] reverse [build flags] package [files]

For example, after building an obfuscated program as follows:

	garble -literals build -tags=mytag ./cmd/mycmd

One can reverse a captured panic stack trace as follows:

	garble -literals reverse -tags=mytag ./cmd/mycmd panic-output.txt
`[1:])
		return errJustExit(2)
	}

	pkg, args := args[0], args[1:]
	// We don't actually run `go list -toolexec=garble`; we only use toolexecCmd
	// to ensure that sharedCache.ListedPackages is filled.
	_, err := toolexecCmd("list", append(flags, pkg))
	defer os.RemoveAll(os.Getenv("GARBLE_SHARED"))
	if err != nil {
		return err
	}

	// We don't actually run a main Go command with all flags,
	// so if the user gave a non-build flag,
	// we need this check to not silently ignore it.
	if _, firstUnknown := filterForwardBuildFlags(flags); firstUnknown != "" {
		// A bit of a hack to get a normal flag.Parse error.
		// Longer term, "reverse" might have its own FlagSet.
		return flag.NewFlagSet("", flag.ContinueOnError).Parse([]string{firstUnknown})
	}

	// A package's names are generally hashed with the action ID of its
	// obfuscated build. We recorded those action IDs above.
	// Note that we parse Go files directly to obtain the names, since the
	// export data only exposes exported names. Parsing Go files is cheap,
	// so it's unnecessary to try to avoid this cost.
	var replaces []string

	for _, lpkg := range sharedCache.ListedPackages {
		if !lpkg.ToObfuscate {
			continue
		}
		addHashedWithPackage := func(str string) {
			replaces = append(replaces, hashWithPackage(lpkg, str), str)
		}

		// Package paths are obfuscated, too.
		addHashedWithPackage(lpkg.ImportPath)

		// Assembly filenames are obfuscated in a simple way.
		// Mirroring [transformer.transformAsm]; note the lack of a test
		// as so far this has only mattered for build errors with positions.
		for _, name := range lpkg.SFiles {
			newName := hashWithPackage(lpkg, name) + ".s"
			replaces = append(replaces, newName, name)
		}

		files, err := parseFiles(lpkg, lpkg.Dir, lpkg.CompiledGoFiles)
		if err != nil {
			return err
		}
		origImporter := importerForPkg(lpkg)
		_, info, err := typecheck(lpkg.ImportPath, files, origImporter)
		if err != nil {
			return err
		}
		fieldToStruct := computeFieldToStruct(info)
		for i, file := range files {
			goFile := lpkg.CompiledGoFiles[i]
			for node := range ast.Preorder(file) {
				switch node := node.(type) {

				// Replace names.
				// TODO: do var names ever show up in output?
				case *ast.FuncDecl:
					addHashedWithPackage(node.Name.Name)
				case *ast.TypeSpec:
					addHashedWithPackage(node.Name.Name)
				case *ast.Field:
					for _, name := range node.Names {
						obj, _ := info.ObjectOf(name).(*types.Var)
						if obj == nil || !obj.IsField() {
							continue
						}
						strct := fieldToStruct[obj]
						if strct == nil {
							panic("could not find struct for field " + name.Name)
						}
						replaces = append(replaces, hashWithStruct(strct, obj), name.Name)
					}

				case *ast.CallExpr:
					// Reverse position information of call sites.
					pos := fset.Position(node.Pos())
					origPos := fmt.Sprintf("%s:%d", goFile, pos.Offset)
					newFilename := hashWithPackage(lpkg, origPos) + ".go"

					// Do "obfuscated.go:1", corresponding to the call site's line.
					// Most common in stack traces.
					replaces = append(replaces,
						newFilename+":1",
						fmt.Sprintf("%s/%s:%d", lpkg.ImportPath, goFile, pos.Line),
					)

					// Do "obfuscated.go" as a fallback.
					// Most useful in build errors in obfuscated code,
					// since those might land on any line.
					// Any ":N" line number will end up being useless,
					// but at least the filename will be correct.
					replaces = append(replaces,
						newFilename,
						fmt.Sprintf("%s/%s", lpkg.ImportPath, goFile),
					)
				}
			}
		}
	}
	repl := strings.NewReplacer(replaces...)

	if len(args) == 0 {
		modified, err := reverseContent(os.Stdout, os.Stdin, repl)
		if err != nil {
			return err
		}
		if !modified {
			return errJustExit(1)
		}
		return nil
	}
	// TODO: cover this code in the tests too
	anyModified := false
	for _, path := range args {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		modified, err := reverseContent(os.Stdout, f, repl)
		if err != nil {
			return err
		}
		anyModified = anyModified || modified
		f.Close() // since we're in a loop
	}
	if !anyModified {
		return errJustExit(1)
	}
	return nil
}

func reverseContent(w io.Writer, r io.Reader, repl *strings.Replacer) (bool, error) {
	// Read line by line.
	// Reading the entire content at once wouldn't be interactive,
	// nor would it support large files well.
	// Reading entire lines ensures we don't cut words in half.
	// We use bufio.Reader instead of bufio.Scanner,
	// to also obtain the newline characters themselves.
	br := bufio.NewReader(r)
	modified := false
	for {
		// Note that ReadString can return a line as well as an error if
		// we hit EOF without a newline.
		// In that case, we still want to process the string.
		line, readErr := br.ReadString('\n')

		newLine := repl.Replace(line)
		if newLine != line {
			modified = true
		}
		if _, err := io.WriteString(w, newLine); err != nil {
			return modified, err
		}
		if readErr == io.EOF {
			return modified, nil
		}
		if readErr != nil {
			return modified, readErr
		}
	}
}
