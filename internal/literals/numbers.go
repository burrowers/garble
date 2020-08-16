package literals

import (
	"encoding/binary"
	"errors"
	"go/ast"
	"go/token"
	"go/types"
	"math"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	ah "mvdan.cc/garble/internal/asthelper"
)

var intTypes = map[types.Type]reflect.Type{
	types.Typ[types.UntypedInt]: reflect.TypeOf(int(0)),
	types.Typ[types.Int]:        reflect.TypeOf(int(0)),
	types.Typ[types.Int8]:       reflect.TypeOf(int8(0)),
	types.Typ[types.Int16]:      reflect.TypeOf(int16(0)),
	types.Typ[types.Int32]:      reflect.TypeOf(int32(0)),
	types.Typ[types.Int64]:      reflect.TypeOf(int64(0)),
	types.Typ[types.Uint]:       reflect.TypeOf(uint(0)),
	types.Typ[types.Uint8]:      reflect.TypeOf(uint8(0)),
	types.Typ[types.Uint16]:     reflect.TypeOf(uint16(0)),
	types.Typ[types.Uint32]:     reflect.TypeOf(uint32(0)),
	types.Typ[types.Uint64]:     reflect.TypeOf(uint64(0)),
	types.Typ[types.Uintptr]:    reflect.TypeOf(uintptr(0)),
}

func obfuscateNumberLiteral(cursor *astutil.Cursor, info *types.Info, obfLits obfuscateLiterals) (bool, error) {
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
			return false, errors.New("UnaryExpr doesn't contain basic literal")
		}
		typeInfo = info.TypeOf(x)

		if x.Op != token.SUB {
			return false, errors.New("UnaryExpr has a non SUB token")
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

		switch typeInfo {
		case types.Typ[types.UntypedFloat], types.Typ[types.UntypedInt]:
			// The post calls from astutil.Apply can be out of order,
			// this guards against the case where the ast.BasicLit is inside an ast.UnaryExpr
			// and the BasicLit gets evaluated before the UnaryExpr
			if _, ok := cursor.Parent().(*ast.UnaryExpr); ok {
				return true, nil
			}
		}

	default:
		return false, errors.New("wrong node Type")
	}

	strValue := sign + basic.Value

	switch typeInfo {
	case types.Typ[types.Float32]:
		if obfLits&obfuscateFloats != 0 {
			fV, err := strconv.ParseFloat(strValue, 32)
			if err != nil {
				return false, err
			}
			call = genObfuscateFloat(float32(fV))
		}
	case types.Typ[types.Float64], types.Typ[types.UntypedFloat]:
		if obfLits&obfuscateFloats != 0 {
			fV, err := strconv.ParseFloat(strValue, 64)
			if err != nil {
				return false, err
			}
			call = genObfuscateFloat(fV)
		}
	}

	if call != nil {
		cursor.Replace(call)
		return true, nil
	}

	if obfLits&obfuscateIntegers == 0 {
		return false, nil
	}

	intValue, err := strconv.ParseInt(strings.Split(strValue, ".")[0], 0, 64)
	if err != nil {
		return false, err
	}

	intType, ok := intTypes[typeInfo]
	if !ok {
		return false, nil
	}

	call = genObfuscateInt(uint64(intValue), intType)

	cursor.Replace(call)
	return true, nil
}

func bytesToUint(bits int) ast.Expr {
	bytes := bits / 8
	bitsStr := strconv.Itoa(bits)

	var expr ast.Expr
	for i := 0; i < bytes; i++ {
		if i == 0 {
			expr = ah.CallExpr(ah.Ident("uint"+bitsStr), ah.IndexExpr("data", ah.IntLit(i)))
			continue
		}

		shiftValue := i * 8
		expr = &ast.BinaryExpr{
			X:  expr,
			Op: token.OR,
			Y: &ast.BinaryExpr{
				X:  ah.CallExpr(ah.Ident("uint"+bitsStr), ah.IndexExpr("data", ah.IntLit(i))),
				Op: token.SHL,
				Y:  ah.IntLit(shiftValue),
			},
		}
	}

	return expr
}

func genObfuscateInt(data uint64, typeInfo reflect.Type) *ast.CallExpr {
	obfuscator := randObfuscator()
	bitsize := typeInfo.Bits()

	bitSizeStr := strconv.Itoa(bitsize)
	byteSize := bitsize / 8
	b := make([]byte, byteSize)

	switch bitsize {
	case 8:
		b = []byte{uint8(data)}
	case 16:
		binary.LittleEndian.PutUint16(b, uint16(data))
	case 32:
		binary.LittleEndian.PutUint32(b, uint32(data))
	case 64:
		binary.LittleEndian.PutUint64(b, uint64(data))
	default:
		panic("data has the wrong length " + bitSizeStr)
	}

	block := obfuscator.obfuscate(b)
	convertExpr := bytesToUint(bitsize)

	block.List = append(block.List, ah.BoundsCheck("data", byteSize-1), ah.ReturnStmt(ah.CallExpr(ah.Ident(typeInfo.Name()), convertExpr)))

	return ah.LambdaCall(ah.Ident(typeInfo.Name()), block)
}

func uintToFloat(uintExpr *ast.CallExpr, typeStr string) *ast.CallExpr {
	usesUnsafe = true
	convert := &ast.StarExpr{
		X: ah.CallExpr(
			&ast.ParenExpr{
				X: &ast.StarExpr{X: ah.Ident(typeStr)},
			},
			ah.CallExpr(
				&ast.SelectorExpr{
					X:   ah.Ident("unsafe"),
					Sel: ah.Ident("Pointer"),
				},
				&ast.UnaryExpr{
					Op: token.AND,
					X:  ah.Ident("result"),
				},
			),
		),
	}

	block := &ast.BlockStmt{List: []ast.Stmt{
		&ast.AssignStmt{
			Lhs: []ast.Expr{ah.Ident("result")},
			Tok: token.DEFINE,
			Rhs: []ast.Expr{uintExpr},
		},
		ah.ReturnStmt(convert),
	}}

	return ah.LambdaCall(ah.Ident(typeStr), block)
}

func genObfuscateFloat(data interface{}) *ast.CallExpr {
	var (
		b       uint64
		typeStr string
		intType reflect.Type
	)

	switch x := data.(type) {
	case float32:
		intType = intTypes[types.Typ[types.Uint32]]
		typeStr = "float32"
		b = uint64(math.Float32bits(x))
	case float64:
		intType = intTypes[types.Typ[types.Uint64]]
		typeStr = "float64"
		b = math.Float64bits(x)
	default:
		panic("data has the wrong type")
	}

	return uintToFloat(genObfuscateInt(b, intType), typeStr)
}
