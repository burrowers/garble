package asthelper

import (
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// TestConstToAstValueExact checks that a constant is emitted as an expression
// whose value is the constant itself. constant.Value.String reports a decimal
// approximation rounded to six significant digits, so re-emitting a float that
// way silently changes the value the transformed program computes.
func TestConstToAstValueExact(t *testing.T) {
	t.Parallel()

	floatLit := func(s string) constant.Value {
		return constant.MakeFromLiteral(s, token.FLOAT, 0)
	}
	one := floatLit("1.0")
	third := constant.BinaryOp(one, token.QUO, floatLit("3.0"))
	negThird := constant.UnaryOp(token.SUB, third, 0)

	tests := []struct {
		name string
		val  constant.Value
	}{
		{"third", third},
		{"negative third", negThird},
		{"one over seven", constant.BinaryOp(one, token.QUO, floatLit("7.0"))},
		{"seven significant digits", floatLit("0.1234567")},
		{"nine significant digits", floatLit("123456.789")},
		{"denormal", floatLit("5e-324")},
		{"integer valued", floatLit("6.0")},
		{"large exponent", floatLit("1e308")},
		{"small exponent", floatLit("1e-9")},
		{"float32 rounded", constant.MakeFloat64(float64(float32(0.1234567)))},
		{"complex", constant.BinaryOp(
			constant.MakeImag(third),
			token.ADD,
			constant.BinaryOp(constant.MakeFloat64(2), token.QUO, constant.MakeFloat64(7)),
		)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr := ConstToAst(tc.val)
			var buf strings.Builder
			if err := format.Node(&buf, token.NewFileSet(), expr); err != nil {
				t.Fatalf("formatting the generated expression: %v", err)
			}
			tv, err := types.Eval(token.NewFileSet(), types.NewPackage("p", "p"), token.NoPos, buf.String())
			if err != nil {
				t.Fatalf("evaluating %s: %v", buf.String(), err)
			}
			if tv.Value == nil {
				t.Fatalf("%s is not a constant expression", buf.String())
			}
			if tv.Value.Kind() != tc.val.Kind() {
				t.Fatalf("%s has kind %v, want %v", buf.String(), tv.Value.Kind(), tc.val.Kind())
			}
			for _, cmp := range compareParts(tc.val) {
				got := cmp.pick(tv.Value)
				want := cmp.pick(tc.val)
				if got == nil || !constant.Compare(got, token.EQL, want) {
					t.Fatalf("%s evaluates to %s, want %s", buf.String(), tv.Value.ExactString(), tc.val.ExactString())
				}
			}
		})
	}
}

type part struct {
	pick func(constant.Value) constant.Value
}

// compareParts returns the value parts worth comparing; a complex constant must
// match in both its real and its imaginary component.
func compareParts(val constant.Value) []part {
	if val.Kind() != constant.Complex {
		return []part{{pick: func(v constant.Value) constant.Value { return v }}}
	}
	return []part{
		{pick: constant.Real},
		{pick: constant.Imag},
	}
}
