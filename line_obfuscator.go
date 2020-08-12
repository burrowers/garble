package main

import (
	"fmt"
	"go/ast"
	"golang.org/x/tools/go/ast/astutil"
	mathrand "math/rand"
)

const (
	// PosMax is the largest line or column value that can be represented without loss.
	// Source: https://golang.org/src/cmd/compile/internal/syntax/pos.go
	PosMax = 1 << 30

	// PosMin is the smallest correct value for the line number.
	// Source: https://github.com/golang/go/blob/2001685ec01c240eda84762a3bc612ddd3ca93fe/src/cmd/compile/internal/syntax/parser_test.go#L229
	PosMin = 1
)

func transformLineInfo(fileIndex int, file *ast.File) *ast.File {
	pre := func(cursor *astutil.Cursor) bool {
		funcDecl, ok := cursor.Node().(*ast.FuncDecl)
		if !ok {
			return true
		}

		// Ignore functions with //go: directives
		if funcDecl.Doc != nil && len(funcDecl.Doc.List) != 0 {
			return true
		}

		if envGarbleTiny {
			funcDecl.Doc = &ast.CommentGroup{List: []*ast.Comment{{Text: "//line :1"}}}
			return true
		}

		linePos := hashWithAsUint64(buildInfo.buildID, fmt.Sprintf("%d:%s", fileIndex, funcDecl.Name), PosMin, PosMax)
		comment := &ast.Comment{Text: fmt.Sprintf("//line %c.go:%d", nameCharset[mathrand.Intn(len(nameCharset))], linePos)}
		funcDecl.Doc = &ast.CommentGroup{List: []*ast.Comment{comment}}
		return true
	}

	return astutil.Apply(file, pre, nil).(*ast.File)
}
