package ssa2ast

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
	ah "mvdan.cc/garble/internal/asthelper"
)

var (
	ErrUnsupported = errors.New("unsupported")

	MarkerInstr = &ssa.Panic{}
)

type NameType int

type ImportNameResolver func(pkg *types.Package) *ast.Ident

type ConverterConfig struct {
	// ImportNameResolver function to get the actual import name.
	// Because converting works at function level, only the caller knows actual name of the import.
	ImportNameResolver ImportNameResolver

	// NamePrefix prefix added to all new local variables. Must be reasonably unique
	NamePrefix string

	// SsaValueRemap is used to replace ssa.Value with the specified ssa.Expr.
	// Note: Replacing ssa.Expr does not guarantee the correctness of the generated code.
	// When using it, strictly adhere to the value types.
	SsaValueRemap map[ssa.Value]ast.Expr

	// MarkerInstrCallback is called every time a MarkerInstr instruction is encountered.
	// Callback result is inserted into ast as is
	MarkerInstrCallback func(vars map[string]types.Type) []ast.Stmt
}

func DefaultConfig() *ConverterConfig {
	return &ConverterConfig{
		ImportNameResolver: defaultImportNameResolver,
		NamePrefix:         "_s2a_",
	}
}

func defaultImportNameResolver(pkg *types.Package) *ast.Ident {
	if pkg == nil || pkg.Name() == "main" {
		return nil
	}
	return ast.NewIdent(pkg.Name())
}

type funcConverter struct {
	importNameResolver  ImportNameResolver
	tc                  *TypeConverter
	namePrefix          string
	valueNameMap        map[ssa.Value]string
	ssaValueRemap       map[ssa.Value]ast.Expr
	markerInstrCallback func(map[string]types.Type) []ast.Stmt
}

func Convert(ssaFunc *ssa.Function, cfg *ConverterConfig) (*ast.FuncDecl, error) {
	return newFuncConverter(cfg).convert(ssaFunc)
}

func newFuncConverter(cfg *ConverterConfig) *funcConverter {
	return &funcConverter{
		importNameResolver:  cfg.ImportNameResolver,
		tc:                  &TypeConverter{resolver: cfg.ImportNameResolver},
		namePrefix:          cfg.NamePrefix,
		valueNameMap:        make(map[ssa.Value]string),
		ssaValueRemap:       cfg.SsaValueRemap,
		markerInstrCallback: cfg.MarkerInstrCallback,
	}
}

func (fc *funcConverter) getVarName(val ssa.Value) string {
	if name, ok := fc.valueNameMap[val]; ok {
		return name
	}

	name := fc.namePrefix + strconv.Itoa(len(fc.valueNameMap))
	fc.valueNameMap[val] = name
	return name
}

func (fc *funcConverter) convertSignatureToFuncDecl(name string, signature *types.Signature) (*ast.FuncDecl, error) {
	funcTypeDecl, err := fc.tc.Convert(signature)
	if err != nil {
		return nil, err
	}
	funcDecl := &ast.FuncDecl{Name: ast.NewIdent(name), Type: funcTypeDecl.(*ast.FuncType)}
	if recv := signature.Recv(); recv != nil {
		recvTypeExpr, err := fc.tc.Convert(recv.Type())
		if err != nil {
			return nil, err
		}
		f := &ast.Field{Type: recvTypeExpr}
		if recvName := recv.Name(); recvName != "" {
			f.Names = []*ast.Ident{ast.NewIdent(recvName)}
		}
		funcDecl.Recv = &ast.FieldList{List: []*ast.Field{f}}
	}
	return funcDecl, nil
}

func (fc *funcConverter) convertSignatureToFuncLit(signature *types.Signature) (*ast.FuncLit, error) {
	funcTypeDecl, err := fc.tc.Convert(signature)
	if err != nil {
		return nil, err
	}
	return &ast.FuncLit{Type: funcTypeDecl.(*ast.FuncType)}, nil
}

type AstBlock struct {
	Index   int
	HasRefs bool
	Body    []ast.Stmt
	Phi     []ast.Stmt
	Exit    ast.Stmt
}

type AstFunc struct {
	Vars   map[string]types.Type
	Blocks []*AstBlock
}

func isVoidType(typ types.Type) bool {
	tuple, ok := typ.(*types.Tuple)
	return ok && tuple.Len() == 0
}

func isStringType(typ types.Type) bool {
	return types.Identical(typ, types.Typ[types.String]) || types.Identical(typ, types.Typ[types.UntypedString])
}

func getFieldName(tp types.Type, index int) (string, error) {
	if pt, ok := tp.(*types.Pointer); ok {
		tp = pt.Elem()
	}
	if named, ok := tp.(*types.Named); ok {
		tp = named.Underlying()
	}
	if stp, ok := tp.(*types.Struct); ok {
		return stp.Field(index).Name(), nil
	}
	return "", fmt.Errorf("field %d not found in %v", index, tp)
}

