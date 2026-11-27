// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"slices"
	"strconv"
	"unicode/utf8"

	"golang.org/x/tools/go/ast/astutil"
	ah "mvdan.cc/garble/internal/asthelper"
)

// MinSize is the lower bound limit, of the size of string-like literals
// which we will obfuscate. This is needed in order for binary size to stay relatively
// moderate, this also decreases the likelihood for performance slowdowns.
const MinSize = 8

// MaxSize is the upper limit for the per-byte AST obfuscators.
const MaxSize = 2 << 10 // 2 KiB

// MaxSizeExpensive is the upper limit for using expensive obfuscators (split, seed).
// Above this size, only cheap obfuscators are used.
const MaxSizeExpensive = 256

const (
	// minStringJunkBytes defines the minimum number of junk bytes to prepend or append during string obfuscation.
	minStringJunkBytes = 2
	// maxStringJunkBytes defines the maximum number of junk bytes to prepend or append during string obfuscation.
	maxStringJunkBytes = 8
)

// NameProviderFunc defines a function type that generates a string based on a random source and a base name.
type NameProviderFunc func(rand *mathrand.Rand, baseName string) string

// EmbeddedValue bypasses the minimum size threshold for resolved //go:embed data.
type EmbeddedValue struct {
	Data   []byte
	String bool
}

// GlobalInit certifies a generated side-effect-free factory for one global.
// The declaration name is retained by pointer so subsequent renaming is reflected.
type GlobalInit struct {
	Global  *ast.Ident
	Factory string
}

// Obfuscate replaces literals with obfuscated anonymous functions.
func Obfuscate(rand *mathrand.Rand, file *ast.File, info *types.Info, linkStrings map[*types.Var]string, embeds map[ast.Expr]EmbeddedValue, staticInits map[*types.Var]bool, nameFunc NameProviderFunc) *ast.File {
	file, _ = ObfuscateWithGlobalInits(rand, file, info, linkStrings, embeds, staticInits, nameFunc)
	return file
}

