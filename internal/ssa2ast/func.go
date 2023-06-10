package ssa2ast

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/exp/slices"
	"golang.org/x/tools/go/ssa"
	ah "mvdan.cc/garble/internal/asthelper"
)

var UnsupportedErr = errors.New("unsupported")

type NameType int

type ImportNameResolver func(pkg *types.Package) *ast.Ident

type ConverterConfig struct {
	ImportNameResolver ImportNameResolver
	NamePrefix         string
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
	importNameResolver ImportNameResolver
	tc                 *typeConverter
	namePrefix         string
	valueNameMap       map[ssa.Value]string
}

func Convert(ssaFunc *ssa.Function, cfg *ConverterConfig) (*ast.FuncDecl, error) {
	return newFuncConverter(cfg).convert(ssaFunc)
}

func newFuncConverter(cfg *ConverterConfig) *funcConverter {
	return &funcConverter{
		importNameResolver: cfg.ImportNameResolver,
		tc:                 &typeConverter{resolver: cfg.ImportNameResolver},
		namePrefix:         cfg.NamePrefix,
		valueNameMap:       make(map[ssa.Value]string),
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

func constToAst(val constant.Value) (ast.Expr, error) {
	switch val.Kind() {
	case constant.Bool:
		return ast.NewIdent(val.ExactString()), nil
	case constant.String:
		return &ast.BasicLit{Kind: token.STRING, Value: val.ExactString()}, nil
	case constant.Int:
		return &ast.BasicLit{Kind: token.INT, Value: val.ExactString()}, nil
	case constant.Float:
		return &ast.BasicLit{Kind: token.FLOAT, Value: val.String()}, nil
	default:
		return nil, fmt.Errorf("contant %v: %w", val, UnsupportedErr)
	}
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
		return nil, fmt.Errorf("anon func %s for call not found", val.Name())
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
			// TODO: review this
			if val.TypeParams().Len() != 0 {
				methodName.Name = methodName.Name[:strings.IndexRune(methodName.Name, '[')]
			}

			if !hasRecv {
				if val.Pkg != nil {
					if pkgIdent := fc.importNameResolver(val.Pkg.Pkg); pkgIdent != nil {
						callExpr.Fun = ah.SelectExpr(pkgIdent, methodName)
						break
					}
				}

				callExpr.Fun = methodName
			} else {
				argsOffset = 1
				recvExpr, err := fc.convertSsaValue(callCommon.Args[0])
				if err != nil {
					return nil, err
				}
				callExpr.Fun = ah.SelectExpr(recvExpr, methodName)
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
		return nil, fmt.Errorf("unsupported thunk type: %w", UnsupportedErr)
	}
	recvVar := thunkType.Recv()
	if recvVar == nil {
		return nil, fmt.Errorf("unsupported non method thunk: %w", UnsupportedErr)
	}

	thunkTypeAst, err := fc.tc.Convert(recvVar.Type())
	if err != nil {
		return nil, err
	}
	trimmedName := ast.NewIdent(strings.TrimSuffix(val.Name(), thunkPrefix))
	return ah.SelectExpr(&ast.ParenExpr{X: thunkTypeAst}, trimmedName), nil
}

func (fc *funcConverter) ssaValue(ssaValue ssa.Value, explicitNil bool) (ast.Expr, error) {
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
			tmpConstExpr, err := constToAst(val.Value)
			if err != nil {
				return nil, err
			}
			constExpr = tmpConstExpr
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
	if refs := reg.Referrers(); refs != nil {
		for _, instr := range *refs {
			extractInstr, ok := instr.(*ssa.Extract)
			if !ok {
				continue
			}
			if extractInstr.Index == idx {
				hasRefs = true
				break
			}
		}
	}

	if hasRefs {
		name = fc.tupleVarName(reg, idx)
	} else {
		name = "_"
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

			for i := 0; i < tuple.Len(); i++ {
				name, typ, hasRefs := fc.tupleVarNameAndType(r, i)
				tmpVars[name] = typ
				if hasRefs {
					localTuple = false
				}
				assignStmt.Lhs = append(assignStmt.Lhs, ast.NewIdent(name))
			}

			if !localTuple {
				for n, t := range tmpVars {
					astFunc.Vars[n] = t
				}
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
		var stmt ast.Stmt
		switch i := instr.(type) {
		case *ssa.Alloc:
			varType := i.Type().Underlying().(*types.Pointer).Elem()
			varExpr, err := fc.tc.Convert(varType)
			if err != nil {
				return err
			}
			stmt = defineVar(i, ah.CallExprByName("new", varExpr))
		case *ssa.BinOp:
			xExpr, err := fc.convertSsaValueNonExplicitNil(i.X)
			if err != nil {
				return err
			}

			var yExpr ast.Expr
			// Handle special case: if nil == nil
			if isNilValue(i.X) && isNilValue(i.Y) {
				yExpr, err = fc.convertSsaValue(i.Y)
			} else {
				yExpr, err = fc.convertSsaValueNonExplicitNil(i.Y)
			}
			if err != nil {
				return err
			}

			stmt = defineVar(i, &ast.BinaryExpr{
				X:  xExpr,
				Op: i.Op,
				Y:  yExpr,
			})
		case *ssa.Call:
			callFunExpr, err := fc.convertCall(i.Call)
			if err != nil {
				return err
			}
			stmt = defineVar(i, callFunExpr)
		case *ssa.ChangeInterface:
			castExpr, err := fc.castCallExpr(i.Type(), i.X)
			if err != nil {
				return err
			}
			stmt = defineVar(i, castExpr)
		case *ssa.ChangeType:
			castExpr, err := fc.castCallExpr(i.Type(), i.X)
			if err != nil {
				return err
			}
			stmt = defineVar(i, castExpr)
		case *ssa.Convert:
			castExpr, err := fc.castCallExpr(i.Type(), i.X)
			if err != nil {
				return err
			}
			stmt = defineVar(i, castExpr)
		case *ssa.Defer:
			callExpr, err := fc.convertCall(i.Call)
			if err != nil {
				return err
			}
			stmt = &ast.DeferStmt{Call: callExpr}
		case *ssa.Extract:
			name := fc.tupleVarName(i.Tuple, i.Index)
			stmt = defineVar(i, ast.NewIdent(name))
		case *ssa.Field:
			xExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}

			fieldName, err := getFieldName(i.X.Type(), i.Field)
			if err != nil {
				return err
			}
			stmt = defineVar(i, ah.SelectExpr(xExpr, ast.NewIdent(fieldName)))
		case *ssa.FieldAddr:
			xExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}

			fieldName, err := getFieldName(i.X.Type(), i.Field)
			if err != nil {
				return err
			}
			stmt = defineVar(i, &ast.UnaryExpr{
				Op: token.AND,
				X:  ah.SelectExpr(xExpr, ast.NewIdent(fieldName)),
			})
		case *ssa.Go:
			callExpr, err := fc.convertCall(i.Call)
			if err != nil {
				return err
			}
			stmt = &ast.GoStmt{Call: callExpr}
		case *ssa.Index:
			xExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}
			indexExpr, err := fc.convertSsaValue(i.Index)
			if err != nil {
				return err
			}
			stmt = defineVar(i, ah.IndexExprByExpr(xExpr, indexExpr))
		case *ssa.IndexAddr:
			xExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}
			indexExpr, err := fc.convertSsaValue(i.Index)
			if err != nil {
				return err
			}
			stmt = defineVar(i, &ast.UnaryExpr{Op: token.AND, X: ah.IndexExprByExpr(xExpr, indexExpr)})
		case *ssa.Lookup:
			mapExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}

			indexExpr, err := fc.convertSsaValue(i.Index)
			if err != nil {
				return err
			}

			mapIndexExpr := ah.IndexExprByExpr(mapExpr, indexExpr)
			if i.CommaOk {
				valName, valType, valHasRefs := fc.tupleVarNameAndType(i, 0)
				okName, okType, okHasRefs := fc.tupleVarNameAndType(i, 1)

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
				stmt = defineVar(i, mapIndexExpr)
			}
		case *ssa.MakeChan:
			chanExpr, err := fc.tc.Convert(i.Type())
			if err != nil {
				return err
			}
			makeExpr := ah.CallExprByName("make", chanExpr)
			if i.Size != nil {
				reserveExpr, err := fc.convertSsaValue(i.Size)
				if err != nil {
					return err
				}
				makeExpr.Args = append(makeExpr.Args, reserveExpr)
			}
			stmt = defineVar(i, makeExpr)
		case *ssa.MakeInterface:
			castExpr, err := fc.castCallExpr(i.Type(), i.X)
			if err != nil {
				return err
			}
			stmt = defineVar(i, castExpr)
		case *ssa.MakeMap:
			mapExpr, err := fc.tc.Convert(i.Type())
			if err != nil {
				return err
			}
			makeExpr := ah.CallExprByName("make", mapExpr)
			if i.Reserve != nil {
				reserveExpr, err := fc.convertSsaValue(i.Reserve)
				if err != nil {
					return err
				}
				makeExpr.Args = append(makeExpr.Args, reserveExpr)
			}
			stmt = defineVar(i, makeExpr)
		case *ssa.MakeSlice:
			sliceExpr, err := fc.tc.Convert(i.Type())
			if err != nil {
				return err
			}
			lenExpr, err := fc.convertSsaValue(i.Len)
			if err != nil {
				return err
			}
			capExpr, err := fc.convertSsaValue(i.Cap)
			if err != nil {
				return err
			}
			stmt = defineVar(i, ah.CallExprByName("make", sliceExpr, lenExpr, capExpr))
		case *ssa.MapUpdate:
			mapExpr, err := fc.convertSsaValue(i.Map)
			if err != nil {
				return err
			}
			keyExpr, err := fc.convertSsaValue(i.Key)
			if err != nil {
				return err
			}
			valueExpr, err := fc.convertSsaValue(i.Value)
			if err != nil {
				return err
			}
			stmt = ah.AssignStmt(ah.IndexExprByExpr(mapExpr, keyExpr), valueExpr)
		case *ssa.Next:
			okName, okType, okHasRefs := fc.tupleVarNameAndType(i, 0)
			keyName, keyType, keyHasRefs := fc.tupleVarNameAndType(i, 1)
			valName, valType, valHasRefs := fc.tupleVarNameAndType(i, 2)
			if okHasRefs {
				astFunc.Vars[okName] = okType
			}
			if keyHasRefs {
				astFunc.Vars[keyName] = keyType
			}
			if valHasRefs {
				astFunc.Vars[valName] = valType
			}

			if i.IsString {
				idxName := fc.tupleVarName(i.Iter, 0)
				iterValName := fc.tupleVarName(i.Iter, 1)

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
					Rhs: []ast.Expr{ah.CallExprByName(fc.getVarName(i.Iter))},
				}
			}
		case *ssa.Phi:
			phiName := fc.getVarName(i)
			astFunc.Vars[phiName] = i.Type()

			for predIdx, edge := range i.Edges {
				edgeExpr, err := fc.convertSsaValue(edge)
				if err != nil {
					return err
				}

				blockIdx := ssaBlock.Preds[predIdx].Index
				astFunc.Blocks[blockIdx].Phi = append(astFunc.Blocks[blockIdx].Phi, ah.AssignStmt(ast.NewIdent(phiName), edgeExpr))
			}
		case *ssa.Range:
			xExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}
			if isStringType(i.X.Type()) {
				idxName := fc.tupleVarName(i, 0)
				valName := fc.tupleVarName(i, 1)

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
				makeIterExpr, nextType, err := makeMapIteratorPolyfill(fc.tc, i.X.Type().(*types.Map))
				if err != nil {
					return err
				}

				stmt = defineTypedVar(i, nextType, ah.CallExpr(makeIterExpr, xExpr))
			}
		case *ssa.Select:
			const reservedTupleIdx = 2

			indexName, indexType, indexHasRefs := fc.tupleVarNameAndType(i, 0)
			okName, okType, okHasRefs := fc.tupleVarNameAndType(i, 1)
			if indexHasRefs {
				astFunc.Vars[indexName] = indexType
			}
			if okHasRefs {
				astFunc.Vars[okName] = okType
			}

			var stmts []ast.Stmt

			recvIndex := 0
			for idx, state := range i.States {
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
					valName, valType, valHasRefs := fc.tupleVarNameAndType(i, reservedTupleIdx+recvIndex)
					if valHasRefs {
						astFunc.Vars[valName] = valType
					}
					commStmt = ah.AssignStmt(ast.NewIdent(valName), &ast.UnaryExpr{Op: token.ARROW, X: chanExpr})
					recvIndex++
				default:
					return fmt.Errorf("not suuported select chan dir %d: %w", state.Dir, UnsupportedErr)
				}

				stmts = append(stmts, &ast.CommClause{
					Comm: commStmt,
					Body: []ast.Stmt{
						ah.AssignStmt(ast.NewIdent(indexName), ah.IntLit(idx)),
					},
				})
			}

			if !i.Blocking {
				stmts = append(stmts, &ast.CommClause{Body: []ast.Stmt{ah.AssignStmt(ast.NewIdent(indexName), ah.IntLit(len(i.States)))}})
			}

			stmt = &ast.SelectStmt{Body: ah.BlockStmt(stmts...)}
		case *ssa.Send:
			chanExpr, err := fc.convertSsaValue(i.Chan)
			if err != nil {
				return err
			}
			valExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}
			stmt = &ast.SendStmt{
				Chan:  chanExpr,
				Value: valExpr,
			}
		case *ssa.Slice:
			valExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}
			sliceExpr := &ast.SliceExpr{X: valExpr}
			if i.Low != nil {
				sliceExpr.Low, err = fc.convertSsaValue(i.Low)
				if err != nil {
					return err
				}
			}
			if i.High != nil {
				sliceExpr.High, err = fc.convertSsaValue(i.High)
				if err != nil {
					return err
				}
			}
			if i.Max != nil {
				sliceExpr.Max, err = fc.convertSsaValue(i.Max)
				if err != nil {
					return err
				}
			}
			stmt = defineVar(i, sliceExpr)
		case *ssa.SliceToArrayPointer:
			castExpr, err := fc.tc.Convert(i.Type())
			if err != nil {
				return err
			}
			xExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}
			stmt = defineVar(i, ah.CallExpr(&ast.ParenExpr{X: castExpr}, xExpr))
		case *ssa.Store:
			addrExpr, err := fc.convertSsaValue(i.Addr)
			if err != nil {
				return err
			}
			valExpr, err := fc.convertSsaValue(i.Val)
			if err != nil {
				return err
			}
			stmt = ah.AssignStmt(&ast.StarExpr{X: addrExpr}, valExpr)
		case *ssa.TypeAssert:
			valExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}

			assertTypeExpr, err := fc.tc.Convert(i.AssertedType)
			if err != nil {
				return err
			}

			if i.CommaOk {
				valName, valType, valHasRefs := fc.tupleVarNameAndType(i, 0)
				okName, okType, okHasRefs := fc.tupleVarNameAndType(i, 1)
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
				stmt = defineVar(i, &ast.TypeAssertExpr{X: valExpr, Type: assertTypeExpr})
			}
		case *ssa.UnOp:
			valExpr, err := fc.convertSsaValue(i.X)
			if err != nil {
				return err
			}
			if i.CommaOk {
				if i.Op != token.ARROW {
					return fmt.Errorf("unary operator %s in %v: %w", i.Op, instr, UnsupportedErr)
				}

				valName, valType, valHasRefs := fc.tupleVarNameAndType(i, 0)
				okName, okType, okHasRefs := fc.tupleVarNameAndType(i, 1)
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
			} else if i.Op == token.MUL {
				stmt = defineVar(i, &ast.StarExpr{X: valExpr})
			} else {
				stmt = defineVar(i, &ast.UnaryExpr{Op: i.Op, X: valExpr})
			}
		case *ssa.MakeClosure:
			anonFunc := i.Fn.(*ssa.Function)
			anonFuncName, err := fc.getAnonFunctionName(anonFunc)
			if err != nil {
				return err
			}
			if anonFuncName == nil {
				return fmt.Errorf("make closure for non anon func %s: %w", anonFunc.Name(), UnsupportedErr)
			}

			callExpr := &ast.CallExpr{Fun: anonFuncName}
			for _, freeVar := range i.Bindings {
				varExr, err := fc.convertSsaValue(freeVar)
				if err != nil {
					return err
				}
				callExpr.Args = append(callExpr.Args, varExr)
			}

			stmt = defineVar(i, callExpr)
		case *ssa.RunDefers, *ssa.DebugRef:
			// ignored
			continue
		default:
			return fmt.Errorf("instruction %v: %w", instr, UnsupportedErr)
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
		return fmt.Errorf("exit instruction %v: %w", exit, UnsupportedErr)
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

	if len(f.Vars) > 0 {
		groupedVar := make(map[types.Type][]string)

		// TODO: rewrite algo
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
		stmts = append(stmts, &ast.DeclStmt{Decl: &ast.GenDecl{
			Tok:   token.VAR,
			Specs: specs,
		}})
	}

	for _, block := range f.Blocks {
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
	if ssaFunc.Signature.TypeParams() != nil || ssaFunc.Signature.RecvTypeParams() != nil {
		return nil, UnsupportedErr
	}

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