func (fc *funcConverter) castCallExpr(typ types.Type, x ssa.Value) (*ast.CallExpr, error) {
	castExpr, err := fc.tc.Convert(typ)
	if err != nil {
		return nil, err
	}
	valExpr, err := fc.convertSsaValue(x)
	if err != nil {
		return nil, err
	}
	return ah.CallExpr(&ast.ParenExpr{X: castExpr}, valExpr), nil
}

func (fc *funcConverter) getLabelName(blockIdx int) *ast.Ident {
	return ast.NewIdent(fmt.Sprintf("%sl%d", fc.namePrefix, blockIdx))
}

func (fc *funcConverter) gotoStmt(blockIdx int) *ast.BranchStmt {
	return &ast.BranchStmt{
		Tok:   token.GOTO,
		Label: fc.getLabelName(blockIdx),
	}
}

func (fc *funcConverter) getAnonFunctionName(val *ssa.Function) (*ast.Ident, error) {
	parent := val.Parent()
	if parent == nil {
		return nil, nil
	}
	anonFuncIdx := slices.Index(parent.AnonFuncs, val)
	if anonFuncIdx < 0 {
		return nil, fmt.Errorf("anon func %q for call not found", val.Name())
	}
	return ast.NewIdent(fc.getAnonFuncName(anonFuncIdx)), nil
}

func (fc *funcConverter) convertCall(callCommon ssa.CallCommon) (*ast.CallExpr, error) {
	callExpr := &ast.CallExpr{}
	argsOffset := 0

	if !callCommon.IsInvoke() {
		switch val := callCommon.Value.(type) {
		case *ssa.Function:
			anonFuncName, err := fc.getAnonFunctionName(val)
			if err != nil {
				return nil, err
			}
			if anonFuncName != nil {
				callExpr.Fun = anonFuncName
				break
			}

			thunkCall, err := fc.getThunkMethodCall(val)
			if err != nil {
				return nil, err
			}
			if thunkCall != nil {
				callExpr.Fun = thunkCall
				break
			}

			hasRecv := val.Signature.Recv() != nil
			methodName := ast.NewIdent(val.Name())
			if hasRecv {
				argsOffset = 1
				recvExpr, err := fc.convertSsaValue(callCommon.Args[0])
				if err != nil {
					return nil, err
				}
				callExpr.Fun = ah.SelectExpr(recvExpr, methodName)
			} else {
				if val.Pkg != nil {
					if pkgIdent := fc.importNameResolver(val.Pkg.Pkg); pkgIdent != nil {
						callExpr.Fun = ah.SelectExpr(pkgIdent, methodName)
						break
					}
				}
				callExpr.Fun = methodName
			}
			if typeArgs := val.TypeArgs(); len(typeArgs) > 0 {
				// Generic methods are called in a monomorphic view (e.g. "someMethod[int string]"),
				// so to get the original name, delete everything starting from "[" inclusive.
				methodName.Name, _, _ = strings.Cut(methodName.Name, "[")
				genericCallExpr := &ast.IndexListExpr{
					X: callExpr.Fun,
				}

				// For better readability of generated code and to avoid ambiguities,
				// we explicitly specify generic method types (e.g. "someMethod[int, string](0, "str")")
				for _, typArg := range typeArgs {
					typeExpr, err := fc.tc.Convert(typArg)
					if err != nil {
						return nil, err
					}
					genericCallExpr.Indices = append(genericCallExpr.Indices, typeExpr)
				}
				callExpr.Fun = genericCallExpr
			}
		case *ssa.Builtin:
			name := val.Name()
			if _, ok := types.Unsafe.Scope().Lookup(name).(*types.Builtin); ok {
				unsafePkgIdent := fc.importNameResolver(types.Unsafe)
				if unsafePkgIdent == nil {
					return nil, fmt.Errorf("cannot resolve unsafe package")
				}
				callExpr.Fun = &ast.SelectorExpr{X: unsafePkgIdent, Sel: ast.NewIdent(name)}
			} else {
				callExpr.Fun = ast.NewIdent(name)
			}
		default:
			callFunExpr, err := fc.convertSsaValue(val)
			if err != nil {
				return nil, err
			}
			callExpr.Fun = callFunExpr
		}
	} else {
		recvExpr, err := fc.convertSsaValue(callCommon.Value)
		if err != nil {
			return nil, err
		}
		callExpr.Fun = ah.SelectExpr(recvExpr, ast.NewIdent(callCommon.Method.Name()))
	}

	for _, arg := range callCommon.Args[argsOffset:] {
		argExpr, err := fc.convertSsaValue(arg)
		if err != nil {
			return nil, err
		}
		callExpr.Args = append(callExpr.Args, argExpr)
	}
	if callCommon.Signature().Variadic() {
		callExpr.Ellipsis = 1
	}
	return callExpr, nil
}

