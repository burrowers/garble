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

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ssa"
	ah "mvdan.cc/garble/internal/asthelper"
	"mvdan.cc/garble/internal/ssa2ast"
)

const (
	mergedFileName = "GARBLE_controlflow.go"
	directiveName  = "//garble:controlflow"
	importPrefix   = "___garble_import"

	defaultBlockSplits   = 0
	defaultJunkJumps     = 0
	defaultFlattenPasses = 1
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
	fieldsStr, ok := strings.CutPrefix(directive, directiveName)
	if !ok {
		return nil, false
	}

	fields := strings.Fields(fieldsStr)
	if len(fields) == 0 {
		return nil, true
	}
	m := make(map[string]string)
	for _, v := range fields {
		key, value, ok := strings.Cut(v, "=")
		if ok {
			m[key] = value
		} else {
			m[key] = ""
		}
	}
	return m, true
}

// Obfuscate obfuscates control flow of all functions with directive using control flattening.
// All obfuscated functions are removed from the original file and moved to the new one.
// Obfuscation can be customized by passing parameters from the directive, example:
//
// //garble:controlflow flatten_passes=1 junk_jumps=0 block_splits=0
// func someMethod() {}
//
// flatten_passes - controls number of passes of control flow flattening. Have exponential complexity and more than 3 passes are not recommended in most cases.
// junk_jumps - controls how many junk jumps are added. It does not affect final binary by itself, but together with flattening linearly increases complexity.
// block_splits - controls number of times largest block must be splitted. Together with flattening improves obfuscation of long blocks without branches.
func Obfuscate(fset *token.FileSet, ssaPkg *ssa.Package, files []*ast.File, obfRand *mathrand.Rand) (newFileName string, newFile *ast.File, affectedFiles []*ast.File, err error) {
	var ssaFuncs []*ssa.Function
	var ssaParams []directiveParamMap

	for _, file := range files {
		affected := false
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Doc == nil {
				continue
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
				// TODO: implement a complete function removal
				funcDecl.Name = ast.NewIdent("_")
				funcDecl.Body = ah.BlockStmt()
				funcDecl.Recv = nil
				funcDecl.Type = &ast.FuncType{Params: &ast.FieldList{}}
				affected = true

				break
			}
		}

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
	fset.AddFile(mergedFileName, int(newFile.Package), 1) // required for correct printer output

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

		split := params.GetInt("block_splits", defaultBlockSplits)
		junkCount := params.GetInt("junk_jumps", defaultJunkJumps)
		passes := params.GetInt("flatten_passes", defaultFlattenPasses)

		applyObfuscation := func(ssaFunc *ssa.Function) {
			for i := 0; i < split; i++ {
				if !applySplitting(ssaFunc, obfRand) {
					break // no more candidates for splitting
				}
			}
			if junkCount > 0 {
				addJunkBlocks(ssaFunc, junkCount, obfRand)
			}
			for i := 0; i < passes; i++ {
				applyFlattening(ssaFunc, obfRand)
			}
			fixBlockIndexes(ssaFunc)
		}

		applyObfuscation(ssaFunc)
		for _, anonFunc := range ssaFunc.AnonFuncs {
			applyObfuscation(anonFunc)
		}

		astFunc, err := ssa2ast.Convert(ssaFunc, funcConfig)
		if err != nil {
			return "", nil, nil, err
		}
		newFile.Decls = append(newFile.Decls, astFunc)
	}

	newFileName = mergedFileName
	return
}
