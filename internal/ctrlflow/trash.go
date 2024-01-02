package ctrlflow

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"math"
	mathrand "math/rand"
	"strconv"
	"strings"

	"golang.org/x/exp/maps"
	"golang.org/x/tools/go/ssa"
	ah "mvdan.cc/garble/internal/asthelper"
	"mvdan.cc/garble/internal/ssa2ast"
)

const (
	varProb        = 0.6
	globalProb     = 0.4
	assignVarProb  = 0.3
	methodCallProb = 0.5

	minMethodsForType = 2
	maxStringLen      = 32
	minVarsForAssign  = 2
	maxAssignVars     = 4
	maxVariadicParams = 5

	limitFunctionCount = 256
)

var stringEncoders = []func([]byte) string{
	hex.EncodeToString,
	base64.StdEncoding.EncodeToString,
	base64.URLEncoding.EncodeToString,
	base32.HexEncoding.EncodeToString,
	base32.StdEncoding.EncodeToString,
}

var valueGenerators = map[types.Type]func(rand *mathrand.Rand, targetType types.Type) ast.Expr{
	types.Typ[types.Bool]: func(rand *mathrand.Rand, _ types.Type) ast.Expr {
		var val string
		if rand.Float32() > 0.5 {
			val = "true"
		} else {
			val = "false"
		}
		return ast.NewIdent(val)
	},
	types.Typ[types.String]: func(rand *mathrand.Rand, _ types.Type) ast.Expr {
		buf := make([]byte, 1+rand.Intn(maxStringLen))
		rand.Read(buf)

		return ah.StringLit(stringEncoders[rand.Intn(len(stringEncoders))](buf))
	},
	types.Typ[types.UntypedNil]: func(rand *mathrand.Rand, _ types.Type) ast.Expr {
		return ast.NewIdent("nil")
	},
	types.Typ[types.Float32]: func(rand *mathrand.Rand, t types.Type) ast.Expr {
		var val float32
		if basic, ok := t.(*types.Basic); ok && (basic.Kind() != types.Float32 && basic.Kind() != types.Float64) {
			// If the target type is not float, generate float without fractional part for safe type conversion
			val = float32(rand.Intn(math.MaxInt8))
		} else {
			val = rand.Float32()
		}
		return &ast.BasicLit{
			Kind:  token.FLOAT,
			Value: strconv.FormatFloat(float64(val), 'f', -1, 32),
		}
	},
	types.Typ[types.Float64]: func(rand *mathrand.Rand, t types.Type) ast.Expr {
		var val float64
		if basic, ok := t.(*types.Basic); ok && basic.Kind() != types.Float64 {
			// If the target type is not float64, generate float without fractional part for safe type conversion
			val = float64(rand.Intn(math.MaxInt8))
		} else {
			val = rand.Float64()
		}
		return &ast.BasicLit{
			Kind:  token.FLOAT,
			Value: strconv.FormatFloat(val, 'f', -1, 64),
		}
	},
	types.Typ[types.Int]: func(rand *mathrand.Rand, t types.Type) ast.Expr {
		maxValue := math.MaxInt32
		if basic, ok := t.(*types.Basic); ok {
			// Int can be cast to any numeric type, but compiler checks for overflow when casting constants.
			// To prevent this, limiting the maximum value
			switch basic.Kind() {
			case types.Int8, types.Byte:
				maxValue = math.MaxInt8
			case types.Int16, types.Uint16:
				maxValue = math.MaxInt16
			}
		}
		return &ast.BasicLit{
			Kind:  token.INT,
			Value: strconv.FormatInt(int64(rand.Intn(maxValue)), 10),
		}
	},
}

func isInternal(path string) bool {
	return strings.HasSuffix(path, "/internal") || strings.HasPrefix(path, "internal/") || strings.Contains(path, "/internal/")
}

func under(t types.Type) types.Type {
	for {
		if t == t.Underlying() {
			return t
		}
		t = t.Underlying()
	}
}

