package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"
	"strconv"
	"testing"
)

func TestByteSliceDecoderCapacityMatchesLiteral(t *testing.T) {
	data := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	for _, tc := range []struct {
		name string
		obf  obfuscator
	}{
		{"seed", seed{}},
		{"shuffle", shuffle{}},
		{"split", split{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Inspect the decoder before its call-site wrapper is chosen. The
			// capacity contract is independent of whether that wrapper is a
			// closure or a lifted top-level function.
			rnd := mathrand.New(mathrand.NewSource(1))
			decoder := tc.obf.obfuscate(
				rnd,
				append([]byte(nil), data...),
				randExtKeys(rnd),
			)

			var capacities []int
			ast.Inspect(decoder, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 3 {
					return true
				}
				fun, ok := call.Fun.(*ast.Ident)
				if !ok || fun.Name != "make" || !isGeneratedByteSliceType(call.Args[0]) {
					return true
				}
				literal, ok := call.Args[2].(*ast.BasicLit)
				if !ok || literal.Kind != token.INT {
					t.Fatalf("make capacity=%T, want integer literal", call.Args[2])
				}
				capacity, err := strconv.Atoi(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				capacities = append(capacities, capacity)
				return true
			})
			if len(capacities) != 1 || capacities[0] != len(data) {
				t.Fatalf("decoded capacities=%v, want [%d]", capacities, len(data))
			}
		})
	}
}

func isGeneratedByteSliceType(expr ast.Expr) bool {
	array, ok := expr.(*ast.ArrayType)
	if !ok || array.Len != nil {
		return false
	}
	element, ok := array.Elt.(*ast.Ident)
	return ok && element.Name == "byte"
}
