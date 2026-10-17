package literals_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quicktest/qt"
	"mvdan.cc/garble/internal/literals"
)

// The fuzzing string is passed in as a string and []byte literal.
var fuzzTemplate = `
package main

var str string = %#[1]v
var strFold string = "x" + %#[1]v + "y"
var byt []byte = %#[2]v
var bytPtr *[]byte = &%#[2]v

func main() {
	println(str)
	println(strFold)
	println("--")
	println(string(byt))
	println(string(*bytPtr))
	// The decoded slices must have the same cap as the original literals.
	println(len(byt), cap(byt))
	println(len(*bytPtr), cap(*bytPtr))
}
`[1:]

func TestObfuscateDeadFunctionDecoders(t *testing.T) {
	const source = `package main
func main() { println("live literal secret"); _ = "live literal secret" }
func dead() string { return func() string { return "live literal secret" }() }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "owner.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	dead := file.Decls[1].(*ast.FuncDecl)
	originalCount := len(file.Decls)
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("main", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	literals.Obfuscate(mathrand.New(mathrand.NewSource(1)), file, info, nil, nil, func(r *mathrand.Rand, base string) string {
		return fmt.Sprintf("%s%d", base, r.Uint64())
	})
	generated := make(map[string]ast.Node)
	for _, decl := range file.Decls[originalCount:] {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			generated[decl.Name.Name] = decl
		case *ast.GenDecl:
			if decl.Tok == token.VAR {
				for _, spec := range decl.Specs {
					spec := spec.(*ast.ValueSpec)
					generated[spec.Names[0].Name] = spec
				}
			}
		}
	}
	seen := make(map[string]bool)
	var decoders []string
	var roots []string
	var visit func(ast.Node)
	visit = func(node ast.Node) {
		ast.Inspect(node, func(node ast.Node) bool {
			if id, ok := node.(*ast.Ident); ok && !seen[id.Name] {
				if decl := generated[id.Name]; decl != nil {
					seen[id.Name] = true
					if _, ok := decl.(*ast.FuncDecl); ok {
						decoders = append(decoders, id.Name)
					} else {
						roots = append(roots, id.Name)
					}
					visit(decl)
				}
			}
			return true
		})
	}
	visit(dead.Body)
	if len(decoders) == 0 {
		t.Fatal("dead function has no generated decoders")
	}
	var text strings.Builder
	if err := printer.Fprint(&text, fset, file); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srcPath, binPath := filepath.Join(dir, "main.go"), filepath.Join(dir, "main.exe")
	if err := os.WriteFile(srcPath, []byte(text.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-trimpath", "-o", binPath, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if out, err := exec.Command(binPath).CombinedOutput(); err != nil || string(out) != "live literal secret\n" {
		t.Fatalf("run: %v\n%s", err, out)
	}
	out, err := exec.Command("go", "tool", "nm", binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("nm: %v\n%s", err, out)
	}
	for _, name := range append(decoders, roots...) {
		if strings.Contains(string(out), "main."+name) {
			t.Errorf("dead owner's generated declaration %s retained in binary", name)
		}
	}
}

func FuzzObfuscate(f *testing.F) {
	initialRandSeed := int64(123)
	f.Add("", initialRandSeed)
	f.Add("short", initialRandSeed)
	f.Add("long_enough_string", initialRandSeed)
	f.Add("binary_\x00\x01\x02", initialRandSeed)
	f.Add("whitespace    \n\t\t", initialRandSeed)
	f.Add(strings.Repeat("x", (2<<10)+1), initialRandSeed) // past MaxSize
	f.Add(strings.Repeat("large_\x00\xff_SECRET_194", 4096), initialRandSeed)

	tdir := f.TempDir()
	var tdirCounter atomic.Int64
	f.Fuzz(func(t *testing.T, in string, randSeed int64) {
		// The code below is an extreme simplification of what "garble build" does,
		// but it does significantly less, allowing the fuzz function to be faster.
		// For example, we only obfuscate the literals, not any identifiers.
		// Note that the fuzzer is still quite slow, as it still builds a binary.

		// Create the source, parse it, and typecheck it.
		srcText := fmt.Sprintf(fuzzTemplate, in, []byte(in))
		t.Logf("input length %d, random seed %d", len(in), randSeed)
		fset := token.NewFileSet()
		srcSyntax, err := parser.ParseFile(fset, "", srcText, parser.SkipObjectResolution)
		qt.Assert(t, qt.IsNil(err))
		info := types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		}
		var conf types.Config
		_, err = conf.Check("p", fset, []*ast.File{srcSyntax}, &info)
		qt.Assert(t, qt.IsNil(err))

		// Obfuscate the literals and print the source back.
		rand := mathrand.New(mathrand.NewSource(randSeed))
		srcSyntax = literals.Obfuscate(rand, srcSyntax, &info, nil, nil, func(rand *mathrand.Rand, baseName string) string {
			return fmt.Sprintf("%s%d", baseName, rand.Uint64())
		})
		count := tdirCounter.Add(1)
		f, err := os.Create(filepath.Join(tdir, fmt.Sprintf("src_%d.go", count)))
		qt.Assert(t, qt.IsNil(err))
		srcPath := f.Name()
		t.Cleanup(func() {
			f.Close()
			os.Remove(srcPath)
		})
		err = printer.Fprint(f, fset, srcSyntax)
		qt.Assert(t, qt.IsNil(err))

		// Build the main package. Use some flags to avoid work.
		binPath := strings.TrimSuffix(srcPath, ".go")
		if runtime.GOOS == "windows" {
			binPath += ".exe"
		}
		if out, err := exec.Command(
			"go", "build", "-trimpath", "-ldflags=-w -s", "-p", "1",
			"-o", binPath, srcPath,
		).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		t.Cleanup(func() { os.Remove(binPath) })

		// Runtime println does not retry short writes to pipes after signals.
		outFile, err := os.CreateTemp(t.TempDir(), "output")
		qt.Assert(t, qt.IsNil(err))
		t.Cleanup(func() { outFile.Close() })
		cmd := exec.Command(binPath)
		cmd.Stdout, cmd.Stderr = outFile, outFile
		qt.Assert(t, qt.IsNil(cmd.Run()))
		qt.Assert(t, qt.IsNil(outFile.Close()))
		out, err := os.ReadFile(outFile.Name())
		qt.Assert(t, qt.IsNil(err))
		want := fmt.Sprintf("%[1]s\nx%[1]sy\n--\n%[1]s\n%[1]s\n%[2]d %[2]d\n%[2]d %[2]d\n", in, len(in))
		qt.Assert(t, qt.Equals(string(out), want))
	})
}
