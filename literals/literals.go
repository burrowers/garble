package literals

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"mvdan.cc/garble/literals/obfuscators"
)

func getCallexpr(resultType ast.Expr, block *ast.BlockStmt) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.FuncLit{
			Type: &ast.FuncType{
				Params: &ast.FieldList{},
				Results: &ast.FieldList{
					List: []*ast.Field{
						{Type: resultType},
					},
				},
			},
			Body: block,
		},
	}
}

func getObfuscator() obfuscators.Obfuscator {
	randPos := mathrand.Intn(len(obfuscators.Obfuscators))
	return obfuscators.Obfuscators[randPos]
}

func getReturnStmt(result ast.Expr) *ast.ReturnStmt {
	return &ast.ReturnStmt{
		Results: []ast.Expr{result},
	}
}

func isTypeDefStr(typ types.Type) bool {
	strType := types.Typ[types.String]

	if named, ok := typ.(*types.Named); ok {
		return types.Identical(named.Underlying(), strType)
	}

	return false
}

func containsTypeDefStr(expr ast.Expr, info *types.Info) bool {
	typ := info.TypeOf(expr)
	// log.Println(expr, typ, reflect.TypeOf(expr), reflect.TypeOf(typ))

	if sig, ok := typ.(*types.Signature); ok {
		for i := 0; i < sig.Params().Len(); i++ {
			if isTypeDefStr(sig.Params().At(i).Type()) {
				return true
			}
		}
	}

	if mapT, ok := typ.(*types.Map); ok {
		return isTypeDefStr(mapT.Elem()) || isTypeDefStr(mapT.Key())
	}

	if named, ok := typ.(*types.Named); ok {
		return isTypeDefStr(named)
	}

	return false
}

// Obfuscate replace literals with obfuscated lambda functions
func Obfuscate(files []*ast.File, info *types.Info, blacklist map[types.Object]struct{}) []*ast.File {
	pre := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.ValueSpec:
			return !containsTypeDefStr(x.Type, info)

		case *ast.AssignStmt:
			for _, expr := range x.Lhs {
				if index, ok := expr.(*ast.IndexExpr); ok {
					return !containsTypeDefStr(index.X, info)
				}

				if ident, ok := expr.(*ast.Ident); ok {
					return !containsTypeDefStr(ident, info)
				}
			}
		case *ast.CallExpr:
			return !containsTypeDefStr(x.Fun, info)

		case *ast.CompositeLit:
			if t, ok := x.Type.(*ast.MapType); ok {
				return !(containsTypeDefStr(t.Key, info) || containsTypeDefStr(t.Value, info))
			}

		case *ast.FuncDecl:
			if x.Type.Results == nil {
				return true
			}
			for _, result := range x.Type.Results.List {
				for _, name := range result.Names {
					return !containsTypeDefStr(name, info)
				}
			}

		case *ast.KeyValueExpr:
			if ident, ok := x.Key.(*ast.Ident); ok {
				return !containsTypeDefStr(ident, info)
			}
		case *ast.GenDecl:
			if x.Tok != token.CONST {
				return true
			}
			for _, spec := range x.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					return false
				}

				for _, name := range spec.Names {
					obj := info.ObjectOf(name)

					// The object itself is blacklisted, e.g. a value that needs to be constant
					if _, ok := blacklist[obj]; ok {
						return false
					}
				}

				for _, val := range spec.Values {
					if _, ok := val.(*ast.BasicLit); !ok {
						return false // skip the block if it contains non basic literals
					}
				}

			}

			x.Tok = token.VAR
			// constants are not possible if we want to obfuscate literals, therefore
			// move all constant blocks which only contain strings to variables

		}
		return true
	}

	post := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.Ident:
			obj := info.ObjectOf(x)
			if obj == nil {
				return true
			}

			if obj.Type() == types.Typ[types.Bool] || obj.Type() == types.Typ[types.UntypedBool] {
				if obj.Name() == "true" || obj.Name() == "false" {
					cursor.Replace(obfuscateBool(x.Name == "true"))
				}
			}

		case *ast.CompositeLit:
			if info.TypeOf(x.Type).String() == "[]byte" {
				var data []byte

				for _, el := range x.Elts {
					lit, ok := el.(*ast.BasicLit)
					if !ok {
						return true
					}

					value, err := strconv.Atoi(lit.Value)
					if err != nil {
						return true
					}

					data = append(data, byte(value))
				}

				cursor.Replace(obfuscateByte(data))
			}
		case *ast.UnaryExpr:
			switch cursor.Name() {
			case "Values", "Rhs", "Value", "Args", "X":
			default:
				return true // we don't want to obfuscate imports etc.
			}

			obfuscateNumberLiteral(cursor, info)

		case *ast.BasicLit:
			_, ok := cursor.Parent().(*ast.UnaryExpr)
			if ok {
				break
			}
			switch cursor.Name() {
			case "Values", "Rhs", "Value", "Args", "X":
			default:
				return true // we don't want to obfuscate imports etc.
			}

			switch x.Kind {
			case token.FLOAT, token.INT:
				obfuscateNumberLiteral(cursor, info)

			case token.STRING:
				value, err := strconv.Unquote(x.Value)
				if err != nil {
					panic(fmt.Sprintf("cannot unquote string: %v", err))
				}

				cursor.Replace(obfuscateString(value))
			}
		}

		return true
	}

	for i := range files {
		files[i] = astutil.Apply(files[i], pre, post).(*ast.File)
	}
	return files
}