func deref(typ types.Type) types.Type {
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	return typ
}

func canConvert(from, to types.Type) bool {
	i, isInterface := under(to).(*types.Interface)
	if isInterface {
		if ptr, ok := from.(*types.Pointer); ok {
			from = ptr.Elem()
		}
		return types.Implements(from, i)
	}
	return types.ConvertibleTo(from, to)
}

func isSupportedType(v types.Type) bool {
	for t := range valueGenerators {
		if canConvert(t, v) {
			return true
		}
	}
	return false
}

func isGenericType(p types.Type) bool {
	switch typ := p.(type) {
	case *types.Named:
		return typ.TypeParams() != nil
	case *types.Signature:
		return typ.TypeParams() != nil && typ.RecvTypeParams() == nil
	}
	return false
}

func isSupportedSig(m *types.Func) bool {
	sig := m.Type().(*types.Signature)
	if isGenericType(sig) {
		return false
	}
	for i := 0; i < sig.Params().Len(); i++ {
		if !isSupportedType(sig.Params().At(i).Type()) {
			return false
		}
	}
	return true
}

type trashGenerator struct {
	importNameResolver ssa2ast.ImportNameResolver
	rand               *mathrand.Rand
	typeConverter      *ssa2ast.TypeConverter
	globals            []*types.Var
	pkgFunctions       [][]*types.Func
	methodCache        map[types.Type][]*types.Func
}

func newTrashGenerator(ssaProg *ssa.Program, importNameResolver ssa2ast.ImportNameResolver, rand *mathrand.Rand) *trashGenerator {
	t := &trashGenerator{
		importNameResolver: importNameResolver,
		rand:               rand,
		typeConverter:      ssa2ast.NewTypeConverted(importNameResolver),
		methodCache:        make(map[types.Type][]*types.Func),
	}
	t.initialize(ssaProg)
	return t
}

type definedVar struct {
	Type     types.Type
	External bool

	Refs   int
	Ident  *ast.Ident
	Assign *ast.AssignStmt
}

func (d *definedVar) AddRef() {
	if !d.External {
		d.Refs++
	}
}

func (d *definedVar) HasRefs() bool {
	return d.External || d.Refs > 0
}

func (t *trashGenerator) initialize(ssaProg *ssa.Program) {
	for _, p := range ssaProg.AllPackages() {
		if isInternal(p.Pkg.Path()) || p.Pkg.Name() == "main" {
			continue
		}
		var pkgFuncs []*types.Func
		for _, member := range p.Members {
			if !token.IsExported(member.Name()) {
				continue
			}
			switch m := member.(type) {
			case *ssa.Global:
				if !isGenericType(m.Type()) && m.Object() != nil {
					t.globals = append(t.globals, m.Object().(*types.Var))
				}
			case *ssa.Function:
				if m.Signature.Recv() != nil || !isSupportedSig(m.Object().(*types.Func)) {
					continue
				}

				pkgFuncs = append(pkgFuncs, m.Object().(*types.Func))
				if len(pkgFuncs) > limitFunctionCount {
					break
				}
			}
		}

		if len(pkgFuncs) > 0 {
			t.pkgFunctions = append(t.pkgFunctions, pkgFuncs)
		}
	}
}

func (t *trashGenerator) convertExpr(from, to types.Type, expr ast.Expr) ast.Expr {
	if types.AssignableTo(from, to) {
		return expr
	}

	castExpr, err := t.typeConverter.Convert(to)
	if err != nil {
		panic(err)
	}
	return ah.CallExpr(&ast.ParenExpr{X: castExpr}, expr)
}

