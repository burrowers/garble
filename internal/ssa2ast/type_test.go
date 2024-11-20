package ssa2ast

import (
	"go/ast"
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
