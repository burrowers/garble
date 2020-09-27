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
	// Not necessarily, but it is desirable to prevent unexpected consequences in cases where "//go:generate" is linked to "node.Doc"
	"//go:generate",
}, nameSpecialDirectives...)

func isOneOfDirective(text string, directives []string) bool {
	for _, prefix := range directives {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func getLocalName(text string) (string, bool) {
	if !isOneOfDirective(text, nameSpecialDirectives) {
		return "", false
	}
	parts := strings.SplitN(text, " ", 3)
	if len(parts) < 2 {
		return "", false
	}

	name := strings.TrimSpace(parts[1])
	if len(name) == 0 {
		return "", false
	}

	return name, false
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
		if strings.HasPrefix(comment.Text, "//go:") && !isOneOfDirective(comment.Text, specialDirectives) {
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

func processSpecialComments(commentGroups []*ast.CommentGroup) (extraComments, localnameBlacklist []string) {
	var buildTags []string
	var specialComments []string
	for _, commentGroup := range commentGroups {
		for _, comment := range commentGroup.List {
			if strings.HasPrefix(comment.Text, buildTagPrefix) {
				buildTags = append(buildTags, comment.Text)
				continue
			}

			if !isOneOfDirective(comment.Text, specialDirectives) {
				continue
			}

			specialComments = append(specialComments, comment.Text)
			localName, ok := getLocalName(comment.Text)
			if ok {
				localnameBlacklist = append(localnameBlacklist, localName)
			}
		}
	}

	extraComments = append(extraComments, buildTags...)
	extraComments = append(extraComments, "")
	extraComments = append(extraComments, specialComments...)
	extraComments = append(extraComments, "")
	return extraComments, localnameBlacklist
}

func transformLineInfo(file *ast.File, cgoFile bool) ([]string, []string, *ast.File) {
	prefix := ""
	if cgoFile {
		prefix = "_cgo_"
	}

	// Save build tags and add file name leak protection
	extraComments, localNameBlacklist := processSpecialComments(file.Comments)
	extraComments = append(extraComments, "", "//line "+prefix+":1")
	file.Comments = nil

	newLines := mathrand.Perm(len(file.Decls))

	funcCounter := 0
	pre := func(cursor *astutil.Cursor) bool {
		node := cursor.Node()
		clearNodeComments(node)

		if envGarbleTiny {
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

	return extraComments, localNameBlacklist, astutil.Apply(file, pre, nil).(*ast.File)
}
