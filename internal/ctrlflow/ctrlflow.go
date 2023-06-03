package ctrlflow

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	mathrand "math/rand"
	"strconv"
	"strings"

	"github.com/pagran/go-ssa2ast"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ssa"
	ah "mvdan.cc/garble/internal/asthelper"
)

const (
	FileName = "GARBLE_controlflow.go"

	directiveName = "//garble:controlflow"
	importPrefix  = "___garble_import"
)

func parseDirective(directive string) (map[string]string, bool) {
	if !strings.HasPrefix(directive, directiveName) {
		return nil, false
	}

	fields := strings.Fields(directive)
	if len(fields) <= 1 {
		return nil, true
	}
	m := make(map[string]string)
	for _, v := range fields[1:] {
		params := strings.SplitN(v, "=", 2)
		if len(params) == 2 {
			m[params[0]] = params[1]
		} else {
			m[params[0]] = ""
		}
	}
	return m, true
}

type directiveParamMap map[string]string

func (m directiveParamMap) GetInt(name string, def int) int {
	rawVal, ok := m[name]
	if !ok {
		return def
	}

	val, err := strconv.Atoi(rawVal)
	if err != nil {
		panic(fmt.Errorf("invalid flag %s format: %v", name, err))
	}
	return val
}

func Obfuscate(fset *token.FileSet, ssaPkg *ssa.Package, files []*ast.File, obfRand *mathrand.Rand) (newFile *ast.File, affectedFiles []*ast.File, err error) {
	var ssaFuncs []*ssa.Function
	var ssaParams []directiveParamMap

	for _, file := range files {
		affected := false
		ast.Inspect(file, func(node ast.Node) bool {
			if _, ok := node.(*ast.File); ok {
				return true
			}

			funcDecl, ok := node.(*ast.FuncDecl)
			if !ok || funcDecl.Doc == nil {
				return false
			}

			for _, comment := range funcDecl.Doc.List {
				params, hasDirective := parseDirective(comment.Text)
				if !hasDirective {
					continue
				}

				path, _ := astutil.PathEnclosingInterval(file, funcDecl.Pos(), funcDecl.Pos())
				ssaFunc := ssa.EnclosingFunction(ssaPkg, path)
				if ssaFunc == nil {
					panic("function exists in ast but not found in ssa")
				}

				ssaFuncs = append(ssaFuncs, ssaFunc)
				ssaParams = append(ssaParams, params)

				log.Printf("detected function for controlflow %s (params: %v)", funcDecl.Name.Name, params)

				// Remove inplace function from original file
				funcDecl.Name = ast.NewIdent("_")
				funcDecl.Body = ah.BlockStmt()
				funcDecl.Recv = nil
				funcDecl.Type = &ast.FuncType{Params: &ast.FieldList{}}
				affected = true

				break
			}
			return false
		})

		if affected {
			affectedFiles = append(affectedFiles, file)
		}
	}

	if len(ssaFuncs) == 0 {
		return
	}

	newFile = &ast.File{
		Package: token.Pos(fset.Base()),
		Name:    ast.NewIdent(files[0].Name.Name),
	}
	fset.AddFile(FileName, int(newFile.Package), 1)

	funcConfig := ssa2ast.DefaultConfig()
	imports := make(map[string]string)
	funcConfig.ImportNameResolver = func(pkg *types.Package) *ast.Ident {
		if pkg == nil || pkg.Path() == ssaPkg.Pkg.Path() {
			return nil
		}

		name, ok := imports[pkg.Path()]
		if !ok {
			name = importPrefix + strconv.Itoa(len(imports))
			imports[pkg.Path()] = name
			astutil.AddNamedImport(fset, newFile, name, pkg.Path())
		}
		return ast.NewIdent(name)
	}

	for idx, ssaFunc := range ssaFuncs {
		params := ssaParams[idx]
		addJunkBlocks(ssaFunc, params.GetInt("junk", 0), obfRand)
		for i := 0; i < params.GetInt("passes", 1); i++ {
			applyControlFlowFlattening(ssaFunc, obfRand)
		}
		astFunc, err := ssa2ast.Convert(ssaFunc, funcConfig)
		if err != nil {
			return nil, nil, err
		}
		newFile.Decls = append(newFile.Decls, astFunc)
	}

	return
}
