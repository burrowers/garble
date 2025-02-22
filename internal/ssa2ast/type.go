package ssa2ast

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
)

type TypeConverter struct {
	resolver ImportNameResolver
}

func NewTypeConverted(resolver ImportNameResolver) *TypeConverter {
	return &TypeConverter{resolver: resolver}
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

		// TODO: rewrite struct inlining without reflection hack
		if parent := obj.Parent(); parent != nil {
			isFuncScope := reflect.ValueOf(parent).Elem().FieldByName("isFunc")
			if isFuncScope.Bool() {
				return tc.Convert(obj.Type().Underlying())
			}
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
		funcSigExpr := &ast.FuncType{Params: &ast.FieldList{}}
		if sigParams := typ.Params(); sigParams != nil {
			for i := range sigParams.Len() {
				param := sigParams.At(i)

				var paramType ast.Expr
				if typ.Variadic() && i == sigParams.Len()-1 {
					slice := param.Type().(*types.Slice)

					eltExpr, err := tc.Convert(slice.Elem())
					if err != nil {
						return nil, err
					}
					paramType = &ast.Ellipsis{Elt: eltExpr}
				} else {
					paramExpr, err := tc.Convert(param.Type())
					if err != nil {
						return nil, err
					}
					paramType = paramExpr
				}
				f := &ast.Field{Type: paramType}
				if name := param.Name(); name != "" {
					f.Names = []*ast.Ident{ast.NewIdent(name)}
				}
				funcSigExpr.Params.List = append(funcSigExpr.Params.List, f)
			}
		}
		if sigResults := typ.Results(); sigResults != nil {
			funcSigExpr.Results = &ast.FieldList{}
			for i := range sigResults.Len() {
				result := sigResults.At(i)
				resultExpr, err := tc.Convert(result.Type())
				if err != nil {
					return nil, err
				}

				f := &ast.Field{Type: resultExpr}
				if name := result.Name(); name != "" {
					f.Names = []*ast.Ident{ast.NewIdent(name)}
				}
				funcSigExpr.Results.List = append(funcSigExpr.Results.List, f)
			}
		}
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
