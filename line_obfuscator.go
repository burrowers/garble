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
		if strings.HasPrefix(comment.Text, "//go:") {
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
	case *ast.ImportSpec:
		n.Doc = clearCommentGroup(n.Doc)
	case *ast.ValueSpec:
		n.Doc = clearCommentGroup(n.Doc)
	case *ast.TypeSpec:
		n.Doc = clearCommentGroup(n.Doc)
	case *ast.GenDecl:
		n.Doc = clearCommentGroup(n.Doc)
	case *ast.FuncDecl:
		n.Doc = clearCommentGroup(n.Doc)
	case *ast.File:
		n.Doc = clearCommentGroup(n.Doc)
	}
}

func findBuildTags(commentGroups []*ast.CommentGroup) (buildTags []string) {
	for _, group := range commentGroups {
		for _, comment := range group.List {
			if !strings.Contains(comment.Text, "+build") {
				continue
			}
			buildTags = append(buildTags, comment.Text)
		}
	}
	return buildTags
}

func transformLineInfo(file *ast.File) ([]string, *ast.File) {
	// Save build tags and add file name leak protection
	extraComments := append(findBuildTags(file.Comments), "", "//line :1")
	file.Comments = nil

	newLines := mathrand.Perm(len(file.Decls))

	funcCounter := 0
	pre := func(cursor *astutil.Cursor) bool {
		node := cursor.Node()
		clearNodeComments(node)

		funcDecl, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}

		comment := &ast.Comment{Text: fmt.Sprintf("//line %c.go:%d", nameCharset[mathrand.Intn(len(nameCharset))], PosMin+newLines[funcCounter])}
		funcDecl.Doc = prependComment(funcDecl.Doc, comment)
		funcCounter++
		return true
	}

	return extraComments, astutil.Apply(file, pre, nil).(*ast.File)
}