func obfuscateNumberLiteral(cursor *astutil.Cursor, info *types.Info) error {
	var (
		call     *ast.CallExpr
		basic    *ast.BasicLit
		ok       bool
		typeInfo types.Type
	)

	sign := ""
	node := cursor.Node()

	switch x := node.(type) {
	case *ast.UnaryExpr:
		basic, ok = x.X.(*ast.BasicLit)
		if !ok {
			return errors.New("UnaryExpr doesn't contain basic literal")
		}
		typeInfo = info.TypeOf(x)

		if x.Op != token.SUB {
			return errors.New("UnaryExpr has a non SUB token")
		}
		sign = "-"

		switch y := cursor.Parent().(type) {
		case *ast.ValueSpec:
			tempInfo := info.TypeOf(y.Type)
			if tempInfo != nil {
				typeInfo = tempInfo
			}
		}

	case *ast.BasicLit:
		basic = x
		typeInfo = info.TypeOf(x)

	default:
		return errors.New("Wrong node Type")
	}

	strValue := sign + basic.Value

	switch typeInfo {
	case types.Typ[types.Float32]:
		fV, err := strconv.ParseFloat(strValue, 32)
		if err != nil {
			panic(err)
		}
		call = obfuscateFloat32(float32(fV))
	case types.Typ[types.Float64], types.Typ[types.UntypedFloat]:
		fV, err := strconv.ParseFloat(strValue, 64)
		if err != nil {
			panic(err)
		}
		call = obfuscateFloat64(fV)
	}

	if call != nil {
		cursor.Replace(call)
		return nil
	}

	// Explicitly typed integers can have a decimal place
	splitStrValue := strings.Split(strValue, ".")
	intValue, err := strconv.Atoi(splitStrValue[0])
	if err != nil {
		panic(err)
	}

	switch typeInfo {
	case types.Typ[types.Int], types.Typ[types.UntypedInt]:
		call = obfuscateInt(intValue)
	case types.Typ[types.Int8]:
		call = obfuscateInt8(int8(intValue))
	case types.Typ[types.Int16]:
		call = obfuscateInt16(int16(intValue))
	case types.Typ[types.Int32]:
		call = obfuscateInt32(int32(intValue))
	case types.Typ[types.Int64]:
		call = obfuscateInt64(int64(intValue))
	case types.Typ[types.Uint]:
		call = obfuscateUint(uint(intValue))
	case types.Typ[types.Uint8]:
		call = obfuscateUint8(uint8(intValue))
	case types.Typ[types.Uint16]:
		call = obfuscateUint16(uint16(intValue))
	case types.Typ[types.Uint32]:
		call = obfuscateUint32(uint32(intValue))
	case types.Typ[types.Uint64]:
		call = obfuscateUint64(uint64(intValue))
	case types.Typ[types.Uintptr]:
		call = obfuscateUintptr(uintptr(intValue))
	}

	if call == nil {
		return errors.New("Node is not Integer")
	}

	cursor.Replace(call)
	return nil
}

// ConstBlacklist blacklist identifieres used in constant expressions
func ConstBlacklist(node ast.Node, info *types.Info, blacklist map[types.Object]struct{}) {
	blacklistObjects := func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}

		obj := info.ObjectOf(ident)
		blacklist[obj] = struct{}{}

		return true
	}

	switch x := node.(type) {
	// in a slice or array composite literal all explicit keys must be constant representable
	case *ast.CompositeLit:
		if _, ok := x.Type.(*ast.ArrayType); !ok {
			break
		}
		for _, elt := range x.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				ast.Inspect(kv.Key, blacklistObjects)
			}
		}
	// in an array type the length must be a constant representable
	case *ast.ArrayType:
		if x.Len != nil {
			ast.Inspect(x.Len, blacklistObjects)
		}

	// in a const declaration all values must be constant representable
	case *ast.GenDecl:
		if x.Tok != token.CONST {
			break
		}
		for _, spec := range x.Specs {
			spec := spec.(*ast.ValueSpec)

			for _, val := range spec.Values {
				ast.Inspect(val, blacklistObjects)
			}
		}
	}
}
