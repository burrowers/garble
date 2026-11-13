package ssa2ast

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
)

type TypeConverter struct {
	resolver ImportNameResolver
}

func NewTypeConverted(resolver ImportNameResolver) *TypeConverter {
	return &TypeConverter{resolver: resolver}
}

// canName reports whether Convert writes typ as the named type itself, rather
// than as a substitute such as its underlying type or an emulated interface for
// an unexported type of another package.
func (tc *TypeConverter) canName(typ types.Type) bool {
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj.Pkg() != nil && obj.Parent() != obj.Pkg().Scope() {
		return false
	}
	return tc.resolver(obj.Pkg()) == nil || token.IsExported(obj.Name())
}

// ConvertVar converts the type of a variable declared by the converted
// function body, rather than the type of an expression inside it.
//
// A cross-block variable holds every operation its SSA value supports, not
// only method calls. A named type of another package that is not exported
// cannot be written, and the interface that stands in for it keeps only the
// exported methods, so len, indexing, slicing, ranging and passing the value as
// an argument stop typechecking once the value crosses a block boundary. When
// that interface would carry no method at all, the underlying type is the
// better substitute: an unexported method cannot be called from another
// package anyway, and a value of the named type stays assignable to its
// underlying type.
//
// The substitute has to be an unnamed type, so a named basic type keeps the
// emulated interface: every predeclared basic type is a named type as well, and
// a value of a named basic type is not assignable to its underlying type.
func (tc *TypeConverter) ConvertVar(typ types.Type) (ast.Expr, error) {
	if underlying, ok := tc.varUnderlying(typ); ok {
		return tc.Convert(underlying)
	}
	return tc.Convert(typ)
}

// varUnderlying reports the type to declare for a variable of typ when the
// emulated interface would drop operations the value still has.
func (tc *TypeConverter) varUnderlying(typ types.Type) (types.Type, bool) {
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok || tc.canName(typ) {
		return nil, false
	}
	for i := range named.NumMethods() {
		if token.IsExported(named.Method(i).Name()) {
			// The interface exists to keep these method calls.
			return nil, false
		}
	}
	underlying := named.Underlying()
	if _, basic := underlying.(*types.Basic); basic {
		// A predeclared basic type is a named type too, so the value is not
		// assignable to its underlying type either; that declaration would
		// need a conversion the generated file cannot write.
		return nil, false
	}
	// A value is assignable to the declared variable only when both share an
	// identical underlying type, so every named type inside it has to be
	// written as itself as well.
	if !tc.spellable(underlying) {
		return nil, false
	}
	return underlying, true
}

// spellable reports whether Convert writes typ as itself, without substituting
// an underlying type or an emulated interface anywhere inside it.
func (tc *TypeConverter) spellable(typ types.Type) bool {
	switch typ := types.Unalias(typ).(type) {
	case *types.Basic:
		return true
	case *types.Named:
		return tc.canName(typ)
	case *types.Array:
		return tc.spellable(typ.Elem())
	case *types.Chan:
		return tc.spellable(typ.Elem())
	case *types.Interface:
		for i := range typ.NumEmbeddeds() {
			if !tc.spellable(typ.EmbeddedType(i)) {
				return false
			}
		}
		for i := range typ.NumExplicitMethods() {
			if !tc.spellable(typ.ExplicitMethod(i).Type()) {
				return false
			}
		}
		return true
	case *types.Map:
		return tc.spellable(typ.Key()) && tc.spellable(typ.Elem())
	case *types.Pointer:
		return tc.spellable(typ.Elem())
	case *types.Signature:
		return tc.spellableTuple(typ.Params()) && tc.spellableTuple(typ.Results())
	case *types.Slice:
		return tc.spellable(typ.Elem())
	case *types.Struct:
		for i := range typ.NumFields() {
			if !tc.spellable(typ.Field(i).Type()) {
				return false
			}
		}
		return true
	}
	return false
}

func (tc *TypeConverter) spellableTuple(tuple *types.Tuple) bool {
	if tuple == nil {
		return true
	}
	for i := range tuple.Len() {
		if !tc.spellable(tuple.At(i).Type()) {
			return false
		}
	}
	return true
}

