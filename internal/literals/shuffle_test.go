// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"math/rand"
	"testing"
)

func shuffleProbe(t *testing.T, seed int64, size int) (*ast.BlockStmt, *generatedNames, []*externalKey) {
	t.Helper()
	r := newObfRand(rand.New(rand.NewSource(seed)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
	names := newGeneratedNames(r)
	keys := randExtKeys(r.rnd, names)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	return (shuffle{}).obfuscate(r.rnd, names, data, keys), names, keys
}

func TestShuffleBoundedOperations(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		counts := []int{}
		for _, size := range []int{8, MaxSizeExpensive} {
			body, _, keys := shuffleProbe(t, seed, size)
			var first, repeated bytes.Buffer
			if err := printer.Fprint(&first, token.NewFileSet(), body); err != nil {
				t.Fatal(err)
			}
			again, _, _ := shuffleProbe(t, seed, size)
			if err := printer.Fprint(&repeated, token.NewFileSet(), again); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Bytes(), repeated.Bytes()) {
				t.Fatal("operation encoding is not deterministic")
			}
			count, loops, indirect := 0, 0, 0
			for node := range ast.Preorder(body) {
				count++
				switch node := node.(type) {
				case *ast.ForStmt:
					loops++
				case *ast.CallExpr:
					if _, ok := node.Fun.(*ast.IndexExpr); ok {
						indirect++
					}
				}
			}
			if loops != 1 || indirect != 1 {
				t.Errorf("seed %d size %d: want one operation loop and indexed indirect call; got %d, %d", seed, size, loops, indirect)
			}
			refs := 0
			for _, key := range keys {
				refs += key.refs
			}
			if refs == 0 {
				t.Fatal("missing external key dependencies")
			}
			counts = append(counts, count)
		}
		if delta := counts[1] - counts[0]; delta > 200 {
			t.Errorf("seed %d: AST scales with bytes: %v (delta %d)", seed, counts, delta)
		}
		t.Logf("seed %d: AST nodes at sizes 8 and 256: %v", seed, counts)
	}
}

func TestShuffleOperationRoundTrip(t *testing.T) {
	testDecoderRoundTrip(t, shuffle{}, []int{1, 8, 17, 255, 256, 257, MaxSize})
}

func TestShuffleOperationForms(t *testing.T) {
	const marker = "operation_stream_SECRET_194"
	var array [len(marker)]byte
	copy(array[:], marker)
	text := fmt.Sprintf(`package main
var str = %q
var slice = %#v
var ptr = &%#v
var array = %#v
type named []byte
func main() {
 converted := named(%q)
 if len(slice) != cap(slice) || len(*ptr) != cap(*ptr) || len(converted) != cap(converted) { panic("capacity") }
 println(str, string(slice), string(*ptr), string(array[:]), string(converted))
}`, marker, []byte(marker), []byte(marker), array, marker)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", text, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	previous := testPkgToObfuscatorMap
	testPkgToObfuscatorMap = map[string]obfuscator{"main": shuffle{}}
	defer func() { testPkgToObfuscatorMap = previous }()
	Obfuscate(rand.New(rand.NewSource(123)), file, info, nil, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%d", base, r.Uint64()) })
	var source bytes.Buffer
	if err := printer.Fprint(&source, fset, file); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(source.Bytes(), []byte(marker)) {
		t.Fatal("plaintext literal survived")
	}
	buildAndRunProbe(t, source.Bytes(), marker+" "+marker+" "+marker+" "+marker+" "+marker+"\n")
}
