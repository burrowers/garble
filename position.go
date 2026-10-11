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
// comments and adding extra compiler directives to obfuscate position information.
func printFile(lpkg *listedPackage, file *ast.File) ([]byte, error) {
	if lpkg.toObfuscate() {
		// Omit comments from the final Go code.
		// Keep directives, as they affect the build.
		// We do this before printing to print fewer bytes below.
		var newComments []*ast.CommentGroup
		for _, group := range file.Comments {
			var newGroup ast.CommentGroup
			for _, comment := range group.List {
				if isGoDirective(comment.Text) {
					newGroup.List = append(newGroup.List, comment)
				}
			}
			if len(newGroup.List) > 0 {
				newComments = append(newComments, &newGroup)
			}
		}
		file.Comments = newComments
	}

	printBuf1.Reset()
	printConfig := printer.Config{Mode: printer.RawFormat}
	if err := printConfig.Fprint(&printBuf1, fset, file); err != nil {
		return nil, err
	}
	src := printBuf1.Bytes()

	if !lpkg.toObfuscate() {
		// We lightly transform packages which shouldn't be obfuscated,
		// such as when rewriting go:linkname directives to obfuscated packages.
		// We still need to print the files, but without obfuscating positions.
		return src, nil
	}
	// Tiny mode strips source positions in the linker, so directives are unnecessary.
	// Don't obfuscate positions in runtime - the //line directives confuse
	// the compiler's linkname verification
	if flagTiny || lpkg.ImportPath == "runtime" {
		return src, nil
	}

	fsetFile := fset.File(file.Pos())
	filename := filepath.Base(fsetFile.Name())
	newPrefix := ""
	if strings.HasPrefix(filename, "_cgo_") {
		newPrefix = "_cgo_"
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
	// Positionless top-level functions are generated, not original source.
	// Their printed line numbers carry no reversible source information.
	generatedIdents := make(map[*ast.Ident]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Pos().IsValid() || fn.Name.Pos().IsValid() {
			continue
		}
		positionless := true
		for node := range ast.Preorder(fn) {
			if node.Pos().IsValid() {
				positionless = false
				break
			}
		}
		if !positionless {
			continue
		}
		for node := range ast.Preorder(fn) {
			if id, ok := node.(*ast.Ident); ok {
				generatedIdents[id] = true
			}
		}
	}
	var origCallOffsets []int
	var generatedIdent []bool
	nextOffset := -1
	for node := range ast.Preorder(file) {
		switch node := node.(type) {
		case *ast.CallExpr:
			nextOffset = fsetFile.Position(node.Pos()).Offset
		case *ast.Ident:
			origCallOffsets = append(origCallOffsets, nextOffset)
			generatedIdent = append(generatedIdent, generatedIdents[node])
			nextOffset = -1
		}
	}

	copied := 0
	printBuf2.Reset()

	// Make sure the entire file gets a zero filename by default,
	// in case we miss any positions below.
	// We use a //-style comment, because there might be build tags.
	fmt.Fprintf(&printBuf2, "//line %s:1\n", newPrefix)

	// We use an empty filename when tokenizing below.
	// We use a nil go/scanner.ErrorHandler because src comes from go/printer.
	// Syntax errors should be rare, and when they do happen,
	// we don't want to point to the original source file on disk.
	// That would be confusing, as we've changed the source in memory.
	var s scanner.Scanner
	fsetFile = fset.AddFile("", fset.Base(), len(src))
	s.Init(fsetFile, src, nil, scanner.ScanComments)

	identIndex := 0
	braceDepth, parenDepth := 0, 0
	generated := false
	generatedLine := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.FUNC && braceDepth == 0 && identIndex < len(generatedIdent) {
			generated = generatedIdent[identIndex]
			generatedLine = 0
		}
		line := fsetFile.Position(pos).Line
		if generated && line != generatedLine && tok != token.SEMICOLON && tok != token.COMMENT && tok != token.EOF {
			generatedLine = line
			offset := fsetFile.Position(pos).Offset
			printBuf2.Write(src[copied:offset])
			copied = offset
			fmt.Fprintf(&printBuf2, " /*line %s:1*/ ", newPrefix)
		}
		switch tok {
		case token.LPAREN:
			parenDepth++
		case token.RPAREN:
			parenDepth--
		case token.LBRACE:
			braceDepth++
		case token.RBRACE:
			braceDepth--
			if braceDepth == 0 && parenDepth == 0 {
				generated = false
			}
		}
		switch tok {
		case token.EOF:
			// Copy the rest and return.
			printBuf2.Write(src[copied:])
			return printBuf2.Bytes(), nil
		case token.COMMENT:
			// Omit comments from the final Go code, again.
			// Before we removed the comments from file.Comments,
			// but go/printer also grabs comments from some Doc ast.Node fields.
			// TODO: is there an easy way to filter all comments at once?
			if isGoDirective(lit) {
				continue // directives are kept
			}
			offset := fsetFile.Position(pos).Offset
			printBuf2.Write(src[copied:offset])
			copied = offset + len(lit)
		case token.IDENT:
			if identIndex >= len(origCallOffsets) {
				// A transform such as ctrlflow/ssa2ast may add identifiers
				// that have no corresponding entry in origCallOffsets, so the
				// printed source can contain more identifiers than were
				// collected from the AST. They carry no original position to
				// remap, so leave them untouched.
				continue
			}
			origOffset := origCallOffsets[identIndex]
			identIndex++
			if generatedIdent[identIndex-1] {
				continue
			}
			if origOffset == -1 {
				continue // identifiers which don't start func calls are left untouched
			}
			newName := ""
			if !flagTiny {
				origPos := fmt.Sprintf("%s:%d", filename, origOffset)
				newName = hashWithPackage(lpkg, origPos) + ".go"
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
			fmt.Fprintf(&printBuf2, " /*line %s%s:1*/ ", newPrefix, newName)
		}
	}
}

// isGoDirective reports whether a comment is a Go toolchain directive,
// such as "//go:linkname" or "//go:build".
func isGoDirective(comment string) bool {
	dir, ok := ast.ParseDirective(token.NoPos, comment)
	return ok && dir.Tool == "go"
}