// staticInits holds variables whose initializer must stay a compile-time
// constant, such as the compiler's read-only coverage metadata variable.
func ObfuscateWithGlobalInits(rand *mathrand.Rand, file *ast.File, info *types.Info, linkStrings map[*types.Var]string, embeds map[ast.Expr]EmbeddedValue, staticInits map[*types.Var]bool, nameFunc NameProviderFunc, summaries ...map[string]bool) (*ast.File, []GlobalInit) {
	var constructors map[string]bool
	if len(summaries) != 0 {
		constructors = summaries[0]
	} else {
		constructors = ConstructorSummaries([]*ast.File{file}, info, nil)
	}
	or := newObfRand(rand, file, nameFunc)
	or.typeParams = newTypeParamSpellings(file, info)
	var globalInits []GlobalInit
	// Package initializers, including their function literals, keep a shared
	// dispatcher. Each original function declaration gets a separate tree so
	// a live function cannot retain another function's lifted decoders.
	packageDispatcher := or.proxyDispatcher
	dispatchers := []*proxyDispatcher{packageDispatcher}
	pre := func(cursor *astutil.Cursor) bool {
		switch node := cursor.Node().(type) {
		case *ast.FuncDecl:
			// Obfuscating literals can push the stack frame over the //go:nosplit limit,
			// which is just 800 bytes. These funcs are mostly in the runtime,
			// so obfuscating strings in these is less important in any case.
			if node.Doc != nil {
				for _, comment := range node.Doc.List {
					dir, ok := ast.ParseDirective(token.NoPos, comment.Text)
					if ok && dir.Tool == "go" && dir.Name == "nosplit" {
						return false
					}
				}
			}
			// Cache decoder recipes only within an original function, including
			// its nested closures, never across package initializer scopes.
			or.stringCalls = make(map[string]*ast.CallExpr)
			owner := len(dispatchers)
			or.proxyDispatcher = newProxyDispatcher(rand, func(rand *mathrand.Rand, base string) string {
				return nameFunc(rand, fmt.Sprintf("literalOwner%d_%s", owner, base))
			})
			dispatchers = append(dispatchers, or.proxyDispatcher)
		case *ast.GenDecl:
			// constants are obfuscated by replacing all references with the obfuscated value
			if node.Tok == token.CONST {
				return false
			}
		case *ast.ValueSpec:
			if skipLiteralInit(node, info, staticInits) {
				// Keep the initializer static and do not descend into it,
				// as any decoded expression would run at initialization time.
				return false
			}
			hadValues := len(node.Values) > 0
			for i, name := range node.Names {
				obj := info.Defs[name].(*types.Var)
				value, ok := linkStrings[obj]
				if !ok || !types.Identical(obj.Type(), types.Typ[types.String]) {
					continue
				}
				if len(node.Values) == 0 {
					for range node.Names {
						node.Values = append(node.Values, &ast.BasicLit{Kind: token.STRING, Value: `""`})
					}
				} else if hadValues && (len(node.Values) != len(node.Names) ||
					(info.Types[node.Values[i]].Value == nil && !isEmbeddedPlaceholder(node.Values[i], embeds))) {
					// Like cmd/link, leave non-constant initializers alone. An
					// embedded variable has no initializer in the source, so -X
					// still applies to it; see isEmbeddedPlaceholder.
					continue
				}
				// Like source literals, values above MaxSize still get
				// obfuscated; pickObfuscator falls back to the blob decoder
				// for them. Only values below MinSize stay in plaintext.
				expr := ast.Expr(&ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(value)})
				if len(value) >= MinSize {
					expr = obfuscateString(or, value)
				}
				node.Values[i] = withPos(expr, name.Pos()).(ast.Expr)
			}
			if len(node.Names) == 1 && len(node.Values) == 1 && node.Names[0].Name != "_" {
				obj := info.Defs[node.Names[0]].(*types.Var)
				value := node.Values[0]
				var constructorType ast.Expr
				if _, ok := ast.Unparen(value).(*ast.CallExpr); ok && obj.Parent() == obj.Pkg().Scope() && constructorVariables(value, info, nil) && constructorExpr(value, info, nil, func(fn *types.Func) bool { return constructors[fn.FullName()] }) {
					found := false
					ast.Inspect(value, func(n ast.Node) bool {
						if e, ok := n.(ast.Expr); ok {
							v := info.Types[e]
							if v.Type == types.Typ[types.String] && v.Value != nil && v.Value.Kind() == constant.String {
								size := len(constant.StringVal(v.Value))
								found = found || size >= MinSize && size <= MaxSize
							}
						}
						return true
					})
					if found {
						constructorType = constructorResult(file, info, obj.Type())
					}
				}
				if obj.Parent() == obj.Pkg().Scope() && types.Identical(obj.Type(), info.TypeOf(value)) && (pureStringInit(value, info) || constructorType != nil) {
					resultType := ast.Expr(ast.NewIdent("string"))
					if constructorType != nil {
						resultType = constructorType
					}
					if lit, ok := ast.Unparen(value).(*ast.CompositeLit); ok {
						resultType = lit.Type
						if array, ok := resultType.(*ast.ArrayType); ok {
							if _, ok := array.Len.(*ast.Ellipsis); ok {
								resultType = &ast.ArrayType{Len: ah.IntLit(int(obj.Type().Underlying().(*types.Array).Len())), Elt: array.Elt}
							}
						}
					}
					factory := or.globalLiteralFactory(resultType, func() ast.Expr {
						return astutil.Apply(value, func(c *astutil.Cursor) bool {
							expr, ok := c.Node().(ast.Expr)
							if !ok || info.Types[expr].Value == nil {
								return true
							}
							v := info.Types[expr]
							if v.Type == types.Typ[types.String] && v.Value.Kind() == constant.String {
								text := constant.StringVal(v.Value)
								if len(text) >= MinSize && len(text) <= MaxSize {
									c.Replace(withPos(obfuscateString(or, text), expr.Pos()))
									return false
								}
							}
							return v.Value.Kind() == constant.String
						}, nil).(ast.Expr)
					})
					globalInits = append(globalInits, GlobalInit{Global: node.Names[0], Factory: factory})
					node.Values[0] = withPos(ah.CallExprByName(factory), value.Pos()).(ast.Expr)
					return false
				}
			}

		case ast.Expr:
			if value, ok := embeds[node]; ok {
				var replacement ast.Node
				if value.String {
					replacement = obfuscateString(or, string(value.Data))
				} else {
					replacement = obfuscateBytes(or, false, value.Data, 0)
				}
				cursor.Replace(withPos(replacement, node.Pos()))
				return false
			}
			// A constant string converted to a byte or rune slice has an
			// exact capacity. Decoding first can make the allocation larger.
			if call, ok := node.(*ast.CallExpr); ok && len(call.Args) == 1 {
				if slice, ok := info.TypeOf(call.Fun).Underlying().(*types.Slice); ok && (byteElemType(slice.Elem()) || slice.Elem() == types.Universe.Lookup("rune").Type()) {
					elem := slice.Elem()
					if value := info.Types[call.Args[0]].Value; value != nil && value.Kind() == constant.String {
						text := constant.StringVal(value)
						if len(text) >= MinSize {
							size := len(text)
							if elem == types.Universe.Lookup("rune").Type() {
								size = utf8.RuneCountInString(text)
							}
							name := or.nameFunc(or.rnd, "literalSlice")
							result := ast.NewIdent(name)
							converted := ah.CallExpr(call.Fun, obfuscateString(or, text))
							body := ah.BlockStmt(
								ah.AssignDefineStmt(result, converted),
								ah.ReturnStmt(&ast.SliceExpr{
									X:    ast.NewIdent(name),
									High: ah.IntLit(size),
									Max:  ah.IntLit(size),
								}),
							)
							cursor.Replace(withPos(ah.LambdaCall(nil, call.Fun, body, nil), node.Pos()))
							return false
						}
					}
				}
			}
			// Rewrite &[]byte{...} as a whole on the way down. Rewriting the
			// composite literal on its own would leave behind &(decoder()),
			// which is not addressable, and parentheses in the input would
			// hide the literal from a check on the way up.
			//
			// See issue #520.
			if unary, ok := node.(*ast.UnaryExpr); ok && unary.Op == token.AND {
				if lit, ok := ast.Unparen(unary.X).(*ast.CompositeLit); ok {
					if newNode := handleCompositeLiteral(or, true, lit, info); newNode != nil {
						cursor.Replace(newNode)
						return false
					}
				}
			}

			// The compiler folds constant expressions, so only the outermost
			// one exists at run time. Rewrite that one and stop descending,
			// rather than also rewriting its operands into dead decoders.
			typeAndValue := info.Types[node]
			if typeAndValue.Value == nil {
				break
			}
			if typeAndValue.Value.Kind() != constant.String {
				// Constants of other kinds, such as len or unsafe.Sizeof, do
				// not evaluate their operands at all. Rewriting them would be
				// useless, and would stop uses which require a constant, such
				// as array lengths or keyed indexes, from compiling.
				return false
			}
			byteStringArg := false
			if typeAndValue.Type == types.Typ[types.UntypedString] {
				if call, ok := cursor.Parent().(*ast.CallExpr); ok && len(call.Args) == 2 && call.Args[1] == node {
					if ident, ok := call.Fun.(*ast.Ident); ok {
						if builtin, ok := info.Uses[ident].(*types.Builtin); ok &&
							(builtin.Name() == "copy" || builtin.Name() == "append" && call.Ellipsis.IsValid()) {
							if slice, ok := info.TypeOf(call.Args[0]).Underlying().(*types.Slice); ok && slice.Elem() == types.Universe.Lookup("byte").Type() {
								byteStringArg = true
							}
						}
					}
				}
			}
			if isPlainString(typeAndValue.Type) || byteStringArg {
				value := constant.StringVal(typeAndValue.Value)
				if len(value) >= MinSize {
					cursor.Replace(withPos(obfuscateString(or, value), node.Pos()))
					return false
				}
			}
			// A constant whose type is a named string type is the same literal
			// with a conversion around it, so leaving it alone keeps the bytes in
			// plaintext for no reason: `Env("secret")`, `var e Env = "secret"`,
			// a map key, a struct field or a call argument all record the named
			// type instead of plain string. Keep the conversion and decode its
			// operand, which leaves the type unchanged. A reference to a
			// constant declaration gains nothing, because the declaration keeps
			// its bytes anyway and the decoder would only repeat them.
			if named, ok := types.Unalias(typeAndValue.Type).(*types.Named); ok {
				if basic, ok := named.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
					value := constant.StringVal(typeAndValue.Value)
					if stringLiteralText(node) && len(value) >= MinSize {
						if typeExpr := namedTypeExpr(file, info, named); typeExpr != nil {
							cursor.Replace(withPos(ah.CallExpr(typeExpr, obfuscateString(or, value)), node.Pos()))
							return false
						}
					}
				}
			}
			// A constant whose recorded type is a type parameter is decodable
			// too: the decoded value is a string, and only a conversion to the
			// parameter makes it assignable again, so the rewrite keeps the
			// shape the same and adds that conversion. The conversion is valid
			// exactly when a string converts to every type in the parameter's
			// type set, which also rejects an unconstrained parameter.
			if tp, ok := typeAndValue.Type.(*types.TypeParam); ok && stringLiteralText(node) {
				value := constant.StringVal(typeAndValue.Value)
				if name, ok := or.typeParams.spelling(tp); ok && len(value) >= MinSize &&
					types.ConvertibleTo(types.Typ[types.String], tp) {
					// The identifier is part of the file the renaming pass walks
					// next, so it is bound to the parameter's object and renamed
					// with the declaration; the spelling above is the current
					// one, before that pass runs.
					typeIdent := ast.NewIdent(name)
					info.Uses[typeIdent] = tp.Obj()
					cursor.Replace(withPos(ah.CallExpr(typeIdent, obfuscateString(or, value)), node.Pos()))
					return false
				}
			}
			// Keep descending: a string constant we left alone, such as one
			// below MinSize or one of a named type, may still have operands
			// which we can rewrite.
		}
		return true
	}

	post := func(cursor *astutil.Cursor) bool {
		if _, ok := cursor.Node().(*ast.FuncDecl); ok {
			or.proxyDispatcher = packageDispatcher
			or.stringCalls = nil
		}
		// Here we handle the plain []byte{...} or [4]byte{...} value form.
		// The pointer form was already replaced on the way down; a literal
		// which we declined there is declined here just the same.
		if lit, ok := cursor.Node().(*ast.CompositeLit); ok {
			if newNode := handleCompositeLiteral(or, false, lit, info); newNode != nil {
				cursor.Replace(newNode)
			}
		}
		return true
	}

	newFile := astutil.Apply(file, pre, post).(*ast.File)
	newFile.Decls = append(newFile.Decls, or.liftedFuncs...)
	// A proxy root declares package-level state, and a package-level
	// initializer that decodes a literal depends on it. Initialization runs
	// the earliest declared variable whose dependencies are ready, so a root
	// declared after the user variables defers the initializer that needs it
	// behind the later variables which read the side effect it writes. All
	// roots are therefore declared ahead of the first variable of this file.
	roots := &ast.File{}
	for _, dispatcher := range dispatchers {
		dispatcher.AddToFile(roots)
	}
	if len(roots.Decls) > 0 {
		pos := declGapPos(newFile)
		for _, decl := range roots.Decls {
			pinGenerated(decl, pos)
		}
		first := 0
		for first < len(newFile.Decls) {
			if decl, ok := newFile.Decls[first].(*ast.GenDecl); ok && decl.Tok == token.IMPORT {
				first++
				continue
			}
			break
		}
		newFile.Decls = slices.Insert(newFile.Decls, first, roots.Decls...)
	}
	return newFile, globalInits
}