func (fc *funcConverter) convertSsaValueNonExplicitNil(ssaValue ssa.Value) (ast.Expr, error) {
	return fc.ssaValue(ssaValue, false)
}

func (fc *funcConverter) convertSsaValue(ssaValue ssa.Value) (ast.Expr, error) {
	return fc.ssaValue(ssaValue, true)
}

func (fc *funcConverter) getThunkMethodCall(val *ssa.Function) (ast.Expr, error) {
	const thunkPrefix = "$thunk"
	if !strings.HasSuffix(val.Name(), thunkPrefix) {
		return nil, nil
	}
	thunkType, ok := val.Object().Type().Underlying().(*types.Signature)
	if !ok {
		return nil, fmt.Errorf("unsupported thunk type: %w", ErrUnsupported)
	}
	recvVar := thunkType.Recv()
	if recvVar == nil {
		return nil, fmt.Errorf("unsupported non method thunk: %w", ErrUnsupported)
	}

	thunkTypeAst, err := fc.tc.Convert(recvVar.Type())
	if err != nil {
		return nil, err
	}
	trimmedName := ast.NewIdent(strings.TrimSuffix(val.Name(), thunkPrefix))
	return ah.SelectExpr(&ast.ParenExpr{X: thunkTypeAst}, trimmedName), nil
}

func (fc *funcConverter) ssaValue(ssaValue ssa.Value, explicitNil bool) (expr ast.Expr, err error) {
	defer func() {
		if err == nil && len(fc.ssaValueRemap) > 0 {
			if newExpr, ok := fc.ssaValueRemap[ssaValue]; ok {
				expr = newExpr
			}
		}
	}()

	switch val := ssaValue.(type) {
	case *ssa.Builtin:
		return ast.NewIdent(val.Name()), nil
	case *ssa.Global:
		globalExpr := &ast.UnaryExpr{Op: token.AND}
		newName := ast.NewIdent(val.Name())
		if pkgIdent := fc.importNameResolver(val.Pkg.Pkg); pkgIdent != nil {
			globalExpr.X = ah.SelectExpr(pkgIdent, newName)
		} else {
			globalExpr.X = newName
		}
		return globalExpr, nil
	case *ssa.Function:
		anonFuncName, err := fc.getAnonFunctionName(val)
		if err != nil {
			return nil, err
		}
		if anonFuncName != nil {
			return anonFuncName, nil
		}

		thunkCall, err := fc.getThunkMethodCall(val)
		if err != nil {
			return nil, err
		}
		if thunkCall != nil {
			return thunkCall, nil
		}

		name := ast.NewIdent(val.Name())
		if val.Signature.Recv() == nil && val.Pkg != nil {
			if pkgIdent := fc.importNameResolver(val.Pkg.Pkg); pkgIdent != nil {
				return ah.SelectExpr(pkgIdent, name), nil
			}
		}
		return name, nil
	case *ssa.Const:
		var constExpr ast.Expr
		if val.Value == nil {
			// handle nil constant for non-pointer structs
			typ := val.Type()
			if _, ok := typ.(*types.Named); ok {
				typ = typ.Underlying()
			}
			if _, ok := typ.(*types.Struct); ok {
				typExpr, err := fc.tc.Convert(val.Type())
				if err != nil {
					return nil, err
				}
				return &ast.CompositeLit{Type: typExpr}, nil
			}

			constExpr = ast.NewIdent("nil")
			if !explicitNil {
				return constExpr, nil
			}
		} else {
			constExpr = ah.ConstToAst(val.Value)
		}

		if basicType, ok := val.Type().(*types.Basic); ok {
			if basicType.Info()&(types.IsString|types.IsUntyped) != 0 {
				return constExpr, nil
			}
		}

		castExpr, err := fc.tc.Convert(val.Type())
		if err != nil {
			return nil, err
		}
		return ah.CallExpr(&ast.ParenExpr{X: castExpr}, constExpr), nil
	case *ssa.Parameter, *ssa.FreeVar:
		return ast.NewIdent(val.Name()), nil
	default:
		return ast.NewIdent(fc.getVarName(val)), nil
	}
}

type register interface {
	Name() string
	Referrers() *[]ssa.Instruction
	Type() types.Type

	String() string
	Parent() *ssa.Function
	Pos() token.Pos
}

func (fc *funcConverter) tupleVarName(val ssa.Value, idx int) string {
	return fmt.Sprintf("%s_%d", fc.getVarName(val), idx)
}

