package ctrlflow

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"

	"golang.org/x/tools/go/ssa"
)

func outlineEligible(fn *ssa.Function) bool {
	if len(fn.Blocks) != 1 || len(fn.AnonFuncs) != 0 || fn.Signature.Recv() != nil || fn.Signature.Results().Len() != 1 {
		return false
	}
	basic, ok := fn.Signature.Results().At(0).Type().(*types.Basic)
	if !ok || (basic.Kind() != types.Uint8 && basic.Kind() != types.Uint16 && basic.Kind() != types.Uint32 && basic.Kind() != types.Uint64) {
		return false
	}
	for _, p := range fn.Params {
		if !types.Identical(p.Type(), basic) {
			return false
		}
	}
	for _, i := range fn.Blocks[0].Instrs {
		switch i := i.(type) {
		case *ssa.BinOp:
			if !types.Identical(i.Type(), basic) {
				return false
			}
			switch i.Op {
			case token.ADD, token.SUB, token.MUL, token.XOR, token.AND, token.OR:
			default:
				return false
			}
		case *ssa.Return, *ssa.DebugRef:
		default:
			return false
		}
	}
	return true
}

func outlineOperation(fs *token.FileSet, fn *ast.FuncDecl, typ types.Type, name string, noinline bool, rnd *rand.Rand) (*ast.FuncDecl, error) {
	var choices []*ast.AssignStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok && len(a.Rhs) == 1 {
			if _, ok := a.Rhs[0].(*ast.BinaryExpr); ok {
				choices = append(choices, a)
			}
		}
		return true
	})
	if len(choices) == 0 {
		return nil, nil
	}
	a := choices[rnd.Intn(len(choices))]
	expr := a.Rhs[0].(*ast.BinaryExpr)
	annotation := ""
	if noinline {
		annotation = "//go:noinline\n"
	}
	source := fmt.Sprintf("package outline\n%sfunc %s(a,b %s) %s { return a %s b }", annotation, name, types.TypeString(typ, nil), types.TypeString(typ, nil), expr.Op)
	parsed, err := parser.ParseFile(fs, name+".go", source, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	helper := parsed.Decls[0].(*ast.FuncDecl)
	a.Rhs[0] = &ast.CallExpr{Fun: ast.NewIdent(name), Args: []ast.Expr{expr.X, expr.Y}}
	return helper, nil
}