func (t *trashGenerator) chooseRandomVar(typ types.Type, vars map[string]*definedVar) ast.Expr {
	var candidates []string
	for name, d := range vars {
		if canConvert(d.Type, typ) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	targetVarName := candidates[t.rand.Intn(len(candidates))]
	targetVar := vars[targetVarName]
	targetVar.AddRef()

	return t.convertExpr(targetVar.Type, typ, ast.NewIdent(targetVarName))
}

func (t *trashGenerator) chooseRandomGlobal(typ types.Type) ast.Expr {
	var candidates []*types.Var
	for _, global := range t.globals {
		if canConvert(global.Type(), typ) {
			candidates = append(candidates, global)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	targetGlobal := candidates[t.rand.Intn(len(candidates))]

	var globalExpr ast.Expr
	if pkgIdent := t.importNameResolver(targetGlobal.Pkg()); pkgIdent != nil {
		globalExpr = ah.SelectExpr(pkgIdent, ast.NewIdent(targetGlobal.Name()))
	} else {
		globalExpr = ast.NewIdent(targetGlobal.Name())
	}
	return t.convertExpr(targetGlobal.Type(), typ, globalExpr)
}

func (t *trashGenerator) generateRandomConst(p types.Type, rand *mathrand.Rand) ast.Expr {
	var candidates []types.Type
	for typ := range valueGenerators {
		if canConvert(typ, p) {
			candidates = append(candidates, typ)
		}
	}

	if len(candidates) == 0 {
		panic(fmt.Errorf("unsupported type: %v", p))
	}

	generatorType := candidates[rand.Intn(len(candidates))]
	generator := valueGenerators[generatorType]
	return t.convertExpr(generatorType, p, generator(rand, under(p)))
}

func (t *trashGenerator) generateRandomValue(typ types.Type, vars map[string]*definedVar) ast.Expr {
	if t.rand.Float32() < varProb {
		if expr := t.chooseRandomVar(typ, vars); expr != nil {
			return expr
		}
	}
	if t.rand.Float32() < globalProb {
		if expr := t.chooseRandomGlobal(typ); expr != nil {
			return expr
		}
	}
	return t.generateRandomConst(typ, t.rand)
}

func (t *trashGenerator) cacheMethods(vars map[string]*definedVar) {
	for _, d := range vars {
		typ := deref(d.Type)
		if _, ok := t.methodCache[typ]; ok {
			continue
		}

		var methods []*types.Func
		switch typ := typ.(type) {
		case *types.Named:
			for i := 0; i < typ.NumMethods(); i++ {
				if m := typ.Method(i); token.IsExported(m.Name()) && isSupportedSig(m) {
					methods = append(methods, m)
					if len(methods) > limitFunctionCount {
						break
					}
				}
			}
		case *types.Interface:
			for i := 0; i < typ.NumMethods(); i++ {
				if m := typ.Method(i); token.IsExported(m.Name()) && isSupportedSig(m) {
					methods = append(methods, m)
					if len(methods) > limitFunctionCount {
						break
					}
				}
			}
		}
		if len(methods) < minMethodsForType {
			methods = nil
		}
		t.methodCache[typ] = methods
	}
}

func (t *trashGenerator) chooseRandomMethod(vars map[string]*definedVar) (*types.Func, string) {
	t.cacheMethods(vars)

	groupedCandidates := make(map[types.Type][]string)
	for name, v := range vars {
		typ := deref(v.Type)
		if len(t.methodCache[typ]) == 0 {
			continue
		}
		groupedCandidates[typ] = append(groupedCandidates[typ], name)
	}

	if len(groupedCandidates) == 0 {
		return nil, ""
	}

	candidateTypes := maps.Keys(groupedCandidates)
	candidateType := candidateTypes[t.rand.Intn(len(candidateTypes))]
	candidates := groupedCandidates[candidateType]

	name := candidates[t.rand.Intn(len(candidates))]
	vars[name].AddRef()

	methods := t.methodCache[candidateType]
	return methods[t.rand.Intn(len(methods))], name
}

func (t *trashGenerator) generateCall(vars map[string]*definedVar) ast.Stmt {
	var (
		targetFunc     *types.Func
		targetRecvName string
	)
	if t.rand.Float32() < methodCallProb {
		targetFunc, targetRecvName = t.chooseRandomMethod(vars)
	}

	if targetFunc == nil {
		targetPkg := t.pkgFunctions[t.rand.Intn(len(t.pkgFunctions))]
		targetFunc = targetPkg[t.rand.Intn(len(targetPkg))]
	}

	var args []ast.Expr

	targetSig := targetFunc.Type().(*types.Signature)
	params := targetSig.Params()
	for i := 0; i < params.Len(); i++ {
		param := params.At(i)
		if !targetSig.Variadic() || i != params.Len()-1 {
			args = append(args, t.generateRandomValue(param.Type(), vars))
			continue
		}

		variadicCount := t.rand.Intn(maxVariadicParams)
		for i := 0; i < variadicCount; i++ {
			sliceTyp, ok := param.Type().(*types.Slice)
			if !ok {
				panic(fmt.Errorf("unsupported variadic type: %v", param.Type()))
			}
			args = append(args, t.generateRandomValue(sliceTyp.Elem(), vars))
		}
	}

	var fun ast.Expr
	if targetSig.Recv() != nil {
		if len(targetRecvName) == 0 {
			panic("recv var must be set")
		}
		fun = ah.SelectExpr(ast.NewIdent(targetRecvName), ast.NewIdent(targetFunc.Name()))
	} else if pkgIdent := t.importNameResolver(targetFunc.Pkg()); pkgIdent != nil {
		fun = ah.SelectExpr(pkgIdent, ast.NewIdent(targetFunc.Name()))
	} else {
		fun = ast.NewIdent(targetFunc.Name())
	}

	callExpr := ah.CallExpr(fun, args...)
	results := targetSig.Results()
	if results == nil {
		return ah.ExprStmt(callExpr)
	}

	assignStmt := &ast.AssignStmt{
		Tok: token.ASSIGN,
		Rhs: []ast.Expr{callExpr},
	}

	for i := 0; i < results.Len(); i++ {
		ident := ast.NewIdent(getRandomName(t.rand))
		vars[ident.Name] = &definedVar{
			Type:   results.At(i).Type(),
			Ident:  ident,
			Assign: assignStmt,
		}
		assignStmt.Lhs = append(assignStmt.Lhs, ident)
	}
	return assignStmt
}

func (t *trashGenerator) generateAssign(vars map[string]*definedVar) ast.Stmt {
	var varNames []string
	for name, d := range vars {
		if d.HasRefs() && isSupportedType(d.Type) {
			varNames = append(varNames, name)
		}
	}
	t.rand.Shuffle(len(varNames), func(i, j int) {
		varNames[i], varNames[j] = varNames[j], varNames[i]
	})

	varCount := 1 + t.rand.Intn(maxAssignVars)
	if varCount > len(varNames) {
		varCount = len(varNames)
	}

	assignStmt := &ast.AssignStmt{
		Tok: token.ASSIGN,
	}
	for _, name := range varNames[:varCount] {
		d := vars[name]
		d.AddRef()

		assignStmt.Lhs = append(assignStmt.Lhs, ast.NewIdent(name))
		assignStmt.Rhs = append(assignStmt.Rhs, t.generateRandomValue(d.Type, vars))
	}
	return assignStmt
}

func (t *trashGenerator) Generate(statementCount int, externalVars map[string]types.Type) []ast.Stmt {
	vars := make(map[string]*definedVar)
	for name, typ := range externalVars {
		vars[name] = &definedVar{Type: typ, External: true}
	}

	var stmts []ast.Stmt
	for i := 0; i < statementCount; i++ {
		var stmt ast.Stmt
		if len(vars) >= minVarsForAssign && t.rand.Float32() < assignVarProb {
			stmt = t.generateAssign(vars)
		} else {
			stmt = t.generateCall(vars)
		}
		stmts = append(stmts, stmt)
	}

	for _, v := range vars {
		if v.Ident != nil && !v.HasRefs() {
			v.Ident.Name = "_"
		} else if v.Assign != nil {
			v.Assign.Tok = token.DEFINE
		}
	}
	return stmts
}
