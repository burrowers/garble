// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/scanner"
	"go/token"
	"path/filepath"
	"strings"
)

var printBuf1, printBuf2 bytes.Buffer

// printFile prints a Go file to a buffer, while also removing non-directive
// comments and adding extra compiler directives to obfuscate position
// information.
func printFile(file *ast.File) ([]byte, error) {
	printConfig := printer.Config{Mode: printer.RawFormat}

	printBuf1.Reset()
	if err := printConfig.Fprint(&printBuf1, fset, file); err != nil {
		return nil, err
	}
	src := printBuf1.Bytes()

	if !curPkg.ToObfuscate {
		// We lightly transform packages which shouldn't be obfuscated,
		// such as when rewriting go:linkname directives to obfuscated packages.
		// We still need to print the files, but without obfuscating positions.
		return src, nil
	}

	fsetFile := fset.File(file.Pos())
	filename := filepath.Base(fsetFile.Name())
	if strings.HasPrefix(filename, "_cgo_") {
		// cgo-generated files don't need changed line numbers.
		// Plus, the compiler can complain rather easily.
		return src, nil
	}

	// Many parts of garble, notably the literal obfuscator, modify the AST.
	// Unfortunately, comments are free-floating in File.Comments,
	// and those are the only source of truth that go/printer uses.
	// So the positions of the comments in the given file are wrong.
	// The only way we can get the final ones is to tokenize again.
	// Using go/scanner is slightly awkward, but cheaper than parsing again.

	// We want to use the original positions for the hashed positions.
	// Since later we'll iterate on tokens rather than walking an AST,
	// we use a list of offsets indexed by identifiers in source order.
	var origCallOffsets []int
	nextOffset := -1
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			nextOffset = fsetFile.Position(node.Pos()).Offset
		case *ast.Ident:
			origCallOffsets = append(origCallOffsets, nextOffset)
			nextOffset = -1
		}
		return true
	})

	copied := 0
	printBuf2.Reset()

	// Make sure the entire file gets a zero filename by default,
	// in case we miss any positions below.
	// We use a //-style comment, because there might be build tags.
	// toAdd is for /*-style comments, so add it to printBuf2 directly.
	printBuf2.WriteString("//line :1\n")

	// We use an empty filename when tokenizing below.
	// We use a nil go/scanner.ErrorHandler because src comes from go/printer.
	// Syntax errors should be rare, and when they do happen,
	// we don't want to point to the original source file on disk.
	// That would be confusing, as we've changed the source in memory.
	var s scanner.Scanner
	fsetFile = fset.AddFile("", fset.Base(), len(src))
	s.Init(fsetFile, src, nil, scanner.ScanComments)

	identIndex := 0
	for {
		pos, tok, lit := s.Scan()
		switch tok {
		case token.EOF:
			// Copy the rest and return.
			printBuf2.Write(src[copied:])
			return printBuf2.Bytes(), nil
		case token.COMMENT:
			// Omit comments from the final Go code.
			// Keep directives, as they affect the build.
			// This is superior to removing the comments before printing,
			// because then the final source would have different line numbers.
			if strings.HasPrefix(lit, "//go:") {
				continue // directives are kept
			}
			offset := fsetFile.Position(pos).Offset
			printBuf2.Write(src[copied:offset])
			copied = offset + len(lit)
		case token.IDENT:
			origOffset := origCallOffsets[identIndex]
			identIndex++
			if origOffset == -1 {
				continue // identifiers which don't start func calls are left untouched
			}
			newName := ""
			if !flagTiny {
				origPos := fmt.Sprintf("%s:%d", filename, origOffset)
				newName = hashWithPackage(curPkg, origPos) + ".go"
				// log.Printf("%q hashed with %x to %q", origPos, curPkg.GarbleActionID, newName)
			}

			offset := fsetFile.Position(pos).Offset
			printBuf2.Write(src[copied:offset])
			copied = offset

			// We use the "/*text*/" form, since we can use multiple of them
			// on a single line, and they don't require extra newlines.
			// Make sure there is whitespace at either side of a comment.
			// Otherwise, we could change the syntax of the program.
			// Inserting "/*text*/" in "a/b" // must be "a/ /*text*/ b",
			// as "a//*text*/b" is tokenized as a "//" comment.
			fmt.Fprintf(&printBuf2, " /*line %s:1*/ ", newName)
		}
	}
}