// declGapPos reports a position between the last import and the first
// declaration, so a generated declaration pinned to it is printed after the
// imports and before any declaration of the original source.
func declGapPos(file *ast.File) token.Pos {
	if n := len(file.Imports); n > 0 {
		return file.Imports[n-1].End()
	}
	return file.Name.End()
}

// skipLiteralInit reports whether any name declared by spec must keep a static
// initializer. The compiler marks its coverage metadata variable read-only and
// passes that symbol's address to the runtime, so a decoded initializer would
// be stored into read-only data and fault at startup.
func skipLiteralInit(spec *ast.ValueSpec, info *types.Info, staticInits map[*types.Var]bool) bool {
	if len(staticInits) == 0 {
		return false
	}
	for _, name := range spec.Names {
		if obj, ok := info.Defs[name].(*types.Var); ok && staticInits[obj] {
			return true
		}
	}
	return false
}

// isPlainString reports whether t is the predeclared string type, directly or
// through an alias. `type S = string` records a *types.Alias whose Unalias
// result is the basic string type, so comparing only with
// types.Typ[types.String] left every literal that names the alias in plaintext,
// while a defined string type records a *types.Named and keeps the
// conversion-wrapping rewrite.
func isPlainString(t types.Type) bool {
	return types.Unalias(t) == types.Typ[types.String]
}

