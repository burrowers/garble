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

	defaultSplit  = 0
	defaultJunk   = 0
	defaultPasses = 1
)

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

// parseDirective parses a directive string and returns a map of directive parameters.
// Each parameter should be in the form "key=value" or "key"
func parseDirective(directive string) (directiveParamMap, bool) {
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

// Obfuscate obfuscates control flow of all methods with directive using control flattening.
// All obfuscated methods are removed from the original file and moved to the new one.
// Obfuscation can be customized by passing parameters from the directive, example:
//
// //garble:controlflow passes=1 junk=0 split=0
// func someMethod() {}
//
// passes - controls number of passes of control flow flattening. Have exponential complexity and more than 3 passes are not recommended in most cases.
// junk - controls how many junk jumps are added. It does not affect final binary by itself, but together with flattening linearly increases complexity.
// split - controls number of times largest block must be splitted. Together with flattening improves obfuscation of long blocks without branches.
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
	fset.AddFile(FileName, int(newFile.Package), 1) // required for correct printer output

	funcConfig := ssa2ast.DefaultConfig()
	imports := make(map[string]string) // TODO: indirect imports turned into direct currently brake build process
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
		for i := 0; i < params.GetInt("split", defaultSplit); i++ {
			if !applySplitting(ssaFunc, obfRand) {
				break // no more candidates for splitting
			}
		}
		if junkCount := params.GetInt("junk", defaultJunk); junkCount > 0 {
			addJunkBlocks(ssaFunc, junkCount, obfRand)
		}
		for i := 0; i < params.GetInt("passes", defaultPasses); i++ {
			applyFlattening(ssaFunc, obfRand)
		}

		fixBlockIndexes(ssaFunc)
		astFunc, err := ssa2ast.Convert(ssaFunc, funcConfig)
		if err != nil {
			return nil, nil, err
		}
		newFile.Decls = append(newFile.Decls, astFunc)
	}

	return
}
