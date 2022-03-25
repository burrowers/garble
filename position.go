// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"path/filepath"
	"strings"

	"golang.org/x/exp/slices"
)

func isDirective(text string) bool {
	// TODO: can we remove the check for "// +build" now that we require Go 1.18
	// or later? we should update the tests too.
	return strings.HasPrefix(text, "//go:") || strings.HasPrefix(text, "// +build")
}

var printBuf1, printBuf2 bytes.Buffer

// printFile prints a Go file to a buffer, while also removing non-directive
// comments and adding extra compiler directives to obfuscate position
// information.
func printFile(file1 *ast.File) ([]byte, error) {
	printConfig := printer.Config{Mode: printer.RawFormat}

	printBuf1.Reset()
	if err := printConfig.Fprint(&printBuf1, fset, file1); err != nil {
		return nil, err
	}
	src := printBuf1.Bytes()

	if !curPkg.ToObfuscate {
		// TODO(mvdan): make transformCompile handle untouched
		// packages like runtime earlier on, to remove these checks.
		return src, nil
	}

	absFilename := fset.Position(file1.Pos()).Filename
	filename := filepath.Base(absFilename)
	if strings.HasPrefix(filename, "_cgo_") {
		// cgo-generated files don't need changed line numbers.
		// Plus, the compiler can complain rather easily.
		return src, nil
	}

	// Many parts of garble, notably the literal obfuscator, modify the AST.
	// Unfortunately, comments are free-floating in File.Comments,
	// and those are the only source of truth that go/printer uses.
	// So the positions of the comments in the given file are wrong.
	// The only way we can get the final ones is to parse again.
	//
	// We use an empty filename here.
	// Syntax errors should be rare, and when they do happen,
	// we don't want to point to the original source file on disk.
	// That would be confusing, as we've changed the source in memory.
	file2, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("re-parse error: %w", err)
	}

	// Remove any comments by making them whitespace.
	// Keep directives, as they affect the build.
	// This is superior to removing the comments before printing,
	// because then the final source would have different line numbers.
	for _, group := range file2.Comments {
		for _, comment := range group.List {
			if isDirective(comment.Text) {
				continue
			}
			start := fset.Position(comment.Pos()).Offset
			end := fset.Position(comment.End()).Offset
			for i := start; i < end; i++ {
				src[i] = ' '
			}
		}
	}

	var origCallExprs []*ast.CallExpr
	ast.Inspect(file1, func(node ast.Node) bool {
		if node, ok := node.(*ast.CallExpr); ok {
			origCallExprs = append(origCallExprs, node)
		}
		return true
	})

	// Keep the compiler directives, and change position info.
	type commentToAdd struct {
		offset int
		text   string
	}
	var toAdd []commentToAdd
	i := 0
	ast.Inspect(file2, func(node ast.Node) bool {
		node, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		origNode := origCallExprs[i]
		i++
		newName := ""
		if !flagTiny {
			origPos := fmt.Sprintf("%s:%d", filename, fset.Position(origNode.Pos()).Offset)
			newName = hashWithPackage(curPkg, origPos) + ".go"
			// log.Printf("%q hashed with %x to %q", origPos, curPkg.GarbleActionID, newName)
		}
		pos := fset.Position(node.Pos())

		// We use the "/*text*/" form, since we can use multiple of them
		// on a single line, and they don't require extra newlines.
		toAdd = append(toAdd, commentToAdd{
			offset: pos.Offset,
			text:   fmt.Sprintf("/*line %s:1*/", newName),
		})
		return true
	})

	// We add comments in order.
	slices.SortFunc(toAdd, func(a, b commentToAdd) bool {
		return a.offset < b.offset
	})

	copied := 0
	printBuf2.Reset()

	// Make sure the entire file gets a zero filename by default,
	// in case we miss any positions below.
	// We use a //-style comment, because there might be build tags.
	// toAdd is for /*-style comments, so add it to printBuf2 directly.
	printBuf2.WriteString("//line :1\n")

	for _, comment := range toAdd {
		printBuf2.Write(src[copied:comment.offset])
		copied = comment.offset

		// We assume that all comments are of the form "/*text*/".
		// Make sure there is whitespace at either side of a comment.
		// Otherwise, we could change the syntax of the program.
		// Inserting "/*text*/" in "a/b" // must be "a/ /*text*/ b",
		// as "a//*text*/b" is tokenized as a "//" comment.
		printBuf2.WriteByte(' ')
		printBuf2.WriteString(comment.text)
		printBuf2.WriteByte(' ')
	}
	printBuf2.Write(src[copied:])
	return printBuf2.Bytes(), nil
}
