package ssa2ast

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
)

// ErrUnwritableVarType reports a value that has to survive a block boundary but
// whose type the converted function-level declaration cannot write. A value of a
// named type is assignable only to its own type, to an unnamed type with an
// identical underlying type, or to an interface it implements, and an unexported
// type of another package can be written as none of the first two.
var ErrUnwritableVarType = errors.New("type cannot be written from another package, and no unnamed equivalent is assignable")

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

// VarUses reports how the values that share a cross-block declaration are used.
// One declaration has to keep every operation of the group, and the operations
// split into two kinds that a substitute type can only keep one of: a use that
// needs the value's method set, and a use that needs its own type or its
// underlying type. ConvertVar picks the substitute from this information
// instead of from the type alone.
type VarUses struct {
	// MethodSet reports a use that needs the value's method set: a call of
	// one of its methods, a method value, or a conversion of the value to an
	// interface, which the underlying type would change the dynamic type of.
	MethodSet bool
	// Concrete reports a use that needs the value's own or underlying type:
	// len, cap, indexing, slicing, ranging, append, copy, field access, or
	// passing the value where the underlying type is wanted.
	Concrete bool
}

// ConvertVar converts the type of a variable declared by the converted
// function body, rather than the type of an expression inside it.
//
// A cross-block variable holds every operation its SSA value supports, not
// only method calls, so the declaration has to keep the value assignable. A
// value of a named type is assignable to its own type, to an interface it
// implements, and to an unnamed type with an identical underlying type. The
// first is unavailable for a type of another package that is not exported, and
// the second keeps only the exported methods, which loses len, indexing,
// slicing, ranging, field access and argument passing. The third is the useful
// substitute where it exists: an unexported method cannot be called from
// another package anyway, and a value of the named type is assignable to an
// unnamed type with the same structure.
//
// Where none of the three can be written the conversion fails with an
// ErrUnwritableVarType error naming the type. A named basic type is the clearest
// example: every predeclared basic type is a named type too, so the value is not
// assignable to the underlying type that could be written for it. Emitting the
// substituted declaration anyway would leave a typecheck error in the generated
// source that no longer mentions the value's type.
func (tc *TypeConverter) ConvertVar(typ types.Type, uses VarUses) (ast.Expr, error) {
	declared, ok := tc.varType(typ, uses)
	if !ok {
		return nil, fmt.Errorf("type %s: %w", typ, ErrUnwritableVarType)
	}
	return tc.Convert(declared)
}

// varType reports the type to declare for a cross-block variable of typ, and
// whether the converted source can declare one at all. It tries the type
// itself first, then the substitutes that keep as much of the value as the
// generated file can express.
func (tc *TypeConverter) varType(typ types.Type, uses VarUses) (types.Type, bool) {
	candidates := []types.Type{typ}
	if named, ok := types.Unalias(typ).(*types.Named); ok {
		obj := named.Obj()
		switch {
		case tc.canName(typ):
			// Written as itself, which the candidate above already covers.
		case obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope():
			// A named type declared in a local (function or block) scope is
			// not part of the converted output, so Convert writes its
			// underlying type.
			candidates = append(candidates, named.Underlying())
		default:
			// An unexported type of another package. The interface of its
			// exported methods keeps the method calls, but only when the
			// value implements it, which a pointer-receiver method alone does
			// not. It also replaces every other operation the value supports,
			// so it is only the declaration of a group whose values are not
			// used for anything but their method set; a group used any other
			// way takes the underlying type, which is writable and
			// assignable, and keeps len, indexing, slicing, ranging, field
			// access and argument passing.
			if iface := exportedMethodInterface(named); iface != nil && (uses.MethodSet || !uses.Concrete) {
				candidates = append(candidates, iface)
			}
			candidates = append(candidates, named.Underlying())
		}
	}
	for _, candidate := range candidates {
		// Convert has to write the declared type as itself, and the value of
		// the SSA type has to stay assignable to it. Both fail together for
		// the shapes of this class that the type system does not let the
		// generated file express, such as a named basic type or a pointer to
		// an unexported struct of another package.
		if tc.spellable(candidate) && types.AssignableTo(typ, candidate) {
			return candidate, true
		}
	}
	return nil, false
}

// exportedMethodInterface reports the interface of a named type's exported
// methods, or nil when it has none.
func exportedMethodInterface(named *types.Named) *types.Interface {
	var methods []*types.Func
	for i := range named.NumMethods() {
		method := named.Method(i)
		if !token.IsExported(method.Name()) {
			continue
		}
		// An interface method carries no receiver, and the assignability
		// check that consumes this interface needs one that does not.
		sig := method.Type().(*types.Signature)
		methods = append(methods, types.NewFunc(method.Pos(), method.Pkg(), method.Name(),
			types.NewSignatureType(nil, nil, nil, sig.Params(), sig.Results(), sig.Variadic())))
	}
	if len(methods) == 0 {
		return nil
	}
	return types.NewInterfaceType(methods, nil).Complete()
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
		if typeParams := typ.TypeParams(); typeParams != nil {
			for i := range typeParams.Len() {
				// Convert writes the underlying type of the constraint.
				if !tc.spellable(typeParams.At(i).Constraint().Underlying()) {
					return false
				}
			}
		}
		return tc.spellableTuple(typ.Params()) && tc.spellableTuple(typ.Results())
	case *types.TypeParam:
		// Convert writes the type parameter's own name.
		return true
	case *types.Union:
		for i := range typ.Len() {
			if !tc.spellable(typ.Term(i).Type()) {
				return false
			}
		}
		return true
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
