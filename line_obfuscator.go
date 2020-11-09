// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/ast"
	mathrand "math/rand"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// PosMin is the smallest correct value for the line number.
// Source: https://go.googlesource.com/go/+/refs/heads/master/src/cmd/compile/internal/syntax/parser_test.go#229
const PosMin = 1

const buildTagPrefix = "// +build"

// Source: https://go.googlesource.com/go/+/refs/heads/master/src/cmd/compile/internal/gc/noder.go#1493
var nameSpecialDirectives = []string{
	"//go:linkname",

	"//go:cgo_export_static",
	"//go:cgo_export_dynamic",
	"//go:cgo_import_static",
	"//go:cgo_import_dynamic",
}

var specialDirectives = append([]string{
	"//go:cgo_ldflag",
	"//go:cgo_dynamic_linker",
}, nameSpecialDirectives...)

func isDirective(text string, directives []string) bool {
	for _, prefix := range directives {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func getLocalName(text string) (string, bool) {
	if !isDirective(text, nameSpecialDirectives) {
		return "", false
	}
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return "", false
	}

	name := strings.TrimSpace(parts[1])
	if len(name) == 0 {
		return "", false
	}

	return name, true
}

func prependComment(group *ast.CommentGroup, comment *ast.Comment) *ast.CommentGroup {
	if group == nil {
		return &ast.CommentGroup{List: []*ast.Comment{comment}}
	}

	group.List = append([]*ast.Comment{comment}, group.List...)
	return group
}

// Remove all comments from CommentGroup except //go: directives.
func clearCommentGroup(group *ast.CommentGroup) *ast.CommentGroup {
	if group == nil {
		return nil
	}

	var comments []*ast.Comment
	for _, comment := range group.List {
		if strings.HasPrefix(comment.Text, "//go:") && !isDirective(comment.Text, specialDirectives) {
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

// processDetachedDire collects all not attached to declarations comments and build tags
// It returns detached comments and local name blacklist
func processDetachedDirectives(commentGroups []*ast.CommentGroup) (detachedComments, localNameBlacklist []string) {
	var buildTags []string
	var specialComments []string
	for _, commentGroup := range commentGroups {
		for _, comment := range commentGroup.List {
			if strings.HasPrefix(comment.Text, buildTagPrefix) {
				buildTags = append(buildTags, comment.Text)
				continue
			}

			if !isDirective(comment.Text, specialDirectives) {
				continue
			}

			specialComments = append(specialComments, comment.Text)
			if localName, ok := getLocalName(comment.Text); ok {
				localNameBlacklist = append(localNameBlacklist, localName)
			}
		}
	}

	detachedComments = append(detachedComments, buildTags...)
	detachedComments = append(detachedComments, specialComments...)
	detachedComments = append(detachedComments, "")
	return detachedComments, localNameBlacklist
}

// transformLineInfo removes the comment except go directives and build tags. Converts comments to the node view.
// It returns comments not attached to declarations and names of declarations which cannot be renamed.
func transformLineInfo(file *ast.File, cgoFile bool) (detachedComments, localNameBlacklist []string, f *ast.File) {
	prefix := ""
	if cgoFile {
		prefix = "_cgo_"
	}

	// Save build tags and add file name leak protection
	detachedComments, localNameBlacklist = processDetachedDirectives(file.Comments)
	detachedComments = append(detachedComments, "", "//line "+prefix+":1")
	file.Comments = nil

	newLines := mathrand.Perm(len(file.Decls))

	funcCounter := 0
	pre := func(cursor *astutil.Cursor) bool {
		node := cursor.Node()
		clearNodeComments(node)

		// If tiny mode is active information about line numbers is erased in object files
		if opts.Tiny {
			return true
		}
		funcDecl, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}

		comment := &ast.Comment{Text: fmt.Sprintf("//line %s%c.go:%d", prefix, nameCharset[mathrand.Intn(len(nameCharset))], PosMin+newLines[funcCounter])}
		funcDecl.Doc = prependComment(funcDecl.Doc, comment)
		funcCounter++
		return true
	}

	return detachedComments, localNameBlacklist, astutil.Apply(file, pre, nil).(*ast.File)
}