// typeParamSpellings maps a type parameter to the spelling its declaration has
// in this file. A constant in a generic body is typed by the type parameter
// itself - a local variable of type T, an element of []T, a map key, a call
// argument and a switch case all record T instead of the predeclared string -
// so the decoded value has to be converted back to T, and that conversion needs
// the declaration's spelling. Identifiers are renamed before this pass, so the
// type object keeps the original name while the identifier carries the new one.
//
// A spelling that a declaration inside the same function shadows resolves to a
// different object at that point, so the conversion would name the wrong type.
// Such a parameter is marked ambiguous and left alone, which keeps today's
// plaintext rather than producing a build that does not compile.
type typeParamSpellings struct {
	names     map[*types.TypeParam]string
	ambiguous map[*types.TypeParam]bool
}

func newTypeParamSpellings(file *ast.File, info *types.Info) *typeParamSpellings {
	s := &typeParamSpellings{
		names:     make(map[*types.TypeParam]string),
		ambiguous: make(map[*types.TypeParam]bool),
	}
	// declared returns the type parameters a field list declares. A receiver
	// declares its type parameters inside the type of its only field
	// (`func (g genericRecv[T])`), which has no names of its own.
	declared := func(list *ast.FieldList) []*types.TypeParam {
		if list == nil {
			return nil
		}
		var tparams []*types.TypeParam
		record := func(ident *ast.Ident) {
			obj := info.Defs[ident]
			if obj == nil {
				return
			}
			if tp, ok := obj.Type().(*types.TypeParam); ok {
				s.names[tp] = ident.Name
				tparams = append(tparams, tp)
			}
		}
		for _, field := range list.List {
			for _, ident := range field.Names {
				record(ident)
			}
			ast.Inspect(field.Type, func(node ast.Node) bool {
				if ident, ok := node.(*ast.Ident); ok {
					record(ident)
				}
				return true
			})
		}
		return tparams
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncDecl:
			tparams := declared(node.Type.TypeParams)
			tparams = append(tparams, declared(node.Recv)...)
			if len(tparams) == 0 {
				return true
			}
			byName := make(map[string]*types.TypeParam, len(tparams))
			for _, tp := range tparams {
				byName[s.names[tp]] = tp
			}
			ast.Inspect(node, func(node ast.Node) bool {
				ident, ok := node.(*ast.Ident)
				if !ok {
					return true
				}
				tp, ok := byName[ident.Name]
				if !ok {
					return true
				}
				obj := info.Defs[ident]
				if obj == nil {
					obj = info.Uses[ident]
				}
				if obj != nil && obj.Type() != tp {
					s.ambiguous[tp] = true
				}
				return true
			})
			return false
		case *ast.TypeSpec:
			declared(node.TypeParams)
			return false
		}
		return true
	})
	return s
}

// spelling reports the identifier a conversion to tp has to use.
func (s *typeParamSpellings) spelling(tp *types.TypeParam) (string, bool) {
	if s == nil {
		return "", false
	}
	name, ok := s.names[tp]
	if !ok || s.ambiguous[tp] {
		return "", false
	}
	return name, true
}

// stringLiteralText reports whether an expression carries source text for a
// constant string: the literal itself, or the operand of a conversion. A
// reference to a constant declaration does not, and decoding it would only turn
// a constant into runtime work without removing any source bytes.
func stringLiteralText(expr ast.Expr) bool {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		return expr.Kind == token.STRING
	case *ast.CallExpr:
		return len(expr.Args) == 1 && stringLiteralText(expr.Args[0])
	}
	return false
}

// pureStringInit admits strings and aggregates with constant leaves.
// Even unevaluated references to variables must retain their init dependencies.
func pureStringInit(expr ast.Expr, info *types.Info) bool {
	found := false
	var pure func(ast.Expr) bool
	pure = func(expr ast.Expr) bool {
		if value := info.Types[expr]; value.Value != nil {
			// Named string conversions can contain independently obfuscated operands.
			if value.Value.Kind() == constant.String && value.Type != types.Typ[types.String] {
				return false
			}
			if value.Type == types.Typ[types.String] && value.Value.Kind() == constant.String {
				size := len(constant.StringVal(value.Value))
				found = found || size >= MinSize && size <= MaxSize
			}
			return true
		}
		nested, ok := ast.Unparen(expr).(*ast.CompositeLit)
		if !ok {
			return false
		}
		switch info.TypeOf(nested).Underlying().(type) {
		case *types.Array, *types.Struct, *types.Map:
		default:
			return false
		}
		for _, elem := range nested.Elts {
			if keyed, ok := elem.(*ast.KeyValueExpr); ok {
				if _, isMap := info.TypeOf(nested).Underlying().(*types.Map); isMap && !pure(keyed.Key) {
					return false
				}
				elem = keyed.Value
			}
			if !pure(elem) {
				return false
			}
		}
		return true
	}
	noVars := true
	ast.Inspect(expr, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && !info.Types[call.Fun].IsType() {
			if ident, ok := call.Fun.(*ast.Ident); !ok {
				noVars = false
			} else if _, ok := info.Uses[ident].(*types.Builtin); !ok {
				noVars = false
			}
		}
		if ident, ok := node.(*ast.Ident); ok {
			if obj, ok := info.Uses[ident].(*types.Var); ok {
				// Field keys are variables too, but are not evaluated references.
				noVars = noVars && obj.IsField()
			}
		}
		return true
	})
	return noVars && pure(expr) && found
}

