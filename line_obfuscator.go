package main

import (
	"fmt"
	"go/ast"
	"golang.org/x/tools/go/ast/astutil"
	mathrand "math/rand"
)

// PosMax is the largest line or column value that can be represented without loss.
// Incoming values (arguments) larger than PosMax will be set to PosMax.
// Source: https://golang.org/src/cmd/compile/internal/syntax/pos.go
const PosMax = 1 << 30
const PosMin = 1

var emptyLine = &ast.CommentGroup{List: []*ast.Comment{{Text: "//line :1"}}}

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
			funcDecl.Doc = emptyLine
			return true
		}

		line := hashWithAsUint64(buildInfo.buildID, fmt.Sprintf("%d:%s", fileIndex, funcDecl.Name), PosMin, PosMax)
		comment := &ast.Comment{Text: fmt.Sprintf("//line %c.go:%d", nameCharset[mathrand.Intn(len(nameCharset))], line)}
		funcDecl.Doc = &ast.CommentGroup{List: []*ast.Comment{comment}}
		return true
	}

	return astutil.Apply(file, pre, nil).(*ast.File)
}