func (tc *TypeConverter) Convert(typ types.Type) (ast.Expr, error) {
	switch typ := types.Unalias(typ).(type) {
	case *types.Array:
		eltExpr, err := tc.Convert(typ.Elem())
		if err != nil {
			return nil, err
		}
		return &ast.ArrayType{
			Len: &ast.BasicLit{
				Kind:  token.INT,
				Value: strconv.FormatInt(typ.Len(), 10),
			},
			Elt: eltExpr,
		}, nil
	case *types.Basic:
		if typ.Kind() == types.UnsafePointer {
			unsafePkgIdent := tc.resolver(types.Unsafe)
			if unsafePkgIdent == nil {
				return nil, fmt.Errorf("cannot resolve unsafe package")
			}
			return &ast.SelectorExpr{X: unsafePkgIdent, Sel: ast.NewIdent("Pointer")}, nil
		}
		return ast.NewIdent(typ.Name()), nil
	case *types.Chan:
		chanValueExpr, err := tc.Convert(typ.Elem())
		if err != nil {
			return nil, err
		}
		chanExpr := &ast.ChanType{Value: chanValueExpr}
		switch typ.Dir() {
		case types.SendRecv:
			chanExpr.Dir = ast.SEND | ast.RECV
		case types.RecvOnly:
			chanExpr.Dir = ast.RECV
		case types.SendOnly:
			chanExpr.Dir = ast.SEND
		}
		return chanExpr, nil
	case *types.Interface:
		methods := &ast.FieldList{}
		hasComparable := false
		for i := range typ.NumEmbeddeds() {
			embeddedType := typ.EmbeddedType(i)
			if namedType, ok := embeddedType.(*types.Named); ok && namedType.String() == "comparable" {
				hasComparable = true
			}
			embeddedExpr, err := tc.Convert(embeddedType)
			if err != nil {
				return nil, err
			}
			methods.List = append(methods.List, &ast.Field{Type: embeddedExpr})
		}

		// Special case, handle "comparable" interface itself
		if !hasComparable && typ.IsComparable() {
			methods.List = append(methods.List, &ast.Field{Type: ast.NewIdent("comparable")})
		}
		for i := range typ.NumExplicitMethods() {
			method := typ.ExplicitMethod(i)
			methodSig, err := tc.Convert(method.Type())
			if err != nil {
				return nil, err
			}
			methods.List = append(methods.List, &ast.Field{
				Names: []*ast.Ident{ast.NewIdent(method.Name())},
				Type:  methodSig,
			})
		}
		return &ast.InterfaceType{Methods: methods}, nil
	case *types.Map:
		keyExpr, err := tc.Convert(typ.Key())
		if err != nil {
			return nil, err
		}
		valueExpr, err := tc.Convert(typ.Elem())
		if err != nil {
			return nil, err
		}
		return &ast.MapType{Key: keyExpr, Value: valueExpr}, nil
	case *types.Named:
		obj := typ.Obj()

		// A named type declared in a local (function or block) scope is not
		// part of the converted output, so reference its underlying type.
		if obj.Pkg() != nil && obj.Parent() != obj.Pkg().Scope() {
			return tc.Convert(typ.Underlying())
		}

		var namedExpr ast.Expr
		if pkgIdent := tc.resolver(obj.Pkg()); pkgIdent != nil {
			// reference to unexported named emulated through new interface with explicit declarated methods
			if !token.IsExported(obj.Name()) {
				var methods []*types.Func
				for i := range typ.NumMethods() {
					method := typ.Method(i)
					if token.IsExported(method.Name()) {
						methods = append(methods, method)
					}
				}

				fakeInterface := types.NewInterfaceType(methods, nil)
				return tc.Convert(fakeInterface)
			}
			namedExpr = &ast.SelectorExpr{X: pkgIdent, Sel: ast.NewIdent(obj.Name())}
		} else {
			namedExpr = ast.NewIdent(obj.Name())
		}

		typeParams := typ.TypeArgs()
		if typeParams == nil || typeParams.Len() == 0 {
			return namedExpr, nil
		}
		if typeParams.Len() == 1 {
			typeParamExpr, err := tc.Convert(typeParams.At(0))
			if err != nil {
				return nil, err
			}
			return &ast.IndexExpr{X: namedExpr, Index: typeParamExpr}, nil
		}
		genericExpr := &ast.IndexListExpr{X: namedExpr}
		for i := range typeParams.Len() {
			typeArgs := typeParams.At(i)
			typeParamExpr, err := tc.Convert(typeArgs)
			if err != nil {
				return nil, err
			}
			genericExpr.Indices = append(genericExpr.Indices, typeParamExpr)
		}
		return genericExpr, nil
	case *types.Pointer:
		expr, err := tc.Convert(typ.Elem())
		if err != nil {
			return nil, err
		}
		return &ast.StarExpr{X: expr}, nil
	case *types.Signature:
		params, err := tc.convertTuple(typ.Params(), typ.Variadic())
		if err != nil {
			return nil, err
		}
		if params == nil {
			params = &ast.FieldList{}
		}
		results, err := tc.convertTuple(typ.Results(), false)
		if err != nil {
			return nil, err
		}
		funcSigExpr := &ast.FuncType{Params: params, Results: results}
		if typeParams := typ.TypeParams(); typeParams != nil {
			funcSigExpr.TypeParams = &ast.FieldList{}
			for i := range typeParams.Len() {
				typeParam := typeParams.At(i)
				resultExpr, err := tc.Convert(typeParam.Constraint().Underlying())
				if err != nil {
					return nil, err
				}
				f := &ast.Field{Type: resultExpr, Names: []*ast.Ident{ast.NewIdent(typeParam.Obj().Name())}}
				funcSigExpr.TypeParams.List = append(funcSigExpr.TypeParams.List, f)
			}
		}
		return funcSigExpr, nil
	case *types.Slice:
		eltExpr, err := tc.Convert(typ.Elem())
		if err != nil {
			return nil, err
		}
		return &ast.ArrayType{Elt: eltExpr}, nil
	case *types.Struct:
		fieldList := &ast.FieldList{}
		for i := range typ.NumFields() {
			f := typ.Field(i)
			fieldExpr, err := tc.Convert(f.Type())
			if err != nil {
				return nil, err
			}
			field := &ast.Field{Type: fieldExpr}
			if !f.Anonymous() {
				field.Names = []*ast.Ident{ast.NewIdent(f.Name())}
			}
			if tag := typ.Tag(i); len(tag) > 0 {
				field.Tag = &ast.BasicLit{Kind: token.STRING, Value: fmt.Sprintf("%q", tag)}
			}
			fieldList.List = append(fieldList.List, field)
		}
		return &ast.StructType{Fields: fieldList}, nil
	case *types.TypeParam:
		return ast.NewIdent(typ.Obj().Name()), nil
	case *types.Union:
		var unionExpr ast.Expr
		for i := range typ.Len() {
			term := typ.Term(i)
			expr, err := tc.Convert(term.Type())
			if err != nil {
				return nil, err
			}
			if term.Tilde() {
				expr = &ast.UnaryExpr{Op: token.TILDE, X: expr}
			}
			if unionExpr == nil {
				unionExpr = expr
			} else {
				unionExpr = &ast.BinaryExpr{X: unionExpr, Op: token.OR, Y: expr}
			}
		}
		return unionExpr, nil
	default:
		return nil, fmt.Errorf("type %v: %w", typ, ErrUnsupported)
	}
}

// convertTuple converts function parameters or results, preserving their names.
func (tc *TypeConverter) convertTuple(tuple *types.Tuple, variadic bool) (*ast.FieldList, error) {
	if tuple == nil {
		return nil, nil
	}
	fields := &ast.FieldList{}
	for i := range tuple.Len() {
		v := tuple.At(i)
		typ := v.Type()
		ellipsis := variadic && i == tuple.Len()-1
		if ellipsis {
			typ = typ.(*types.Slice).Elem()
		}
		expr, err := tc.Convert(typ)
		if err != nil {
			return nil, err
		}
		if ellipsis {
			expr = &ast.Ellipsis{Elt: expr}
		}
		field := &ast.Field{Type: expr}
		if name := v.Name(); name != "" {
			field.Names = []*ast.Ident{ast.NewIdent(name)}
		}
		fields.List = append(fields.List, field)
	}
	return fields, nil
}