// The proxy roots are local to the factory, so no unconditional package-level
// initialization can retain a dead global's decoder or change init dependencies.
func (r *obfRand) globalLiteralFactory(resultType ast.Expr, value func() ast.Expr) string {
	name := r.nextLiftedFuncName("literalGlobal")
	shared := r.proxyDispatcher
	r.proxyDispatcher = newProxyDispatcher(r.rnd, func(rand *mathrand.Rand, base string) string {
		return r.nameFunc(rand, name+base)
	})
	call := value()
	locals := &ast.File{}
	r.proxyDispatcher.addToFile(locals, true)
	r.proxyDispatcher = shared
	body := &ast.BlockStmt{}
	for _, decl := range locals.Decls {
		body.List = append(body.List, &ast.DeclStmt{Decl: decl})
	}
	body.List = append(body.List, ah.ReturnStmt(call))
	r.liftedFuncs = append(r.liftedFuncs, &ast.FuncDecl{
		Name: ast.NewIdent(name),
		Type: &ast.FuncType{Params: &ast.FieldList{}, Results: &ast.FieldList{List: []*ast.Field{{Type: resultType}}}},
		Body: body,
	})
	return name
}

// generatedNames assigns each decoder-local name before building its AST.
// References receive fresh Ident nodes with the same generated spelling.
type generatedNames struct {
	r      *obfRand
	names  map[string]string
	inputs *decoderInputs
}

func newGeneratedNames(r *obfRand) *generatedNames {
	return &generatedNames{r: r, names: make(map[string]string)}
}

func (n *generatedNames) name(original string) string {
	if name, ok := n.names[original]; ok {
		return name
	}
	name := n.r.nameFunc(n.r.rnd, "literalLocal"+original)
	n.names[original] = name
	return name
}

func (n *generatedNames) ident(original string) *ast.Ident {
	return ast.NewIdent(n.name(original))
}

// liftCall moves a literal decoder out of its owner function and makes it a
// top-level function. The function value remains hidden in the proxy tree, so
// calls stay indirect and the compiler cannot inline the decoder back into its
// owner. Literal-heavy owners therefore no longer accumulate thousands of
// decoder closures or their inlined bodies.
func (r *obfRand) liftCall(params *ast.FieldList, resultType ast.Expr, block *ast.BlockStmt, args []ast.Expr) *ast.CallExpr {
	name := r.nextLiftedFuncName("literalDecoder")
	results := &ast.FieldList{List: []*ast.Field{{Type: resultType}}}
	r.liftedFuncs = append(r.liftedFuncs, &ast.FuncDecl{
		Name: ast.NewIdent(name),
		Type: &ast.FuncType{
			Params:  params,
			Results: results,
		},
		Body: block,
	})
	hiddenType := &ast.FuncType{
		Params:  unnamedFieldList(params),
		Results: unnamedFieldList(results),
	}
	callee := r.proxyDispatcher.HideValue(ast.NewIdent(name), hiddenType)
	return ah.CallExpr(callee, args...)
}

// liftFuncValue turns a generated function literal into an independently
// compiled top-level function while still returning it through the proxy
// dispatcher. In particular, this keeps thousands of []byte-to-string helper
// bodies out of the proxy root's global initializer; the initializer retains
// only their function values.
func (r *obfRand) liftFuncValue(funcVal *ast.FuncLit) ast.Expr {
	name := r.nextLiftedFuncName("literalHelper")
	r.liftedFuncs = append(r.liftedFuncs, &ast.FuncDecl{
		Name: ast.NewIdent(name),
		Type: funcVal.Type,
		Body: funcVal.Body,
	})
	return ast.NewIdent(name)
}

func unnamedFieldList(fields *ast.FieldList) *ast.FieldList {
	if fields == nil {
		return nil
	}
	result := &ast.FieldList{}
	for _, field := range fields.List {
		count := max(1, len(field.Names))
		for range count {
			result.List = append(result.List, &ast.Field{Type: cloneGeneratedType(field.Type)})
		}
	}
	return result
}

// cloneGeneratedType copies the small set of type expressions emitted by the
// literal obfuscator. Keeping the proxy field's function type independent from
// the lifted declaration avoids sharing AST nodes between two syntax parents.
func cloneGeneratedType(expr ast.Expr) ast.Expr {
	switch expr := expr.(type) {
	case *ast.Ident:
		return ast.NewIdent(expr.Name)
	case *ast.BasicLit:
		return &ast.BasicLit{Kind: expr.Kind, Value: expr.Value}
	case *ast.ArrayType:
		var length ast.Expr
		if expr.Len != nil {
			length = cloneGeneratedType(expr.Len)
		}
		return &ast.ArrayType{Len: length, Elt: cloneGeneratedType(expr.Elt)}
	case *ast.StarExpr:
		return &ast.StarExpr{X: cloneGeneratedType(expr.X)}
	case *ast.FuncType:
		return &ast.FuncType{
			Params:  unnamedFieldList(expr.Params),
			Results: unnamedFieldList(expr.Results),
		}
	default:
		panic(fmt.Sprintf("unsupported generated literal type %T", expr))
	}
}

