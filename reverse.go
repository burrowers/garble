// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"io"
	"os"
	"regexp"
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

	if err := rejectUnknownBuildFlags(flags); err != nil {
		return err
	}

	// A package's names are generally hashed with the action ID of its
	// obfuscated build. We recorded those action IDs above.
	// Note that we parse Go files directly to obtain the names, since the
	// export data only exposes exported names. Parsing Go files is cheap,
	// so it's unnecessary to try to avoid this cost.
	var replaces []string
	positions := make(map[string]string)

	for _, lpkg := range sharedCache.ListedPackages.all() {
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

		tf, files, err := transformerForListedPackage(lpkg)
		if err != nil {
			return err
		}
		for i, file := range files {
			goFile := lpkg.CompiledGoFiles[i]
			addPosition := func(nodePos token.Pos) {
				pos := fset.Position(nodePos)
				origPos := fmt.Sprintf("%s:%d", goFile, pos.Offset)
				newFilename := hashWithPackage(lpkg, origPos) + ".go"
				original := fmt.Sprintf("%s:%d", goFile, pos.Line)
				positions[newFilename] = original

				// A relative filename in a "//line" directive is recorded
				// relative to the package's import path, so positions read
				// as "obfuscatedpkg/obfuscated.go". We only replace the
				// filename, as the import path before it is replaced above.
				replaces = append(replaces,
					newFilename+":1", original,
					newFilename, goFile,
				)
			}
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
						obj, _ := tf.info.ObjectOf(name).(*types.Var)
						if obj == nil || !obj.IsField() {
							continue
						}
						originObj := obj.Origin()
						strct := tf.fieldToStruct[originObj]
						if strct == nil {
							panic("could not find struct for field " + name.Name)
						}
						replaces = append(replaces, hashWithStruct(strct, originObj), name.Name)
					}

				case *ast.CallExpr:
					// Reverse position information of call sites.
					addPosition(node.Pos())
				case ast.Expr:
					// Literal obfuscation turns a string constant expression into
					// a decoder call at the expression's position. That call can
					// set the position of a following call on the same line.
					tv := tf.info.Types[node]
					if flagLiterals && tv.Value != nil && tv.Value.Kind() == constant.String {
						addPosition(node.Pos())
					}
				}
			}
		}
	}
	repl := strings.NewReplacer(replaces...)

	if len(args) == 0 {
		modified, err := reverseContent(os.Stdout, os.Stdin, repl, positions)
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
		modified, err := reverseContent(os.Stdout, f, repl, positions)
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

var obfuscatedPosition = regexp.MustCompile(`[A-Za-z0-9_]+\.go:[1-9][0-9]*`)

func reverseContent(w io.Writer, r io.Reader, repl *strings.Replacer, positions map[string]string) (bool, error) {
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
		originalLine := line
		if len(positions) > 0 {
			// The compiler can report :2 (or later) when generated code
			// spans lines after a //line directive that started at :1.
			line = obfuscatedPosition.ReplaceAllStringFunc(line, func(pos string) string {
				name, _, _ := strings.Cut(pos, ":")
				if original, ok := positions[name]; ok {
					return original
				}
				return pos
			})
		}
		newLine := repl.Replace(line)
		if newLine != originalLine {
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
