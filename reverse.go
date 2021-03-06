// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bufio"
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
			goFile = filepath.Join(lpkg.Dir, goFile)
			file, err := parser.ParseFile(fset, goFile, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range file.Decls {
				// TODO: Probably do type names too. What else?
				switch decl := decl.(type) {
				case *ast.FuncDecl:
					addReplace(decl.Name.Name)
				}
			}
		}
	}
	repl := strings.NewReplacer(replaces...)

	// TODO: return a non-zero status code if we could not reverse any string.
	if len(args) == 0 {
		return reverseContent(os.Stdout, os.Stdin, repl)
	}
	for _, path := range args {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := reverseContent(os.Stdout, f, repl); err != nil {
			return err
		}
		f.Close() // since we're in a loop
	}
	return nil
}

func reverseContent(w io.Writer, r io.Reader, repl *strings.Replacer) error {
	// Read line by line.
	// Reading the entire content at once wouldn't be interactive,
	// nor would it support large files well.
	// Reading entire lines ensures we don't cut words in half.
	// We use bufio.Reader instead of bufio.Scanner,
	// to also obtain the newline characters themselves.
	br := bufio.NewReader(r)
	for {
		// Note that ReadString can return a line as well as an error if
		// we hit EOF without a newline.
		// In that case, we still want to process the string.
		line, readErr := br.ReadString('\n')
		if _, err := repl.WriteString(w, line); err != nil {
			return err
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