func (r *obfRand) nextLiftedFuncName(base string) string {
	name := r.nameFunc(r.rnd, fmt.Sprintf("%s%d", base, r.liftedFuncSeq))
	r.liftedFuncSeq++
	return name
}

// byteElemType reports whether elem is a byte element type: byte itself, its
// canonical uint8 spelling, an alias of either, or a defined type whose
// underlying type is uint8. Comparing the element type with the universe byte
// object only matched the spelling "byte", because byte is an alias and
// go/types keeps the alias, so the same element type written as uint8 or through
// an alias was not recognized.
func byteElemType(elem types.Type) bool {
	basic, ok := types.Unalias(elem).Underlying().(*types.Basic)
	return ok && basic.Kind() == types.Uint8
}

// handleCompositeLiteral checks if the input node is a byte slice or byte array
// composite literal and calls the appropriate obfuscation method, returning a
// new node that should be used to replace it.
//
// The literal may spell its type as []byte, as [N]byte, or as a defined type or
// alias whose underlying type is one of those. The spelled type decides whether
// the replacement needs a conversion: a decoder returns an unnamed byte slice or
// array, so any other spelling has to convert the decoded value back.
//
// If the input node cannot be obfuscated nil is returned.
func handleCompositeLiteral(or *obfRand, isPointer bool, node *ast.CompositeLit, info *types.Info) ast.Node {
	if len(node.Elts) < MinSize {
		return nil
	}

	byteType := types.Typ[types.Uint8]

	writtenType := info.TypeOf(node.Type)
	if writtenType == nil {
		return nil
	}
	underlying := types.Unalias(writtenType).Underlying()

	var arrayLen int64
	// A type parameter is neither a slice nor an array to go/types, even when
	// its type set holds exactly one byte slice or byte array, so it has to be
	// recognized before the written type's underlying type is consulted. Every
	// other spelling, including a defined type or an alias, is decided by the
	// underlying type, which is where its elements are visible.
	var typeParam *types.TypeParam
	if tp, ok := types.Unalias(writtenType).(*types.TypeParam); ok {
		var ok bool
		arrayLen, ok = byteTypeParamCore(tp)
		if !ok {
			return nil
		}
		typeParam = tp
	}
	switch y := underlying.(type) {
	case *types.Array:
		// The decoder returns an unnamed byte array or slice, which is only
		// assignable to the written type when its element type is byte itself.
		// A defined element type, such as `type D byte`, would need a conversion
		// Go does not allow between [N]byte and [N]D, so those literals keep
		// their current form.
		if !types.Identical(types.Unalias(y.Elem()), byteType) {
			return nil
		}

		arrayLen = y.Len()

	case *types.Slice:
		if !types.Identical(types.Unalias(y.Elem()), byteType) {
			return nil
		}

	default:
		// A type parameter's underlying type is its constraint, which is an
		// interface and not the shape that decides the decoder, so the
		// parameter was already resolved above.
		if typeParam == nil {
			return nil
		}
	}

	data := make([]byte, 0, len(node.Elts))

	for _, el := range node.Elts {
		elType := info.Types[el]

		if elType.Value == nil || elType.Value.Kind() != constant.Int {
			return nil
		}

		value, ok := constant.Uint64Val(elType.Value)
		if !ok {
			panic(fmt.Sprintf("cannot parse byte value: %v", elType.Value))
		}

		data = append(data, byte(value))
	}

	if typeParam != nil {
		// The decoder returns an unnamed byte slice or array, and the declared
		// type is only known through the parameter's type set, so the decoded
		// value is converted back to the parameter the source wrote.
		replacement := convertTypeParamBytes(or, info, typeParam, node.Type, obfuscateBytes(or, false, data, arrayLen), isPointer)
		if replacement == nil {
			return nil
		}
		return withPos(replacement, node.Pos())
	}
	replacement := obfuscateBytes(or, isPointer, data, arrayLen)
	if types.Identical(writtenType, underlying) {
		return withPos(replacement, node.Pos())
	}
	// The decoder returns an unnamed byte slice or byte array, so a defined type
	// or a named alias spelling has to convert the decoded value back to the type
	// the source wrote. The address-of form needs a pointer conversion: a
	// conversion result is not addressable, so `&conversion(...)` would not
	// compile.
	typeExpr := node.Type
	if isPointer {
		typeExpr = &ast.ParenExpr{X: ah.StarExpr(typeExpr)}
	}
	return withPos(ah.CallExpr(typeExpr, replacement), node.Pos())
}

// byteTypeParamCore reports the array length of a type parameter whose type set
// is exactly one byte slice or one byte array, and zero for that slice.
//
// The decoder returns an unnamed []byte or [N]byte, so a defined or named
// element type is rejected for the same reason as in the array and slice cases.
// A conversion between []byte and [N]byte is legal Go, so types.ConvertibleTo
// cannot tell the two shapes apart: the type set's term decides which decoder
// result the conversion has to receive.
func byteTypeParamCore(tp *types.TypeParam) (arrayLen int64, ok bool) {
	iface, _ := tp.Constraint().Underlying().(*types.Interface)
	if iface == nil {
		return 0, false
	}
	var term types.Type
	terms := 0
	for i := range iface.NumEmbeddeds() {
		switch embedded := iface.EmbeddedType(i).(type) {
		case *types.Union:
			for j := range embedded.Len() {
				term = embedded.Term(j).Type()
				terms++
			}
		case *types.Interface, *types.TypeParam:
			// A nested constraint needs its own type-set walk; fail closed.
			return 0, false
		default:
			term = embedded
			terms++
		}
	}
	if terms != 1 {
		return 0, false
	}
	byteElem := func(elem types.Type) bool {
		return types.Identical(types.Unalias(elem), types.Typ[types.Uint8])
	}
	var result types.Type
	switch under := term.Underlying().(type) {
	case *types.Slice:
		if !byteElem(under.Elem()) {
			return 0, false
		}
		result = types.NewSlice(types.Typ[types.Uint8])
	case *types.Array:
		if !byteElem(under.Elem()) {
			return 0, false
		}
		arrayLen = under.Len()
		result = types.NewArray(types.Typ[types.Uint8], arrayLen)
	default:
		return 0, false
	}
	if !types.ConvertibleTo(result, tp) {
		return 0, false
	}
	return arrayLen, true
}

