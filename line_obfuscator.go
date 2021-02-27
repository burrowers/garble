// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/ast"
	mathrand "math/rand"
	"strings"
)

// PosMin is the smallest correct value for the line number.
// Source: https://go.googlesource.com/go/+/refs/heads/master/src/cmd/compile/internal/syntax/parser_test.go#229
const PosMin = 1

// detachedDirectives is a list of Go compiler directives which don't need to go
// right next to a Go declaration. Unlike all other detached comments, these
// need to be kept around as they alter compiler behavior.
var detachedDirectives = []string{
	"// +build",
	"//go:linkname",
	"//go:cgo_ldflag",
	"//go:cgo_dynamic_linker",
	"//go:cgo_export_static",
	"//go:cgo_export_dynamic",
	"//go:cgo_import_static",
	"//go:cgo_import_dynamic",
}

func isDirective(text string, directives []string) bool {
	for _, prefix := range directives {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func prependComment(group *ast.CommentGroup, comment *ast.Comment) *ast.CommentGroup {
	if group == nil {
		return &ast.CommentGroup{List: []*ast.Comment{comment}}
	}

	group.List = append([]*ast.Comment{comment}, group.List...)
	return group
}

// Remove all comments from CommentGroup except //go: directives.
// go:linkname directives are removed, since they're collected and rewritten
// separately.
func clearCommentGroup(group *ast.CommentGroup) *ast.CommentGroup {
	if group == nil {
		return nil
	}

	var comments []*ast.Comment
	for _, comment := range group.List {
		if strings.HasPrefix(comment.Text, "//go:") && !strings.HasPrefix(comment.Text, "//go:linkname") {
			comments = append(comments, &ast.Comment{Text: comment.Text})
		}
	}
	if len(comments) == 0 {
		return nil
	}
	return &ast.CommentGroup{List: comments}
}

// Remove all comments from Doc (if any) except //go: directives.
func clearNodeComments(node ast.Node) {
	switch n := node.(type) {
	case *ast.Field:
		n.Doc = clearCommentGroup(n.Doc)
		n.Comment = nil
	case *ast.ImportSpec:
		n.Doc = clearCommentGroup(n.Doc)
		n.Comment = nil
	case *ast.ValueSpec:
		n.Doc = clearCommentGroup(n.Doc)
		n.Comment = nil
	case *ast.TypeSpec:
		n.Doc = clearCommentGroup(n.Doc)
		n.Comment = nil
	case *ast.GenDecl:
		n.Doc = clearCommentGroup(n.Doc)
	case *ast.FuncDecl:
		n.Doc = clearCommentGroup(n.Doc)
	case *ast.File:
		n.Doc = clearCommentGroup(n.Doc)
	}
}

// transformLineInfo removes the comment except go directives and build tags. Converts comments to the node view.
// It returns comments not attached to declarations and names of declarations which cannot be renamed.
func (tf *transformer) transformLineInfo(file *ast.File, name string) (detachedComments []string, f *ast.File) {
	prefix := ""
	if strings.HasPrefix(name, "_cgo_") {
		prefix = "_cgo_"
	}

	// Save build tags and add file name leak protection
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if isDirective(comment.Text, detachedDirectives) {
				detachedComments = append(detachedComments, comment.Text)
			}
		}
	}
	detachedComments = append(detachedComments, "", "//line "+prefix+":1")
	file.Comments = nil

	ast.Inspect(file, func(node ast.Node) bool {
		clearNodeComments(node)
		return true
	})

	newLines := mathrand.Perm(len(file.Decls))

	for i, decl := range file.Decls {
		var doc **ast.CommentGroup
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			doc = &decl.Doc
		case *ast.GenDecl:
			doc = &decl.Doc
		}
		newPos := prefix + ":1"
		if !opts.Tiny {
			newPos = fmt.Sprintf("%s%c.go:%d",
				prefix,
				nameCharset[mathrand.Intn(len(nameCharset))],
				PosMin+newLines[i],
			)
		}

		comment := &ast.Comment{Text: "//line " + newPos}
		*doc = prependComment(*doc, comment)
	}

	return detachedComments, file
}
