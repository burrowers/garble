package literals

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ah "mvdan.cc/garble/internal/asthelper"
)

// decoderProbeSizes covers a one-byte literal, a size that forces several
// record chunks, and the largest size the per-byte obfuscators accept.
var decoderProbeSizes = []int{1, 17, 257}

func testDecoderRoundTrip(t *testing.T, obf obfuscator, sizes []int) {
	t.Helper()
	var source bytes.Buffer
	source.WriteString("package main\nfunc main() {\n")
	for _, randSeed := range []int64{1, 2, 3} {
		for _, size := range sizes {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i)
			}
			r := newObfRand(rand.New(rand.NewSource(randSeed)), &ast.File{Name: ast.NewIdent("main")}, func(_ *rand.Rand, name string) string { return "hidden_" + name })
			names := newGeneratedNames(r)
			keys := randExtKeys(r.rnd, names)
			body := obf.obfuscate(r.rnd, names, data, keys)
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
			source.WriteString("for i, b := range got { if b != byte(i) { panic(\"contents\") } }; ")
			source.WriteString("ptr := &got; (*ptr)[0] = 42; fresh := decode(); if got[0] != 42 || fresh[0] != 0 { panic(\"aliasing\") } }\n")
		}
	}
	source.WriteString("}\n")
	buildAndRunProbe(t, source.Bytes(), "")
}

// buildAndRunProbe compiles a probe program and runs it, failing on a build
// error or on output other than want.
func buildAndRunProbe(t *testing.T, source []byte, want string) {
	t.Helper()
	dir := t.TempDir()
	srcPath, binPath := filepath.Join(dir, "main.go"), filepath.Join(dir, "probe.exe")
	if err := os.WriteFile(srcPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-p=1", "-o", binPath, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if out, err := exec.Command(binPath).CombinedOutput(); err != nil || string(out) != want {
		t.Fatalf("run: %v\n%s", err, out)
	}
}