// convertTypeParamBytes converts a decoded byte slice or array back to the type
// parameter its composite literal was written with. The decoder's result is not
// assignable to a type parameter, so the conversion is required rather than
// cosmetic; the pointer form additionally needs a temporary, because a call
// result is not addressable.
//
// Both occurrences of the parameter are fresh identifiers bound to the
// parameter's type object, so the renaming pass renames them together with the
// declaration instead of leaving the original spelling behind. The spelling
// itself comes from the same per-file table the string arm uses, which leaves a
// parameter alone when a declaration inside the same function shadows its name
// and the conversion would therefore name the wrong type.
func convertTypeParamBytes(or *obfRand, info *types.Info, tp *types.TypeParam, written ast.Expr, decoded *ast.CallExpr, isPointer bool) ast.Expr {
	if _, ok := written.(*ast.Ident); !ok {
		// A type parameter is always spelled with a plain identifier.
		return nil
	}
	name, ok := or.typeParams.spelling(tp)
	if !ok {
		return nil
	}
	obj := tp.Obj()
	typeIdent := func() *ast.Ident {
		ident := ast.NewIdent(name)
		info.Uses[ident] = obj
		return ident
	}
	converted := ah.CallExpr(typeIdent(), decoded)
	if !isPointer {
		return converted
	}
	tmpName := or.nameFunc(or.rnd, "literalTypeParamPtr")
	body := ah.BlockStmt(
		ah.AssignDefineStmt(ast.NewIdent(tmpName), converted),
		ah.ReturnStmt(ah.UnaryExpr(token.AND, ast.NewIdent(tmpName))),
	)
	return ah.LambdaCall(nil, ah.StarExpr(typeIdent()), body, nil)
}

// withPos sets any token.Pos fields under node which affect printing to pos.
// Note that we can't set all token.Pos fields, since some affect the semantics.
//
// This function is useful so that go/printer doesn't try to estimate position
// offsets, which can end up in printing comment directives too early.
//
// We don't set any "end" or middle positions, because they seem irrelevant.
func withPos(node ast.Node, pos token.Pos) ast.Node {
	for node := range ast.Preorder(node) {
		switch node := node.(type) {
		case *ast.BasicLit:
			node.ValuePos = pos
		case *ast.Ident:
			node.NamePos = pos
		case *ast.CompositeLit:
			node.Lbrace = pos
			node.Rbrace = pos
		case *ast.ArrayType:
			node.Lbrack = pos
		case *ast.FuncType:
			node.Func = pos
		case *ast.BinaryExpr:
			node.OpPos = pos
		case *ast.StarExpr:
			node.Star = pos
		case *ast.CallExpr:
			node.Lparen = pos
			node.Rparen = pos

		case *ast.GenDecl:
			node.TokPos = pos
		case *ast.ReturnStmt:
			node.Return = pos
		case *ast.ForStmt:
			node.For = pos
		case *ast.RangeStmt:
			node.For = pos
		case *ast.BranchStmt:
			node.TokPos = pos
		}
	}
	return node
}

func obfuscateString(or *obfRand, data string) *ast.CallExpr {
	if call := or.stringCalls[data]; call != nil {
		return or.proxyDispatcher.cloneStringCall(call).(*ast.CallExpr)
	}
	if or.testObfuscator == nil && len(data) >= MinSize && len(data) <= maxSizeShared {
		return or.obfuscateSharedString(data)
	}
	obf := or.pickObfuscator(len(data))

	// Generate junk bytes to to prepend and append to the data.
	// This is to prevent the obfuscated string from being easily fingerprintable.
	junkBytes := make([]byte, or.rnd.Intn(maxStringJunkBytes-minStringJunkBytes)+minStringJunkBytes)
	or.rnd.Read(junkBytes)
	splitIdx := or.rnd.Intn(len(junkBytes))

	names := newGeneratedNames(or)
	extKeys := randExtKeys(or.rnd, names)

	plainData := []byte(data)
	plainDataWithJunkBytes := append(append(junkBytes[:splitIdx], plainData...), junkBytes[splitIdx:]...)

	block := obf.obfuscate(or.rnd, names, plainDataWithJunkBytes, extKeys)
	params, args := extKeysToParams(or, extKeys)

	hiddenCastFunc, funcTyp := or.stringConversionAdapter()
	castParamName := names.name("garbleStringCaster")
	params.List = append(params.List, &ast.Field{
		Names: []*ast.Ident{ast.NewIdent(castParamName)},
		Type:  cloneGeneratedType(funcTyp),
	})
	args = append(args, hiddenCastFunc)
	block.List = append(block.List, ah.ReturnStmt(ah.CallExpr(ast.NewIdent(castParamName), names.ident("data"), ah.IntLit(splitIdx), ah.IntLit(splitIdx+len(plainData)))))
	call := or.liftCall(params, ast.NewIdent("string"), block, args)
	if or.stringCalls != nil {
		or.stringCalls[data] = call
		return or.proxyDispatcher.cloneStringCall(call).(*ast.CallExpr)
	}
	return call
}

