package ctrlflow

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/printer"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"os"
	"strconv"
	"testing"

	"github.com/go-quicktest/qt"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
	ah "mvdan.cc/garble/internal/asthelper"
)

// Test_generateTrashBlock tests correctness of generated trash code by generating and compiling a large number of statements
func Test_generateTrashBlock(t *testing.T) {
	const (
		seed      = 7777
		stmtCount = 1024
	)

	fset := token.NewFileSet()
	buildPkg := func(f *ast.File) *ssa.Package {
		ssaPkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset, types.NewPackage("test/main", ""), []*ast.File{f}, 0)
		qt.Assert(t, qt.IsNil(err))
		return ssaPkg
	}

	body := &ast.BlockStmt{}
	file := &ast.File{
		Name: ast.NewIdent("main"),
		Decls: []ast.Decl{
			&ast.GenDecl{
				Tok: token.IMPORT,
				Specs: []ast.Spec{
					&ast.ImportSpec{
						Name: ast.NewIdent("_"),
						Path: ah.StringLit("os"),
					},
					&ast.ImportSpec{
						Name: ast.NewIdent("_"),
						Path: ah.StringLit("math"),
					},
					&ast.ImportSpec{
						Name: ast.NewIdent("_"),
						Path: ah.StringLit("fmt"),
					},
				},
			},
			&ast.FuncDecl{
				Name: ast.NewIdent("main"),
				Type: &ast.FuncType{Params: &ast.FieldList{}},
				Body: body,
			},
		},
	}
	beforeSsaPkg := buildPkg(file)

	imports := make(map[string]string)
	gen := newTrashGenerator(beforeSsaPkg.Prog, func(pkg *types.Package) *ast.Ident {
		if pkg == nil || pkg.Path() == beforeSsaPkg.Pkg.Path() {
			return nil
		}

		name, ok := imports[pkg.Path()]
		if !ok {
			name = importPrefix + strconv.Itoa(len(imports))
			imports[pkg.Path()] = name
			astutil.AddNamedImport(fset, file, name, pkg.Path())
		}
		return ast.NewIdent(name)
	}, mathrand.New(mathrand.NewSource(seed)))

	predefinedArgs := make(map[string]types.Type)
	for i := types.Bool; i < types.UnsafePointer; i++ {
		name, typ := fmt.Sprintf("v%d", i), types.Typ[i]
		predefinedArgs[name] = typ
		body.List = append(body.List,
			&ast.DeclStmt{Decl: &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent(name)},
					Type:  ast.NewIdent(typ.Name()),
				}},
			}},
			ah.AssignStmt(ast.NewIdent("_"), ast.NewIdent(name)),
		)
	}

	body.List = append(body.List, gen.Generate(stmtCount, predefinedArgs)...)
	printer.Fprint(os.Stdout, fset, file)
	buildPkg(file)
}
