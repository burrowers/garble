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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ah "mvdan.cc/garble/internal/asthelper"
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
	var source bytes.Buffer
	source.WriteString("package main\nfunc main() {\n")
	for _, seed := range []int64{1, 2, 3} {
		for _, size := range []int{1, 8, 17, 255, 256, 257, MaxSize} {
			body, names, keys := shuffleProbe(t, seed, size)
			body.List = append(body.List, ah.ReturnStmt(names.ident("data")))
			params := &ast.FieldList{}
			var args []ast.Expr
			for _, key := range keys {
				params.List = append(params.List, ah.Field(key.Type(), key.Name()))
				args = append(args, ah.UintLit(key.value))
			}
			source.WriteString("{ decode := func() []byte { return ")
			if err := printer.Fprint(&source, token.NewFileSet(), ah.LambdaCall(params, ah.ByteSliceType(), body, args)); err != nil {
				t.Fatal(err)
			}
			source.WriteString(" }; got := decode(); ")
			fmt.Fprintf(&source, "if len(got) != %d || cap(got) != %d { panic(\"capacity\") }; ", size, size)
			source.WriteString("for i, b := range got { if b != byte(i) { panic(\"contents\") } }; got[0] = 42; fresh := decode(); if got[0] != 42 || fresh[0] != 0 { panic(\"aliasing\") } }\n")
		}
	}
	source.WriteString("}\n")
	compileShuffleProbe(t, source.Bytes(), "")
}

func compileShuffleProbe(t *testing.T, source []byte, want string) {
	t.Helper()
	dir := t.TempDir()
	src, bin := filepath.Join(dir, "main.go"), filepath.Join(dir, "probe.exe")
	if err := os.WriteFile(src, source, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin).CombinedOutput(); err != nil || string(out) != want {
		t.Fatalf("run: %v\n%s", err, out)
	}
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
	Obfuscate(rand.New(rand.NewSource(123)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("hidden_%s_%d", base, r.Uint64()) })
	var source bytes.Buffer
	if err := printer.Fprint(&source, fset, file); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(source.Bytes(), []byte(marker)) {
		t.Fatal("plaintext literal survived")
	}
	compileShuffleProbe(t, source.Bytes(), marker+" "+marker+" "+marker+" "+marker+" "+marker+"\n")
}