// Each use needs independent positions; only the hidden values and decoder are
// shared, not mutable AST nodes or the result of running the decoder.
func (d *proxyDispatcher) cloneStringCall(expr ast.Expr) ast.Expr {
	switch expr := expr.(type) {
	case *ast.CallExpr:
		args := make([]ast.Expr, len(expr.Args))
		for i, arg := range expr.Args {
			args[i] = d.cloneStringCall(arg)
		}
		return &ast.CallExpr{Fun: d.cloneStringCall(expr.Fun), Args: args}
	case *ast.SelectorExpr:
		return d.clonePath(expr)
	case *ast.Ident:
		return ast.NewIdent(expr.Name)
	case *ast.BasicLit:
		return &ast.BasicLit{Kind: expr.Kind, Value: expr.Value}
	case *ast.CompositeLit:
		elts := make([]ast.Expr, len(expr.Elts))
		for i, elt := range expr.Elts {
			elts[i] = d.cloneStringCall(elt)
		}
		return &ast.CompositeLit{Type: cloneGeneratedType(expr.Type), Elts: elts}
	case *ast.ArrayType:
		return cloneGeneratedType(expr)
	default:
		panic(fmt.Sprintf("unsupported string decoder expression %T", expr))
	}
}

// stringConversionAdapter shares only within the current owner's dispatcher.
// Bounds stay in each decoder, so junk and different literal lengths do not
// require separate conversion functions.
func (r *obfRand) stringConversionAdapter() (ast.Expr, *ast.FuncType) {
	d := r.proxyDispatcher
	if d.stringAdapter == nil {
		names := newGeneratedNames(r)
		funcVal := &ast.FuncLit{
			Type: &ast.FuncType{
				Params: &ast.FieldList{List: []*ast.Field{
					ah.Field(ah.ByteSliceType(), names.ident("x")),
					ah.Field(ast.NewIdent("int"), names.ident("low")),
					ah.Field(ast.NewIdent("int"), names.ident("high")),
				}},
				Results: &ast.FieldList{List: []*ast.Field{{Type: ast.NewIdent("string")}}},
			},
			Body: ah.BlockStmt(ah.ReturnStmt(ah.CallExprByName("string", &ast.SliceExpr{
				X: names.ident("x"), Low: names.ident("low"), High: names.ident("high"),
			}))),
		}
		d.stringAdapterType = cloneGeneratedType(funcVal.Type).(*ast.FuncType)
		d.stringAdapter = d.HideValue(r.liftFuncValue(funcVal), d.stringAdapterType)
	}
	return d.clonePath(d.stringAdapter), d.stringAdapterType
}

// obfuscateBytes returns a slice decoder when arrayLen is zero, otherwise an array decoder.
func obfuscateBytes(or *obfRand, isPointer bool, data []byte, arrayLen int64) *ast.CallExpr {
	obf := or.pickObfuscator(len(data))

	names := newGeneratedNames(or)
	extKeys := randExtKeys(or.rnd, names)
	block := obf.obfuscate(or.rnd, names, data, extKeys)
	params, args := extKeysToParams(or, extKeys)

	var resultType ast.Expr
	resultName := "data"
	if arrayLen > 0 {
		resultType = ah.ByteArrayType(arrayLen)
		resultName = "newdata"
		block.List = append(block.List,
			&ast.DeclStmt{
				Decl: &ast.GenDecl{
					Tok: token.VAR,
					Specs: []ast.Spec{&ast.ValueSpec{
						Names: []*ast.Ident{names.ident("newdata")},
						Type:  resultType,
					}},
				},
			},
			&ast.RangeStmt{
				Key: names.ident("i"),
				Tok: token.DEFINE,
				X:   names.ident("data"),
				Body: ah.BlockStmt(
					ah.AssignStmt(
						ah.IndexExprByExpr(names.ident("newdata"), names.ident("i")),
						ah.IndexExprByExpr(names.ident("data"), names.ident("i")),
					),
				),
			},
		)
	} else {
		resultType = ah.ByteSliceType()
	}

	var result ast.Expr = names.ident(resultName)
	if isPointer {
		result = ah.UnaryExpr(token.AND, result)
		resultType = ah.StarExpr(resultType)
	}
	block.List = append(block.List, ah.ReturnStmt(result))
	return or.liftCall(params, resultType, block, args)
}

func (or *obfRand) pickObfuscator(size int) obfuscator {
	if size < MinSize || size > MaxSize {
		return blob{}
	}
	if or.testObfuscator != nil {
		return or.testObfuscator
	}
	if size <= MaxSizeExpensive {
		return Obfuscators[or.rnd.Intn(len(Obfuscators))]
	}
	return CheapObfuscators[or.rnd.Intn(len(CheapObfuscators))]
}

// isEmbeddedPlaceholder reports whether expr is the initializer that the embed
// transform installed for a //go:embed variable. Such a variable is uninitialized
// in the source, which is the case where cmd/link applies a -X assignment, so the
// placeholder must not be mistaken for a non-constant initializer.
func isEmbeddedPlaceholder(expr ast.Expr, embeds map[ast.Expr]EmbeddedValue) bool {
	_, ok := embeds[expr]
	return ok
}
