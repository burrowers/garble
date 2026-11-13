package ssa2ast

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"testing"

	"github.com/go-quicktest/qt"
)

const typesSrc = `package main

import (
	"io"
	"time"
)

type localNamed bool

type embedStruct struct {
	int
}

type genericStruct[K comparable, V int64 | float64] struct {
	int
}

type exampleStruct struct {
	embedStruct

	// *types.Array
	array  [3]int
	array2 [0]int

	// *types.Basic
	bool              // anonymous
	string     string "test:\"tag\""
	int        int
	int8       int8
	int16      int16
	int32      int32
	int64      int64
	uint       uint
	uint8      uint8
	uint16     uint16
	uint32     uint32
	uint64     uint64
	uintptr    uintptr
	byte       byte
	rune       rune
	float32    float32
	float64    float64
	complex64  complex64
	complex128 complex128

	// *types.Chan
	chanSendRecv chan struct{}
	chanRecv     <-chan struct{}
	chanSend     chan<- struct{}

	// *types.Interface
	interface1 interface{}
	interface2 interface{ io.Reader }
	interface3 interface{ Dummy(int) bool }
	interface4 interface {
		io.Reader
		io.ByteReader
		Dummy(int) bool
	}

	// *types.Map
	strMap map[string]string

	// *types.Named
	localNamed    localNamed
	importedNamed time.Month

	// *types.Pointer
	pointer1 *string
	pointer2 **string

	// *types.Signature
	func1 func(int, int) int
	func2 func(a int, b int, varargs ...struct{ string }) (res int)

	// *types.Slice
	slice1 []int
	slice2 [][]int

	// generics
	generic genericStruct[genericStruct[genericStruct[bool, int64], int64], int64]
}
`

func TestTypeToExpr(t *testing.T) {
	f, _, info, _ := mustParseAndTypeCheckFile(typesSrc)
	name, structAst := findStruct(f, "exampleStruct")
	obj := info.Defs[name]
	fc := &TypeConverter{resolver: defaultImportNameResolver}
	convAst, err := fc.Convert(obj.Type().Underlying())
	qt.Assert(t, qt.IsNil(err))

	structConvAst := convAst.(*ast.StructType)
	qt.Assert(t, qt.CmpEquals(structConvAst, structAst, astCmpOpt))
}

func TestConvertLocalType(t *testing.T) {
	_, _, info, _ := mustParseAndTypeCheckFile(`package main

func f() {
	if true {
		type local struct{ X int }
		var _ local
	}
}
`)

	var localType *types.Named
	for _, obj := range info.Defs {
		if tn, ok := obj.(*types.TypeName); ok && tn.Name() == "local" {
			localType = tn.Type().(*types.Named)
		}
	}
	if localType == nil {
		t.Fatal("local type not found")
	}

	fc := &TypeConverter{resolver: defaultImportNameResolver}
	convAst, err := fc.Convert(localType)
	qt.Assert(t, qt.IsNil(err))

	// Local (function/block-scoped) types are inlined to their underlying
	// type, since the name is not emitted in the converted output.
	structAst, ok := convAst.(*ast.StructType)
	if !ok {
		t.Fatalf("Convert(local type) = %T, want *ast.StructType (inlined)", convAst)
	}
	qt.Assert(t, qt.Equals(len(structAst.Fields.List), 1))
	qt.Assert(t, qt.Equals(structAst.Fields.List[0].Names[0].Name, "X"))
}

func TestConvertVar(t *testing.T) {
	other := types.NewPackage("example.com/other", "other")
	newNamed := func(name string, underlying types.Type) *types.Named {
		obj := types.NewTypeName(token.NoPos, other, name, nil)
		other.Scope().Insert(obj)
		return types.NewNamed(obj, underlying, nil)
	}

	byteSlice := types.NewSlice(types.Typ[types.Uint8])
	blob := newNamed("blob", byteSlice)
	cell := newNamed("cell", types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, other, "N", types.Typ[types.Int]),
	}, nil))
	// holds has a field of a type that cannot be written either, so its whole
	// underlying type cannot be written.
	holds := newNamed("holds", types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, other, "B", blob),
	}, nil))
	methoded := newNamed("methoded", byteSlice)
	methoded.AddMethod(types.NewFunc(token.NoPos, other, "Size", types.NewSignatureType(
		types.NewVar(token.NoPos, other, "", types.NewPointer(methoded)),
		nil, nil, nil,
		types.NewTuple(types.NewVar(token.NoPos, other, "", types.Typ[types.Int])),
		false,
	)))
	exported := newNamed("Exported", byteSlice)
	basicRank := newNamed("rank", types.Typ[types.Int])

	tc := &TypeConverter{resolver: defaultImportNameResolver}
	for _, test := range []struct {
		name string
		typ  types.Type
		want string
	}{
		{"unexported slice without methods", blob, "[]uint8"},
		{"unexported struct of writable fields", cell, "struct {\n\tN int\n}"},
		{"unexported type with an exported method", methoded, "interface {\n\tSize() int\n}"},
		{"unexported struct holding an unexported type", holds, "interface {\n}"},
		{"unexported basic type", basicRank, "interface {\n}"},
		{"exported type", exported, "other.Exported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			expr, err := tc.ConvertVar(test.typ)
			qt.Assert(t, qt.IsNil(err))
			qt.Assert(t, qt.Equals(exprString(t, expr), test.want))
		})
	}
}

func exprString(t *testing.T, expr ast.Expr) string {
	t.Helper()
	var buf bytes.Buffer
	qt.Assert(t, qt.IsNil(format.Node(&buf, token.NewFileSet(), expr)))
	return buf.String()
}

func TestConvertLocalGenericType(t *testing.T) {
	_, _, info, _ := mustParseAndTypeCheckFile(`package main

func f() {
	type pair[T any] struct{ a, b T }
	var p pair[int]
	_ = p
}
`)

	var pairType *types.Named
	for _, obj := range info.Defs {
		if v, ok := obj.(*types.Var); ok && v.Name() == "p" {
			pairType = v.Type().(*types.Named)
		}
	}
	if pairType == nil {
		t.Fatal("pair type not found")
	}

	fc := &TypeConverter{resolver: defaultImportNameResolver}
	convAst, err := fc.Convert(pairType)
	qt.Assert(t, qt.IsNil(err))

	// The instantiated type's type arguments must be substituted into the
	// inlined underlying type; the generic type parameter T is out of scope.
	structAst, ok := convAst.(*ast.StructType)
	if !ok {
		t.Fatalf("Convert(pair[int]) = %T, want *ast.StructType (inlined)", convAst)
	}
	qt.Assert(t, qt.Equals(len(structAst.Fields.List), 2))
	for _, f := range structAst.Fields.List {
		qt.Assert(t, qt.Equals(f.Type.(*ast.Ident).Name, "int"))
	}
}
