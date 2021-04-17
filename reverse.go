// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// commandReverse implements "garble reverse".
func commandReverse(args []string) error {
	flags, args := splitFlagsFromArgs(args)
	mainPkg := "."
	if len(args) > 0 {
		mainPkg = args[0]
		args = args[1:]
	}

	listArgs := []string{
		"-json",
		"-deps",
		"-export",
	}
	listArgs = append(listArgs, flags...)
	listArgs = append(listArgs, mainPkg)
	// TODO: We most likely no longer need this "list -toolexec" call, since
	// we use the original build IDs.
	if _, err := toolexecCmd("list", listArgs); err != nil {
		return err
	}

	// We don't actually run a main Go command with all flags,
	// so if the user gave a non-build flag,
	// we need this check to not silently ignore it.
	if _, firstUnknown := filterBuildFlags(flags); firstUnknown != "" {
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
	fset := token.NewFileSet()

	for _, lpkg := range cache.ListedPackages {
		if !lpkg.Private {
			continue
		}
		addReplace := func(str string) {
			replaces = append(replaces, hashWith(lpkg.GarbleActionID, str), str)
		}

		// Package paths are obfuscated, too.
		addReplace(lpkg.ImportPath)

		for _, goFile := range lpkg.GoFiles {
			fullGoFile := filepath.Join(lpkg.Dir, goFile)
			file, err := parser.ParseFile(fset, fullGoFile, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch node := node.(type) {

				// Replace names.
				// TODO: do var names ever show up in output?
				case *ast.FuncDecl:
					addReplace(node.Name.Name)
				case *ast.TypeSpec:
					addReplace(node.Name.Name)
				case *ast.Field:
					for _, name := range node.Names {
						addReplace(name.Name)
					}

				case *ast.CallExpr:
					// continues below
				default:
					return true
				}

				// Reverse position information.
				pos := fset.Position(node.Pos())
				origPos := fmt.Sprintf("%s:%d", goFile, pos.Offset)
				newPos := hashWith(lpkg.GarbleActionID, origPos) + ".go:1"

				replaces = append(replaces,
					newPos,
					fmt.Sprintf("%s/%s:%d", lpkg.ImportPath, goFile, pos.Line),
				)

				return true
			})
		}
	}
	repl := strings.NewReplacer(replaces...)

	if len(args) == 0 {
		modified, err := reverseContent(os.Stdout, os.Stdin, repl)
		if err != nil {
			return err
		}
		if !modified {
			return errJustExit
		}
		return nil
	}
	anyModified := false
	for _, path := range args {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		any, err := reverseContent(os.Stdout, f, repl)
		if err != nil {
			return err
		}
		anyModified = anyModified || any
		f.Close() // since we're in a loop
	}
	if !anyModified {
		return errJustExit
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
