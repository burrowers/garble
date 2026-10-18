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
	"strconv"
	"strings"
	"testing"
)

func TestSharedPayloadMutable(t *testing.T) {
	value := []byte(strings.Repeat("mutable_\x00\xff", 260))
	source := fmt.Sprintf(`package main
func main() {
 a, b := %#[1]v, %#[1]v
 p, q := &%#[1]v, &%#[1]v
 x, y := &[2600]byte%[2]s, &[2600]byte%[2]s
 if len(a) != 2600 || cap(a) != 2600 || cap(*p) != 2600 { panic("cap") }
 a[0] ^= 1; (*p)[0] ^= 1; x[0] ^= 1
 if b[0] != 'm' || (*q)[0] != 'm' || y[0] != 'm' { panic("alias") }
 for i := range b { if b[i] != (*q)[i] || b[i] != y[i] { panic("data") } }
}`, value, strings.TrimPrefix(fmt.Sprintf("%#v", value), "[]byte"))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "mutable.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	Obfuscate(rand.New(rand.NewSource(456)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("%s%d", base, r.Uint64()) })
	payloads, decoders := 0, 0
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			if decl.Tok == token.CONST {
				payloads += len(decl.Specs)
			}
		case *ast.FuncDecl:
			if strings.HasPrefix(decl.Name.Name, "literalDecoder") {
				decoders++
			}
		}
	}
	if payloads != 1 || decoders != 6 {
		t.Fatalf("got %d payloads, %d decoders; want 1, 6", payloads, decoders)
	}
	runPayloadProgram(t, fset, file, true)
	// Every generated blob decoder has its own scrambled key expression. Changing
	// all external arguments must break decoding rather than remain decorative.
	main := file.Decls[0].(*ast.FuncDecl)
	for node := range ast.Preorder(main.Body) {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			continue
		}
		if _, ok := call.Fun.(*ast.SelectorExpr); !ok {
			continue
		}
		for i, arg := range call.Args {
			call.Args[i] = &ast.BinaryExpr{X: arg, Op: token.XOR, Y: &ast.BasicLit{Kind: token.INT, Value: "255"}}
		}
	}
	runPayloadProgram(t, fset, file, false)
}

func TestSharedPayload(t *testing.T) {
	value := strings.Repeat("payload_\x00\xff", 300)
	source := fmt.Sprintf(`package main
func first() string { return %q }
func second() string { return %q }
func main() {
 a, b := []byte(%q), []byte(%q)
 if len(a) != %d || cap(a) != len(a) || cap(b) != len(b) { panic("capacity") }
 a[0] ^= 1
 if a[0] == b[0] || string(b) != first() || first() != second() { panic("contents") }
 f := func() string { return %q }
 if f() != string(b) { panic("closure") }
}`, value, value, value, value, len(value), value)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "payload.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	Obfuscate(rand.New(rand.NewSource(123)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("%s%d", base, r.Uint64()) })
	payloads := make(map[string]int)
	decoders := 0
	for _, decl := range file.Decls {
		if decl, ok := decl.(*ast.GenDecl); ok && decl.Tok == token.CONST {
			for _, spec := range decl.Specs {
				payloads[spec.(*ast.ValueSpec).Names[0].Name] = 0
			}
		}
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "literalDecoder") {
			continue
		}
		decoders++
		for node := range ast.Preorder(fn.Body) {
			if id, ok := node.(*ast.Ident); ok {
				if _, ok := payloads[id.Name]; ok {
					payloads[id.Name]++
				}
			}
		}
	}
	if len(payloads) != 3 || decoders != 7 {
		t.Fatalf("got %d payloads, %d independent decoders; want 3, 7", len(payloads), decoders)
	}
	shared := 0
	for _, refs := range payloads {
		if refs == 3 {
			shared++
		} else if refs != 1 {
			t.Errorf("unexpected payload references: %d", refs)
		}
	}
	if shared != 1 {
		t.Fatalf("want one payload shared by main and its closure, got %v", payloads)
	}
	if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, nil); err != nil {
		t.Fatal(err)
	}
	runPayloadProgram(t, fset, file, true)
}

func runPayloadProgram(t *testing.T, fset *token.FileSet, file *ast.File, success bool) {
	t.Helper()
	var text strings.Builder
	if err := printer.Fprint(&text, fset, file); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "main.exe")
	if out, err := exec.Command("go", "build", "-o", binary, path).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	image, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		decl, ok := decl.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			continue
		}
		for _, spec := range decl.Specs {
			encoded, err := strconv.Unquote(spec.(*ast.ValueSpec).Values[0].(*ast.BasicLit).Value)
			if err != nil {
				t.Fatal(err)
			}
			if count := bytes.Count(image, []byte(encoded)); count != 1 {
				t.Fatalf("encoded payload has %d binary copies; want one", count)
			}
		}
	}
	if out, err := exec.Command(binary).CombinedOutput(); (err == nil) != success {
		t.Fatalf("run (want success=%v): %v\n%s", success, err, out)
	} else if !success && !strings.Contains(string(out), "panic: alias") && !strings.Contains(string(out), "panic: data") {
		t.Fatalf("key perturbation did not reach runtime content assertion: %s", out)
	}
}