func (fc *funcConverter) tupleVarNameAndType(reg ssa.Value, idx int) (name string, typ types.Type, hasRefs bool) {
	tupleType := reg.Type().(*types.Tuple)
	typ = tupleType.At(idx).Type()
	name = "_"

	refs := reg.Referrers()
	if refs == nil {
		return
	}

	for _, instr := range *refs {
		extractInstr, ok := instr.(*ssa.Extract)
		if ok && extractInstr.Index == idx {
			hasRefs = true
			name = fc.tupleVarName(reg, idx)
			return
		}
	}
	return
}

func isNilValue(value ssa.Value) bool {
	constVal, ok := value.(*ssa.Const)
	return ok && constVal.Value == nil
}

func (fc *funcConverter) convertBlock(astFunc *AstFunc, ssaBlock *ssa.BasicBlock, astBlock *AstBlock) error {
	astBlock.HasRefs = len(ssaBlock.Preds) != 0

	defineTypedVar := func(r register, typ types.Type, expr ast.Expr) ast.Stmt {
		if isVoidType(typ) {
			return &ast.ExprStmt{X: expr}
		}
		if tuple, ok := typ.(*types.Tuple); ok {
			assignStmt := &ast.AssignStmt{Tok: token.ASSIGN, Rhs: []ast.Expr{expr}}
			localTuple := true
			tmpVars := make(map[string]types.Type)

			for i := range tuple.Len() {
				name, typ, hasRefs := fc.tupleVarNameAndType(r, i)
				tmpVars[name] = typ
				if hasRefs {
					localTuple = false
				}
				assignStmt.Lhs = append(assignStmt.Lhs, ast.NewIdent(name))
			}

			if !localTuple {
				maps.Copy(astFunc.Vars, tmpVars)
			}

			return assignStmt
		}

		refs := r.Referrers()
		if refs == nil || len(*refs) == 0 {
			return ah.AssignStmt(ast.NewIdent("_"), expr)
		}

		localVar := true
		for _, refInstr := range *refs {
			if _, ok := refInstr.(*ssa.Phi); ok || refInstr.Block() != ssaBlock {
				localVar = false
			}
		}

		newName := fc.getVarName(r)
		assignStmt := ah.AssignDefineStmt(ast.NewIdent(newName), expr)
		if !localVar {
			assignStmt.Tok = token.ASSIGN
			astFunc.Vars[newName] = typ
		}
		return assignStmt
	}
	defineVar := func(r register, expr ast.Expr) ast.Stmt {
		return defineTypedVar(r, r.Type(), expr)
	}

	for _, instr := range ssaBlock.Instrs[:len(ssaBlock.Instrs)-1] {
		if instr == MarkerInstr {
			if fc.markerInstrCallback == nil {
				panic("marker callback is nil")
			}
			astBlock.Body = append(astBlock.Body, nil)
			continue
		}

		var stmt ast.Stmt
		switch instr := instr.(type) {
		case *ssa.Alloc:
			varType := instr.Type().Underlying().(*types.Pointer).Elem()
			varExpr, err := fc.tc.Convert(varType)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, ah.CallExprByName("new", varExpr))
		case *ssa.BinOp:
			xExpr, err := fc.convertSsaValueNonExplicitNil(instr.X)
			if err != nil {
				return err
			}

			var yExpr ast.Expr
			// Handle special case: if nil == nil
			if isNilValue(instr.X) && isNilValue(instr.Y) {
				yExpr, err = fc.convertSsaValue(instr.Y)
			} else {
				yExpr, err = fc.convertSsaValueNonExplicitNil(instr.Y)
			}
			if err != nil {
				return err
			}

			stmt = defineVar(instr, &ast.BinaryExpr{
				X:  xExpr,
				Op: instr.Op,
				Y:  yExpr,
			})
		case *ssa.Call:
			callFunExpr, err := fc.convertCall(instr.Call)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, callFunExpr)
		case *ssa.ChangeInterface:
			castExpr, err := fc.castCallExpr(instr.Type(), instr.X)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, castExpr)
		case *ssa.ChangeType:
			castExpr, err := fc.castCallExpr(instr.Type(), instr.X)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, castExpr)
		case *ssa.Convert:
			castExpr, err := fc.castCallExpr(instr.Type(), instr.X)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, castExpr)
		case *ssa.Defer:
			callExpr, err := fc.convertCall(instr.Call)
			if err != nil {
				return err
			}
			stmt = &ast.DeferStmt{Call: callExpr}
		case *ssa.Extract:
			name := fc.tupleVarName(instr.Tuple, instr.Index)
			stmt = defineVar(instr, ast.NewIdent(name))
		case *ssa.Field:
			xExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}

			fieldName, err := getFieldName(instr.X.Type(), instr.Field)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, ah.SelectExpr(xExpr, ast.NewIdent(fieldName)))
		case *ssa.FieldAddr:
			xExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}

			fieldName, err := getFieldName(instr.X.Type(), instr.Field)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, &ast.UnaryExpr{
				Op: token.AND,
				X:  ah.SelectExpr(xExpr, ast.NewIdent(fieldName)),
			})
		case *ssa.Go:
			callExpr, err := fc.convertCall(instr.Call)
			if err != nil {
				return err
			}
			stmt = &ast.GoStmt{Call: callExpr}
		case *ssa.Index:
			xExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}
			indexExpr, err := fc.convertSsaValue(instr.Index)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, ah.IndexExprByExpr(xExpr, indexExpr))
		case *ssa.IndexAddr:
			xExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}
			indexExpr, err := fc.convertSsaValue(instr.Index)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, &ast.UnaryExpr{Op: token.AND, X: ah.IndexExprByExpr(xExpr, indexExpr)})
		case *ssa.Lookup:
			mapExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}

			indexExpr, err := fc.convertSsaValue(instr.Index)
			if err != nil {
				return err
			}

			mapIndexExpr := ah.IndexExprByExpr(mapExpr, indexExpr)
			if instr.CommaOk {
				valName, valType, valHasRefs := fc.tupleVarNameAndType(instr, 0)
				okName, okType, okHasRefs := fc.tupleVarNameAndType(instr, 1)

				if valHasRefs {
					astFunc.Vars[valName] = valType
				}
				if okHasRefs {
					astFunc.Vars[okName] = okType
				}

				stmt = &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent(valName), ast.NewIdent(okName)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{mapIndexExpr},
				}
			} else {
				stmt = defineVar(instr, mapIndexExpr)
			}
		case *ssa.MakeChan:
			chanExpr, err := fc.tc.Convert(instr.Type())
			if err != nil {
				return err
			}
			makeExpr := ah.CallExprByName("make", chanExpr)
			if instr.Size != nil {
				reserveExpr, err := fc.convertSsaValue(instr.Size)
				if err != nil {
					return err
				}
				makeExpr.Args = append(makeExpr.Args, reserveExpr)
			}
			stmt = defineVar(instr, makeExpr)
		case *ssa.MakeInterface:
			castExpr, err := fc.castCallExpr(instr.Type(), instr.X)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, castExpr)
		case *ssa.MakeMap:
			mapExpr, err := fc.tc.Convert(instr.Type())
			if err != nil {
				return err
			}
			makeExpr := ah.CallExprByName("make", mapExpr)
			if instr.Reserve != nil {
				reserveExpr, err := fc.convertSsaValue(instr.Reserve)
				if err != nil {
					return err
				}
				makeExpr.Args = append(makeExpr.Args, reserveExpr)
			}
			stmt = defineVar(instr, makeExpr)
		case *ssa.MakeSlice:
			sliceExpr, err := fc.tc.Convert(instr.Type())
			if err != nil {
				return err
			}
			lenExpr, err := fc.convertSsaValue(instr.Len)
			if err != nil {
				return err
			}
			capExpr, err := fc.convertSsaValue(instr.Cap)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, ah.CallExprByName("make", sliceExpr, lenExpr, capExpr))
		case *ssa.MapUpdate:
			mapExpr, err := fc.convertSsaValue(instr.Map)
			if err != nil {
				return err
			}
			keyExpr, err := fc.convertSsaValue(instr.Key)
			if err != nil {
				return err
			}
			valueExpr, err := fc.convertSsaValue(instr.Value)
			if err != nil {
				return err
			}
			stmt = ah.AssignStmt(ah.IndexExprByExpr(mapExpr, keyExpr), valueExpr)
		case *ssa.Next:
			okName, okType, okHasRefs := fc.tupleVarNameAndType(instr, 0)
			keyName, keyType, keyHasRefs := fc.tupleVarNameAndType(instr, 1)
			valName, valType, valHasRefs := fc.tupleVarNameAndType(instr, 2)
			if okHasRefs {
				astFunc.Vars[okName] = okType
			}
			if keyHasRefs {
				astFunc.Vars[keyName] = keyType
			}
			if valHasRefs {
				astFunc.Vars[valName] = valType
			}

			if instr.IsString {
				idxName := fc.tupleVarName(instr.Iter, 0)
				iterValName := fc.tupleVarName(instr.Iter, 1)

				stmt = ah.BlockStmt(
					ah.AssignStmt(ast.NewIdent(okName), &ast.BinaryExpr{
						X:  ast.NewIdent(idxName),
						Op: token.LSS,
						Y:  ah.CallExprByName("len", ast.NewIdent(iterValName)),
					}),
					&ast.IfStmt{
						Cond: ast.NewIdent(okName),
						Body: ah.BlockStmt(
							&ast.AssignStmt{
								Lhs: []ast.Expr{ast.NewIdent(keyName), ast.NewIdent(valName)},
								Tok: token.ASSIGN,
								Rhs: []ast.Expr{ast.NewIdent(idxName), ah.IndexExprByExpr(ast.NewIdent(iterValName), ast.NewIdent(idxName))},
							},
							&ast.IncDecStmt{X: ast.NewIdent(idxName), Tok: token.INC},
						),
					},
				)
			} else {
				stmt = &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent(okName), ast.NewIdent(keyName), ast.NewIdent(valName)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{ah.CallExprByName(fc.getVarName(instr.Iter))},
				}
			}
		case *ssa.Phi:
			phiName := fc.getVarName(instr)
			astFunc.Vars[phiName] = instr.Type()

			for predIdx, edge := range instr.Edges {
				edgeExpr, err := fc.convertSsaValue(edge)
				if err != nil {
					return err
				}

				blockIdx := ssaBlock.Preds[predIdx].Index
				astFunc.Blocks[blockIdx].Phi = append(astFunc.Blocks[blockIdx].Phi, ah.AssignStmt(ast.NewIdent(phiName), edgeExpr))
			}
		case *ssa.Range:
			xExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}
			if isStringType(instr.X.Type()) {
				idxName := fc.tupleVarName(instr, 0)
				valName := fc.tupleVarName(instr, 1)

				astFunc.Vars[idxName] = types.Typ[types.Int]
				astFunc.Vars[valName] = types.NewSlice(types.Typ[types.Rune])

				stmt = &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent(idxName), ast.NewIdent(valName)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{
						ah.IntLit(0),
						ah.CallExpr(&ast.ArrayType{Elt: ast.NewIdent("rune")}, xExpr),
					},
				}
			} else {
				makeIterExpr, nextType, err := makeMapIteratorPolyfill(fc.tc, instr.X.Type().(*types.Map))
				if err != nil {
					return err
				}

				stmt = defineTypedVar(instr, nextType, ah.CallExpr(makeIterExpr, xExpr))
			}
		case *ssa.Select:
			const reservedTupleIdx = 2

			indexName, indexType, indexHasRefs := fc.tupleVarNameAndType(instr, 0)
			okName, okType, okHasRefs := fc.tupleVarNameAndType(instr, 1)
			if indexHasRefs {
				astFunc.Vars[indexName] = indexType
			}
			if okHasRefs {
				astFunc.Vars[okName] = okType
			}

			var stmts []ast.Stmt

			recvIndex := 0
			for idx, state := range instr.States {
				chanExpr, err := fc.convertSsaValue(state.Chan)
				if err != nil {
					return err
				}

				var commStmt ast.Stmt
				switch state.Dir {
				case types.SendOnly:
					valueExpr, err := fc.convertSsaValue(state.Send)
					if err != nil {
						return err
					}
					commStmt = &ast.SendStmt{Chan: chanExpr, Value: valueExpr}
				case types.RecvOnly:
					valName, valType, valHasRefs := fc.tupleVarNameAndType(instr, reservedTupleIdx+recvIndex)
					if valHasRefs {
						astFunc.Vars[valName] = valType
					}
					commStmt = ah.AssignStmt(ast.NewIdent(valName), &ast.UnaryExpr{Op: token.ARROW, X: chanExpr})
					recvIndex++
				default:
					return fmt.Errorf("not supported select chan dir %d: %w", state.Dir, ErrUnsupported)
				}

				stmts = append(stmts, &ast.CommClause{
					Comm: commStmt,
					Body: []ast.Stmt{
						ah.AssignStmt(ast.NewIdent(indexName), ah.IntLit(idx)),
					},
				})
			}

			if !instr.Blocking {
				stmts = append(stmts, &ast.CommClause{Body: []ast.Stmt{ah.AssignStmt(ast.NewIdent(indexName), ah.IntLit(len(instr.States)))}})
			}

			stmt = &ast.SelectStmt{Body: ah.BlockStmt(stmts...)}
		case *ssa.Send:
			chanExpr, err := fc.convertSsaValue(instr.Chan)
			if err != nil {
				return err
			}
			valExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}
			stmt = &ast.SendStmt{
				Chan:  chanExpr,
				Value: valExpr,
			}
		case *ssa.Slice:
			valExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}
			sliceExpr := &ast.SliceExpr{X: valExpr}
			if instr.Low != nil {
				sliceExpr.Low, err = fc.convertSsaValue(instr.Low)
				if err != nil {
					return err
				}
			}
			if instr.High != nil {
				sliceExpr.High, err = fc.convertSsaValue(instr.High)
				if err != nil {
					return err
				}
			}
			if instr.Max != nil {
				sliceExpr.Max, err = fc.convertSsaValue(instr.Max)
				if err != nil {
					return err
				}
			}
			stmt = defineVar(instr, sliceExpr)
		case *ssa.SliceToArrayPointer:
			castExpr, err := fc.tc.Convert(instr.Type())
			if err != nil {
				return err
			}
			xExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}
			stmt = defineVar(instr, ah.CallExpr(&ast.ParenExpr{X: castExpr}, xExpr))
		case *ssa.Store:
			addrExpr, err := fc.convertSsaValue(instr.Addr)
			if err != nil {
				return err
			}
			valExpr, err := fc.convertSsaValue(instr.Val)
			if err != nil {
				return err
			}
			stmt = ah.AssignStmt(&ast.StarExpr{X: addrExpr}, valExpr)
		case *ssa.TypeAssert:
			valExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}

			assertTypeExpr, err := fc.tc.Convert(instr.AssertedType)
			if err != nil {
				return err
			}

			if instr.CommaOk {
				valName, valType, valHasRefs := fc.tupleVarNameAndType(instr, 0)
				okName, okType, okHasRefs := fc.tupleVarNameAndType(instr, 1)
				if valHasRefs {
					astFunc.Vars[valName] = valType
				}
				if okHasRefs {
					astFunc.Vars[okName] = okType
				}

				stmt = &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent(valName), ast.NewIdent(okName)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{&ast.TypeAssertExpr{X: valExpr, Type: assertTypeExpr}},
				}
			} else {
				stmt = defineVar(instr, &ast.TypeAssertExpr{X: valExpr, Type: assertTypeExpr})
			}
		case *ssa.UnOp:
			valExpr, err := fc.convertSsaValue(instr.X)
			if err != nil {
				return err
			}
			if instr.CommaOk {
				if instr.Op != token.ARROW {
					return fmt.Errorf("unary operator %q in %v: %w", instr.Op, instr, ErrUnsupported)
				}

				valName, valType, valHasRefs := fc.tupleVarNameAndType(instr, 0)
				okName, okType, okHasRefs := fc.tupleVarNameAndType(instr, 1)
				if valHasRefs {
					astFunc.Vars[valName] = valType
				}
				if okHasRefs {
					astFunc.Vars[okName] = okType
				}

				stmt = &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent(valName), ast.NewIdent(okName)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{&ast.UnaryExpr{
						Op: token.ARROW,
						X:  valExpr,
					}},
				}
			} else if instr.Op == token.MUL {
				stmt = defineVar(instr, &ast.StarExpr{X: valExpr})
			} else {
				stmt = defineVar(instr, &ast.UnaryExpr{Op: instr.Op, X: valExpr})
			}
		case *ssa.MakeClosure:
			anonFunc := instr.Fn.(*ssa.Function)
			anonFuncName, err := fc.getAnonFunctionName(anonFunc)
			if err != nil {
				return err
			}
			if anonFuncName == nil {
				return fmt.Errorf("make closure for non anon func %q: %w", anonFunc.Name(), ErrUnsupported)
			}

			callExpr := &ast.CallExpr{Fun: anonFuncName}
			for _, freeVar := range instr.Bindings {
				varExr, err := fc.convertSsaValue(freeVar)
				if err != nil {
					return err
				}
				callExpr.Args = append(callExpr.Args, varExr)
			}

			stmt = defineVar(instr, callExpr)
		case *ssa.RunDefers, *ssa.DebugRef:
			// ignored
			continue
		default:
			return fmt.Errorf("instruction %v: %w", instr, ErrUnsupported)
		}

		if stmt != nil {
			astBlock.Body = append(astBlock.Body, stmt)
		}
	}

	exitInstr := ssaBlock.Instrs[len(ssaBlock.Instrs)-1]
	switch exit := exitInstr.(type) {
	case *ssa.Jump:
		targetBlockIdx := ssaBlock.Succs[0].Index
		astBlock.Exit = fc.gotoStmt(targetBlockIdx)
	case *ssa.If:
		tblock := ssaBlock.Succs[0].Index
		fblock := ssaBlock.Succs[1].Index

		condExpr, err := fc.convertSsaValue(exit.Cond)
		if err != nil {
			return err
		}

		astBlock.Exit = &ast.IfStmt{
			Cond: condExpr,
			Body: ah.BlockStmt(fc.gotoStmt(tblock)),
			Else: ah.BlockStmt(fc.gotoStmt(fblock)),
		}
	case *ssa.Return:
		exitStmt := &ast.ReturnStmt{}
		for _, result := range exit.Results {
			resultExpr, err := fc.convertSsaValue(result)
			if err != nil {
				return err
			}
			exitStmt.Results = append(exitStmt.Results, resultExpr)
		}
		astBlock.Exit = exitStmt
	case *ssa.Panic:
		panicArgExpr, err := fc.convertSsaValue(exit.X)
		if err != nil {
			return err
		}
		astBlock.Exit = &ast.ExprStmt{X: ah.CallExprByName("panic", panicArgExpr)}
	default:
		return fmt.Errorf("exit instruction %v: %w", exit, ErrUnsupported)
	}

	return nil
}

