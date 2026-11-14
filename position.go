// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"path/filepath"
	"strings"
)

var printBuf1, printBuf2 bytes.Buffer

// origPosition is an original source position and the file name its marker
// carries when the position belongs to a file other than the one being printed.
type origPosition struct {
	pos  token.Position
	file string
}

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
	// Parse the printed source to locate call parentheses, then tokenize it
	// below to remove comments and insert position directives.

	// We want to use the original positions for the hashed positions.
	// Since later we'll iterate on tokens rather than walking an AST,
	// we match call expressions between the original and printed ASTs.
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
	// Match calls and statements in AST order before and after printing.
	// Re-parsing identifies their actual parentheses even for nested or
	// generated callee expressions.
	var origCallPositions []token.Pos
	var generatedIdent []bool
	var origStmtPositions []token.Pos
	// A block's closing brace is its own position in the runtime: the
	// compiler places the return instruction and a deferred call's return
	// site there. It is not a statement, so it needs its own positions.
	var origBlockEnds []token.Pos
	for node := range ast.Preorder(file) {
		if block, ok := node.(*ast.BlockStmt); ok {
			origBlockEnds = append(origBlockEnds, block.Rbrace)
		}
		switch node := node.(type) {
		case *ast.CallExpr:
			// A converted function can carry positions from the file it was
			// moved out of, so resolve them through the whole file set rather
			// than through the one file being printed.
			origCallPositions = append(origCallPositions, node.Lparen)
		case ast.Stmt:
			// Statement positions are user visible: a panic that the runtime
			// raises at a statement without a call expression, and a
			// goroutine's creation site, are both reported at the statement
			// itself. Keep them so that below we can both correct line drift
			// and mark the first statement of a file.
			origStmtPositions = append(origStmtPositions, node.Pos())
		case *ast.Ident:
			generatedIdent = append(generatedIdent, generatedIdents[node])
		}
	}
	printed, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	printedFile := fset.File(printed.Pos())
	callPositions := make(map[int]origPosition, len(origCallPositions))
	stmtPositions := make(map[int]origPosition, len(origStmtPositions))
	// record maps the printed position of the node visited at index back to
	// its original position, when both are known.
	record := func(dst map[int]origPosition, orig []token.Pos, index int, printedPos token.Pos) {
		if index >= len(orig) || !printedPos.IsValid() {
			return
		}
		origPos := orig[index]
		if !origPos.IsValid() {
			return
		}
		pos := fset.Position(origPos)
		file := ""
		if physical := fset.PositionFor(origPos, false).Filename; filepath.Base(physical) != filename {
			// The control-flow output moves functions into a generated file,
			// so their statements keep positions of the file they came from.
			// Name their markers after that file to keep the positions of
			// different source files apart.
			file = filepath.Base(physical)
		}
		dst[printedFile.Offset(printedPos)] = origPosition{pos: pos, file: file}
	}
	callIndex, stmtIndex, blockIndex := 0, 0, 0
	for node := range ast.Preorder(printed) {
		switch node := node.(type) {
		case *ast.CallExpr:
			record(callPositions, origCallPositions, callIndex, node.Lparen)
			callIndex++
		case ast.Stmt:
			record(stmtPositions, origStmtPositions, stmtIndex, node.Pos())
			stmtIndex++
			// A block's closing brace belongs to the block's own statement
			// visit, so it keeps its own index alongside the statement one.
			if block, ok := node.(*ast.BlockStmt); ok {
				record(stmtPositions, origBlockEnds, blockIndex, block.Rbrace)
				blockIndex++
			}
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

	// Markers carry the original line rather than resetting to line 1, so that
	// positions after a marker keep a meaningful line. markerLine and
	// markerAtLine remember the last marker so that we can tell when the
	// printed lines have drifted from the original ones.
	markerLine, markerAtLine := 0, 0
	haveMarker := false

	// hashedMarkerName returns the filename used to obfuscate the source
	// position origPos, shared by the call and statement markers.
	hashedMarkerName := func(orig origPosition) string {
		if flagTiny {
			return ""
		}
		name := filename
		if orig.file != "" {
			name = orig.file
		}
		return hashWithPackage(lpkg, fmt.Sprintf("%s:%d", name, orig.pos.Offset)) + ".go"
	}
	// writeMarker inserts a //line directive for the original position origPos
	// before the token at offset, which was found at printed line printedLine.
	writeMarker := func(offset, printedLine int, orig origPosition) {
		printBuf2.Write(src[copied:offset])
		copied = offset

		// We use the "/*text*/" form, since we can use multiple of them
		// on a single line, and they don't require extra newlines.
		// Make sure there is whitespace at either side of a comment.
		// Otherwise, we could change the syntax of the program.
		// Inserting "/*text*/" in "a/b" // must be "a/ /*text*/ b",
		// as "a//*text*/b" is tokenized as a "//" comment.
		fmt.Fprintf(&printBuf2, " /*line %s%s:%d*/ ", newPrefix, hashedMarkerName(orig), orig.pos.Line)
		markerAtLine, markerLine = printedLine, orig.pos.Line
		haveMarker = true
	}
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
			markerAtLine, markerLine = line, 1
			haveMarker = true
		}
		if origPos, ok := stmtPositions[fsetFile.Offset(pos)]; ok && !generated {
			// The first statement of a file gets a marker so that positions
			// before any call are reversible at all. Later statements and
			// block ends only need one when go/printer changed the line
			// spacing or a transform moved code, as their line would
			// otherwise drift.
			if want := markerLine + (line - markerAtLine); !haveMarker || want != origPos.pos.Line {
				writeMarker(fsetFile.Position(pos).Offset, line, origPos)
			}
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
			identIndex++
		case token.LPAREN:
			origPos, ok := callPositions[fsetFile.Offset(pos)]
			if !ok || generated {
				continue
			}
			writeMarker(fsetFile.Position(pos).Offset, line, origPos)
		}
	}
}

// isGoDirective reports whether a comment is a Go toolchain directive,
// such as "//go:linkname" or "//go:build".
func isGoDirective(comment string) bool {
	dir, ok := ast.ParseDirective(token.NoPos, comment)
	return ok && dir.Tool == "go"
}
