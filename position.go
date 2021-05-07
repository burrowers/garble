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
	"sort"
	"strings"
)

func isDirective(text string) bool {
	return strings.HasPrefix(text, "//go:") || strings.HasPrefix(text, "// +build")
}

// printFile prints a Go file to a buffer, while also removing non-directive
// comments and adding extra compiler directives to obfuscate position
// information.
func printFile(file1 *ast.File) ([]byte, error) {
	printConfig := printer.Config{Mode: printer.RawFormat}

	var buf1 bytes.Buffer
	if err := printConfig.Fprint(&buf1, fset, file1); err != nil {
		return nil, err
	}
	src := buf1.Bytes()

	if !curPkg.Private {
		// TODO(mvdan): make transformCompile handle non-private
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

	// Keep the compiler directives, and change position info.
	type commentToAdd struct {
		offset int
		text   string
	}
	var toAdd []commentToAdd
	addComment := func(offset int, text string) {
		toAdd = append(toAdd, commentToAdd{offset, text})
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

	i := 0
	ast.Inspect(file2, func(node ast.Node) bool {
		node, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		origNode := origCallExprs[i]
		i++
		newName := ""
		if !opts.Tiny {
			origPos := fmt.Sprintf("%s:%d", filename, fset.Position(origNode.Pos()).Offset)
			newName = hashWith(curPkg.GarbleActionID, origPos) + ".go"
			// log.Printf("%q hashed with %x to %q", origPos, curPkg.GarbleActionID, newName)
		}
		newPos := fmt.Sprintf("%s:1", newName)
		pos := fset.Position(node.Pos())

		// We use the "/*text*/" form, since we can use multiple of them
		// on a single line, and they don't require extra newlines.
		addComment(pos.Offset, "/*line "+newPos+"*/")
		return true
	})

	// We add comments in order.
	sort.Slice(toAdd, func(i, j int) bool {
		return toAdd[i].offset < toAdd[j].offset
	})

	copied := 0
	var buf2 bytes.Buffer

	// Make sure the entire file gets a zero filename by default,
	// in case we miss any positions below.
	// We use a //-style comment, because there might be build tags.
	// addComment is for /*-style comments, so add it to buf2 directly.
	buf2.WriteString("//line :1\n")

	for _, comment := range toAdd {
		buf2.Write(src[copied:comment.offset])
		copied = comment.offset

		// We assume that all comments are of the form "/*text*/".
		// Make sure there is whitespace at either side of a comment.
		// Otherwise, we could change the syntax of the program.
		// Inserting "/*text*/" in "a/b" // must be "a/ /*text*/ b",
		// as "a//*text*/b" is tokenized as a "//" comment.
		buf2.WriteByte(' ')
		buf2.WriteString(comment.text)
		buf2.WriteByte(' ')
	}
	buf2.Write(src[copied:])
	return buf2.Bytes(), nil
}
