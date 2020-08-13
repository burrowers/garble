package main

import (
	"fmt"
	"go/ast"
	mathrand "math/rand"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

const (
	// PosMax is the largest line or column value that can be represented without loss.
	// Source: https://golang.org/src/cmd/compile/internal/syntax/pos.go
	PosMax = 1 << 30

	// PosMin is the smallest correct value for the line number.
	// Source: https://github.com/golang/go/blob/2001685ec01c240eda84762a3bc612ddd3ca93fe/src/cmd/compile/internal/syntax/parser_test.go#L229
	PosMin = 1
)

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

func transformLineInfo(fileIndex int, file *ast.File) *ast.File {
	file.Comments = nil
	pre := func(cursor *astutil.Cursor) bool {
		node := cursor.Node()
		clearNodeComments(node)

		funcDecl, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}

		if envGarbleTiny {
			funcDecl.Doc = prependComment(funcDecl.Doc, &ast.Comment{Text: "//line :1"})
			return true
		}

		linePos := hashWithAsUint64(buildInfo.buildID, fmt.Sprintf("%d:%s", fileIndex, funcDecl.Name), PosMin, PosMax)
		comment := &ast.Comment{Text: fmt.Sprintf("//line %c.go:%d", nameCharset[mathrand.Intn(len(nameCharset))], linePos)}
		funcDecl.Doc = prependComment(funcDecl.Doc, comment)
		return true
	}

	return astutil.Apply(file, pre, nil).(*ast.File)
}