func (fc *funcConverter) getAnonFuncName(idx int) string {
	return fmt.Sprintf(fc.namePrefix+"anonFunc%d", idx)
}

func (fc *funcConverter) convertAnonFuncs(anonFuncs []*ssa.Function) ([]ast.Stmt, error) {
	var stmts []ast.Stmt

	for i, anonFunc := range anonFuncs {
		anonLit, err := fc.convertSignatureToFuncLit(anonFunc.Signature)
		if err != nil {
			return nil, err
		}
		anonStmts, err := fc.convertToStmts(anonFunc)
		if err != nil {
			return nil, err
		}
		anonLit.Body = ah.BlockStmt(anonStmts...)

		if len(anonFunc.FreeVars) == 0 {
			stmts = append(stmts, &ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent(fc.getAnonFuncName(i))},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{anonLit},
			})
			continue
		}

		var closureVars []*types.Var
		for _, freeVar := range anonFunc.FreeVars {
			closureVars = append(closureVars, types.NewVar(token.NoPos, nil, freeVar.Name(), freeVar.Type()))
		}

		makeClosureType := types.NewSignatureType(nil, nil, nil, types.NewTuple(closureVars...), types.NewTuple(
			types.NewVar(token.NoPos, nil, "", anonFunc.Signature),
		), false)

		makeClosureLit, err := fc.convertSignatureToFuncLit(makeClosureType)
		if err != nil {
			return nil, err
		}
		makeClosureLit.Body = ah.BlockStmt(&ast.ReturnStmt{Results: []ast.Expr{anonLit}})

		stmts = append(stmts, &ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent(fc.getAnonFuncName(i))},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{makeClosureLit},
		})
	}
	return stmts, nil
}

