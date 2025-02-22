package ctrlflow

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"math"
	mathrand "math/rand"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
	ah "mvdan.cc/garble/internal/asthelper"
	"mvdan.cc/garble/internal/ssa2ast"
)

const (
	// varProb is a probability to use a local variable as a call parameter
	// or for assigning to another local variable
	varProb = 0.6
	// globalProb is a probability to use a global variable as a call parameter
	// or for assigning to local variable
	globalProb = 0.4
	// assignVarProb is a probability generation statement to assign random values to random variables
	// instead of generating a method/function call.
	assignVarProb = 0.3
	// methodCallProb is a probability of using a method instead of a function
	methodCallProb = 0.5

	// minMethodsForType minimum number of methods in the type to use when generating calls
	minMethodsForType = 2
	// maxStringLen maximum length of generated trash string
	maxStringLen = 32
	// minVarsForAssign minimum amount of local variables for random assignment
	minVarsForAssign = 2
	// maxAssignVars maximum amount of local variables for random assignment
	maxAssignVars = 4
	// maxVariadicParams maximum number of parameters passed to variadic method/function
	maxVariadicParams = 5

	// limitFunctionCount maximum number of functions in 1 package
	// that can be used to generate calls to functions
	limitFunctionCount = 256
)

// stringEncoders array of functions converting an array of bytes into a string
// used to generate more readable trash strings
var stringEncoders = []func([]byte) string{
	hex.EncodeToString,
	base64.StdEncoding.EncodeToString,
	base64.URLEncoding.EncodeToString,
	base32.HexEncoding.EncodeToString,
	base32.StdEncoding.EncodeToString,
}

// valueGenerators is a map containing trash value generators for basic types
var valueGenerators = map[types.Type]func(rand *mathrand.Rand, targetType types.Type) ast.Expr{
	types.Typ[types.Bool]: func(rand *mathrand.Rand, _ types.Type) ast.Expr {
		val := "false"
		if rand.Float32() > 0.5 {
			val = "true"
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
	if t == t.Underlying() {
		return t
	}
	return under(t.Underlying())
}

func deref(typ types.Type) types.Type {
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	return typ
}

// canConvert checks if one type can be converted to another type
func canConvert(from, to types.Type) bool {
	i, isInterface := under(to).(*types.Interface)
	if !isInterface {
		return types.ConvertibleTo(from, to)
	}
	if ptr, ok := from.(*types.Pointer); ok {
		from = ptr.Elem()
	}
	return types.Implements(from, i)
}

// isSupportedType checks that it is possible to generate a compatible value using valueGenerators
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

// isSupportedSig checks that the function is not generic and all parameters can be generated using valueGenerators
func isSupportedSig(m *types.Func) bool {
	sig := m.Signature()
	if isGenericType(sig) {
		return false
	}
	for i := range sig.Params().Len() {
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

// initialize scans and writes all supported functions in all non-internal packages used in the program
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

// convertExpr if it is not possible to directly assign one type to another, generates (<to>)(value) cast expression
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

// chooseRandomVar returns a random local variable compatible with the passed type
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

// chooseRandomGlobal returns a random global variable compatible with the passed type
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

// generateRandomConst generates a random constant compatible with the passed type
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

// generateRandomValue returns a random local or global variable or a constant value with regard to probabilities
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

// cacheMethods caches exported supported methods from passed local variables
func (t *trashGenerator) cacheMethods(vars map[string]*definedVar) {
	for _, d := range vars {
		typ := deref(d.Type)
		if _, ok := t.methodCache[typ]; ok {
			continue
		}

		type methodSet interface {
			NumMethods() int
			Method(i int) *types.Func
		}

		var methods []*types.Func
		switch typ := typ.(type) {
		case methodSet:
			for i := range typ.NumMethods() {
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

// chooseRandomMethod returns the name of a random variable and a random method
func (t *trashGenerator) chooseRandomMethod(vars map[string]*definedVar) (string, *types.Func) {
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
		return "", nil
	}

	candidateTypes := slices.Collect(maps.Keys(groupedCandidates))
	candidateType := candidateTypes[t.rand.Intn(len(candidateTypes))]
	candidates := groupedCandidates[candidateType]

	name := candidates[t.rand.Intn(len(candidates))]
	vars[name].AddRef()

	methods := t.methodCache[candidateType]
	return name, methods[t.rand.Intn(len(methods))]
}

// generateCall generates a random function or method call with random parameters and storing the call results in local variables
// Example:
//
// _garblebfqv3dv0i98n4 := syscall.Getppid()
// _garble5q5ot93l1arna, _garble7al9sqg518rmm := os.Create("I3BLXYDYB2TMSHB7F55K5IMHJBNAFOKKJRKZHRBR")
// _, _ = _garble5q5ot93l1arna.Readdir(_garblebfqv3dv0i98n4)
// _ = ___garble_import5.Appendf(([]byte)("y4FPLnz8"), _garble7al9sqg518rmm)
func (t *trashGenerator) generateCall(vars map[string]*definedVar) ast.Stmt {
	var (
		targetRecvName string
		targetFunc     *types.Func
	)
	if t.rand.Float32() < methodCallProb {
		targetRecvName, targetFunc = t.chooseRandomMethod(vars)
	}

	if targetFunc == nil {
		targetPkg := t.pkgFunctions[t.rand.Intn(len(t.pkgFunctions))]
		targetFunc = targetPkg[t.rand.Intn(len(targetPkg))]
	}

	var args []ast.Expr

	targetSig := targetFunc.Signature()
	params := targetSig.Params()
	for i := range params.Len() {
		param := params.At(i)
		if !targetSig.Variadic() || i != params.Len()-1 {
			args = append(args, t.generateRandomValue(param.Type(), vars))
			continue
		}

		variadicCount := t.rand.Intn(maxVariadicParams)
		for range variadicCount {
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

	for i := range results.Len() {
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

// generateAssign generates assignments to random variables with trash values or another variables
// Example:
//
// _garblekoc67okop1c1, _garble8qnl5l2r2qgf3, _garblebd5tafd3q10kg = (int)(_garble5l9i0jv62nmks), (int)(76), (int)(75)
// _garbleffa48bbrevdfd = os.Stdout
// _garblecneca0kqjdklo, _garble8n2j5a0p1ples = (int32)(44), (uint32)(33)
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

	varCount := min(1+t.rand.Intn(maxAssignVars), len(varNames))

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

// Generate generates complicated trash code containing calls to functions and methods and assignment of local variables
// Example:
//
// _garble5q5ot93l1arna, _garble7al9sqg518rmm := os.Create("I3BLXYDYB2TMSHB7F55K5IMHJBNAFOKKJRKZHRBR")
//
//	_ = _garble5q5ot93l1arna.Close()
//	v10, v9, v7 = (uint32)(v4), (uint16)(v5), (uint)(v3)
//	v1, v13, _garble5q5ot93l1arna, v14 = v1, (float32)(v8), nil, (float64)(562704055)
//	_ = os.Remove("QQEEH917VEIHK===")
//	_garblecoq8aub6r0q3r, _garble77tl4pskm8ep3 := _garble5q5ot93l1arna.ReadAt(([]byte)("0HHBJP9CFSRDH1HF"), (int64)(v2))
//	_garble66djp5lkdng61 := ___garble_import1.LoadUint32(nil)
func (t *trashGenerator) Generate(statementCount int, externalVars map[string]types.Type) []ast.Stmt {
	vars := make(map[string]*definedVar)
	for name, typ := range externalVars {
		vars[name] = &definedVar{Type: typ, External: true}
	}

	var stmts []ast.Stmt
	for range statementCount {
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
