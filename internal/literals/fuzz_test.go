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
}
`[1:]

func FuzzObfuscate(f *testing.F) {
	initialRandSeed := int64(123)
	f.Add("", initialRandSeed)
	f.Add("short", initialRandSeed)
	f.Add("long_enough_string", initialRandSeed)
	f.Add("binary_\x00\x01\x02", initialRandSeed)
	f.Add("whitespace    \n\t\t", initialRandSeed)
	f.Add(strings.Repeat("x", (2<<10)+1), initialRandSeed) // past maxSize

	tdir := f.TempDir()
	var tdirCounter atomic.Int64
	f.Fuzz(func(t *testing.T, in string, randSeed int64) {
		// The code below is an extreme simplification of what "garble build" does,
		// but it does significantly less, allowing the fuzz function to be faster.
		// For example, we only obfuscate the literals, not any identifiers.
		// Note that the fuzzer is still quite slow, as it still builds a binary.

		// Create the source, parse it, and typecheck it.
		srcText := fmt.Sprintf(fuzzTemplate, in, []byte(in))
		t.Log(srcText) // shown on failures
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
		srcSyntax = literals.Obfuscate(rand, srcSyntax, &info, nil, func(rand *mathrand.Rand, baseName string) string {
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

		// Run the binary, expecting the output to match.
		out, err := exec.Command(binPath).CombinedOutput()
		qt.Assert(t, qt.IsNil(err))
		want := fmt.Sprintf("%[1]s\nx%[1]sy\n--\n%[1]s\n%[1]s\n", in)
		qt.Assert(t, qt.Equals(string(out), want))
	})
}
