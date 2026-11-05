package ssa2ast

import (
	"go/constant"
	"go/token"
	"go/types"
	"testing"

	"github.com/go-quicktest/qt"
)

// TestFoldsLikeRuntime pins the positions where the generated source may not
// spell an operation out as a constant expression: the type checker evaluates
// those exactly and rejects a value the original program produces by wrapping,
// truncating or bounds-checking at run time.
func TestFoldsLikeRuntime(t *testing.T) {
	folds := []token.Token{token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.SHL}
	for _, op := range folds {
		qt.Assert(t, qt.IsTrue(foldsExactly(op)), qt.Commentf("operator %v", op))
	}
	for _, op := range []token.Token{token.EQL, token.LSS, token.AND, token.OR, token.XOR, token.SHR} {
		qt.Assert(t, qt.IsFalse(foldsExactly(op)), qt.Commentf("operator %v", op))
	}

	// A conversion of a value the destination can hold keeps the constant
	// form, so the generated source stays as close to the original as it was.
	kept := []struct {
		value constant.Value
		to    types.Type
	}{
		{constant.MakeInt64(127), types.Typ[types.Int8]},
		{constant.MakeInt64(-128), types.Typ[types.Int8]},
		{constant.MakeInt64(255), types.Typ[types.Int32]},
		{constant.MakeUint64(4294967295), types.Typ[types.Int64]},
		{constant.MakeInt64(3), types.Typ[types.Float64]},
		{constant.MakeFloat64(0.5), types.Typ[types.Float64]},
	}
	for _, test := range kept {
		qt.Assert(t, qt.IsFalse(conversionRejects(test.value, test.to)),
			qt.Commentf("%v to %v", test.value, test.to))
	}

	// A sign change, a narrowing, a float truncation or an overflow is a
	// constant conversion error where the runtime conversion wraps or rounds.
	rejected := []struct {
		value constant.Value
		to    types.Type
	}{
		{constant.MakeUint64(4294967295), types.Typ[types.Int32]},
		{constant.MakeInt64(-1), types.Typ[types.Uint32]},
		{constant.MakeInt64(128), types.Typ[types.Int8]},
		{constant.MakeInt64(256), types.Typ[types.Uint8]},
		{constant.MakeInt64(1 << 40), types.Typ[types.Int32]},
		{constant.MakeFloat64(1e30), types.Typ[types.Int32]},
		{constant.MakeFloat64(1e30), types.Typ[types.Float32]},
		{constant.MakeInt64(1), types.Typ[types.Float32]},
	}
	for _, test := range rejected {
		qt.Assert(t, qt.IsTrue(conversionRejects(test.value, test.to)),
			qt.Commentf("%v to %v", test.value, test.to))
	}

	// A string or a bool conversion cannot overflow, and a struct destination
	// is not a basic type at all.
	qt.Assert(t, qt.IsFalse(conversionRejects(constant.MakeInt64(300), types.Typ[types.String])))
	qt.Assert(t, qt.IsFalse(conversionRejects(constant.MakeInt64(300), types.Typ[types.Bool])))
}
