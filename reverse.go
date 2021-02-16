// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	cmd, err := toolexecCmd("list", listArgs)
	if err != nil {
		return err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("go list error: %v", err)
	}
	mainPkgPath := ""
	dec := json.NewDecoder(stdout)
	var privatePkgPaths []string
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return err
		}
		if pkg.Export == "" {
			continue
		}
		if pkg.Name == "main" {
			if mainPkgPath != "" {
				return fmt.Errorf("found two main packages: %s %s", mainPkgPath, pkg.ImportPath)
			}
			mainPkgPath = pkg.ImportPath
		}
		if isPrivate(pkg.ImportPath) {
			privatePkgPaths = append(privatePkgPaths, pkg.ImportPath)
		}
		buildID, err := buildidOf(pkg.Export)
		if err != nil {
			return err
		}
		// Adding it to buildInfo.imports allows us to reuse the
		// "if" branch below. Plus, if this edge case triggers
		// multiple times in a single package compile, we can
		// call "go list" once and cache its result.
		buildInfo.imports[pkg.ImportPath] = importedPkg{
			packagefile: pkg.Export,
			actionID:    decodeHash(splitActionID(buildID)),
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go list error: %v: %s", err, stderr.Bytes())
	}

	var replaces []string

	for _, pkgPath := range privatePkgPaths {
		ipkg := buildInfo.imports[pkgPath]

		// All original exported names names are hashed with the
		// obfuscated package's action ID.
		tpkg, err := origImporter.Import(pkgPath)
		if err != nil {
			return err
		}
		pkgScope := tpkg.Scope()
		for _, name := range pkgScope.Names() {
			obj := pkgScope.Lookup(name)
			if !obj.Exported() {
				continue
			}
			replaces = append(replaces, hashWith(ipkg.actionID, name), name)
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