func (fc *funcConverter) convertToStmts(ssaFunc *ssa.Function) ([]ast.Stmt, error) {
	stmts, err := fc.convertAnonFuncs(ssaFunc.AnonFuncs)
	if err != nil {
		return nil, err
	}

	f := &AstFunc{
		Vars:   make(map[string]types.Type),
		Blocks: make([]*AstBlock, len(ssaFunc.Blocks)),
	}
	for i := range f.Blocks {
		f.Blocks[i] = &AstBlock{Index: ssaFunc.Blocks[i].Index}
	}

	for i, ssaBlock := range ssaFunc.Blocks {
		if err := fc.convertBlock(f, ssaBlock, f.Blocks[i]); err != nil {
			return nil, err
		}
	}

	groupedVar := make(map[types.Type][]string)
	for varName, varType := range f.Vars {
		exists := false
		for groupedType, names := range groupedVar {
			if types.Identical(varType, groupedType) {
				groupedVar[groupedType] = append(names, varName)
				exists = true
				break
			}
		}
		if !exists {
			groupedVar[varType] = []string{varName}
		}
	}
	var specs []ast.Spec
	for varType, varNames := range groupedVar {
		typeExpr, err := fc.tc.Convert(varType)
		if err != nil {
			return nil, err
		}
		spec := &ast.ValueSpec{
			Type: typeExpr,
		}

		sort.Strings(varNames)
		for _, name := range varNames {
			spec.Names = append(spec.Names, ast.NewIdent(name))
		}
		specs = append(specs, spec)
	}
	if len(specs) > 0 {
		stmts = append(stmts, &ast.DeclStmt{Decl: &ast.GenDecl{
			Tok:   token.VAR,
			Specs: specs,
		}})
	}

	for _, block := range f.Blocks {
		if fc.markerInstrCallback != nil {
			var newBody []ast.Stmt
			for _, stmt := range block.Body {
				if stmt != nil {
					newBody = append(newBody, stmt)
				} else {
					newBody = append(newBody, fc.markerInstrCallback(f.Vars)...)
				}
			}
			block.Body = newBody
		}

		blockStmts := &ast.BlockStmt{List: append(block.Body, block.Phi...)}
		blockStmts.List = append(blockStmts.List, block.Exit)
		if block.HasRefs {
			stmts = append(stmts, &ast.LabeledStmt{Label: fc.getLabelName(block.Index), Stmt: blockStmts})
		} else {
			stmts = append(stmts, blockStmts)
		}
	}
	return stmts, nil
}

func (fc *funcConverter) convert(ssaFunc *ssa.Function) (*ast.FuncDecl, error) {
	funcDecl, err := fc.convertSignatureToFuncDecl(ssaFunc.Name(), ssaFunc.Signature)
	if err != nil {
		return nil, err
	}
	funcStmts, err := fc.convertToStmts(ssaFunc)
	if err != nil {
		return nil, err
	}
	funcDecl.Body = ah.BlockStmt(funcStmts...)
	return funcDecl, err
}
