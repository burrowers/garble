// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
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

func TestOwnerSharedStringAdapter(t *testing.T) {
	for _, obf := range append(append([]obfuscator{}, Obfuscators...), blob{}) {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			r := newObfRand(rand.New(rand.NewSource(7)), &ast.File{Name: ast.NewIdent("main")}, func(r *rand.Rand, base string) string { return fmt.Sprintf("%s%d", base, r.Uint64()) })
			r.testObfuscator = obf
			seen := make(map[ast.Node]bool)
			texts := []string{"first literal", "a longer second literal", "third literal", "fourth literal", "first literal", "longer sixth literal", "seventh literal", "eighth literal"}
			for i, text := range texts {
				call := obfuscateString(r, text)
				for node := range ast.Preorder(call) {
					if seen[node] {
						t.Fatalf("literal calls share AST node %T", node)
					}
					seen[node] = true
				}
				withPos(call, token.Pos(100+i))
			}
			helpers, decoders := 0, 0
			lows := make(map[int]bool)
			for _, decl := range r.liftedFuncs {
				fn := decl.(*ast.FuncDecl)
				if strings.HasPrefix(fn.Name.Name, "literalDecoder") {
					call := fn.Body.List[len(fn.Body.List)-1].(*ast.ReturnStmt).Results[0].(*ast.CallExpr)
					if len(call.Args) != 3 {
						t.Fatalf("conversion has %d arguments, want slice and two bounds", len(call.Args))
					}
					low, _ := strconv.Atoi(call.Args[1].(*ast.BasicLit).Value)
					high, _ := strconv.Atoi(call.Args[2].(*ast.BasicLit).Value)
					if low < 0 || low >= maxStringJunkBytes || high-low != len(texts[decoders]) {
						t.Errorf("decoder %d bounds [%d:%d] do not preserve its literal", decoders, low, high)
					}
					lows[low] = true
					decoders++
				}
				if strings.HasPrefix(fn.Name.Name, "literalHelper") {
					if fn.Pos() != token.NoPos {
						t.Error("adapter acquired an original source position")
					}
					helpers++
					if fn.Type.Params.NumFields() != 3 {
						t.Errorf("adapter has %d parameters, want slice and two bounds", fn.Type.Params.NumFields())
					}
				}
			}
			if len(lows) < 2 {
				t.Error("fixture did not exercise differing junk bounds")
			}
			if helpers != 1 {
				t.Errorf("owner has %d string adapters, want 1", helpers)
			}
		})
	}
}

func TestSharedStringAdapterBehavior(t *testing.T) {
	const source = `package main
 type text string
 type bytes []byte
 type runes []rune
 var global = "global literal secret"
 func values() (string, string, text, bytes, runes, []byte) {
  return "first literal", "second longer literal", text("named literal"), bytes("mutable bytes literal"), runes("日本語のliteral"), []byte{1,2,3,4,5,6,7,8,9}
 }
 func main() {
  for i := 0; i < 3; i++ {
   a,b,c,d,e,f := values()
   plainBytes, plainRunes := []byte("plain bytes literal"), []rune("日本語のliteral")
   if len(plainBytes) != 19 || cap(plainBytes) != 19 || len(plainRunes) != 11 || cap(plainRunes) != 11 { panic(3) }
   if a != "first literal" || b != "second longer literal" || c != text("named literal") || string(d) != "mutable bytes literal" || string(e) != "日本語のliteral" || global != "global literal secret" { panic(1) }
   if len(d) != 21 || cap(d) != 21 || len(e) != 11 || cap(e) != 11 || len(f) != 9 || cap(f) != 9 || f[0] != 1 { panic(2) }
   println(a, b, c, string(d), string(e), global)
   d[0] = 0; e[0] = 0; f[0] = 0
  }
 }
 `
	for _, obf := range append(append([]obfuscator{}, Obfuscators...), blob{}) {
		t.Run(fmt.Sprintf("%T", obf), func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "adapter.go", source, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
			if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			previous := testPkgToObfuscatorMap
			testPkgToObfuscatorMap = map[string]obfuscator{"main": obf}
			defer func() { testPkgToObfuscatorMap = previous }()
			Obfuscate(rand.New(rand.NewSource(7)), file, info, nil, nil, func(r *rand.Rand, base string) string { return fmt.Sprintf("%s%d", base, r.Uint64()) })
			if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, nil); err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			if err := printer.Fprint(&text, fset, file); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			src, bin := filepath.Join(dir, "main.go"), filepath.Join(dir, "main.exe")
			if err := os.WriteFile(src, []byte(text.String()), 0600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			want := strings.Repeat("first literal second longer literal named literal mutable bytes literal 日本語のliteral global literal secret\n", 3)
			if out, err := exec.Command(bin).CombinedOutput(); err != nil || string(out) != want {
				t.Fatalf("run: %v\n%s", err, out)
			}
		})
	}
}
