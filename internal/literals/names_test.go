package literals

import (
	"fmt"
	"go/ast"
	"math/rand"
	"strings"
	"testing"
)

func TestDecodersObfuscateGeneratedNames(t *testing.T) {
	const literal = "long_enough_string"
	cases := []struct {
		name  string
		build func(*obfRand)
	}{
		{"string", func(r *obfRand) { obfuscateString(r, literal) }},
		{"slice", func(r *obfRand) { obfuscateByteSlice(r, false, []byte(literal)) }},
		{"slice pointer", func(r *obfRand) { obfuscateByteSlice(r, true, []byte(literal)) }},
		{"array", func(r *obfRand) { obfuscateByteArray(r, false, []byte(literal), int64(len(literal))) }},
		{"array pointer", func(r *obfRand) { obfuscateByteArray(r, true, []byte(literal), int64(len(literal))) }},
	}
	for _, obf := range Obfuscators {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%T/%s", obf, tc.name), func(t *testing.T) {
				r := newObfRand(rand.New(rand.NewSource(1)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
				r.testObfuscator = obf
				tc.build(r)
				for _, decl := range r.liftedFuncs {
					ast.Inspect(decl, func(node ast.Node) bool {
						if id, ok := node.(*ast.Ident); ok {
							switch id.Name {
							case "data", "key", "fullData", "idxKey", "positions", "localKey", "seed", "decFunc", "fnc", "x", "b", "decryptKey", "counter", "i", "y", "newdata", "garbleStringCaster":
								t.Errorf("generated identifier %q was not obfuscated", id.Name)
							}
							if strings.HasPrefix(id.Name, "garbleExternalKey") {
								t.Errorf("external key %q was not obfuscated", id.Name)
							}
						}
						return true
					})
				}
			})
		}
	}
}

func TestGeneratedLocalNames(t *testing.T) {
	r := newObfRand(rand.New(rand.NewSource(1)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
	names := newGeneratedNames(r)
	first := names.ident("data")
	if first.Name == "data" || names.ident("data").Name != first.Name {
		t.Fatalf("generated local was not consistently named: %q", first.Name)
	}
	if names.ident("other").Name == first.Name {
		t.Fatal("distinct generated locals have the same name")
	}
}
