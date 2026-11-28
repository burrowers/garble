package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"mvdan.cc/garble/internal/literals"
)

func checkPortableLiteralSource(t *testing.T, source string, wordSize int64, extra ...string) (*token.FileSet, *ast.File, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "portable.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Types:     make(map[ast.Expr]types.TypeAndValue),
		Defs:      make(map[*ast.Ident]types.Object),
		Uses:      make(map[*ast.Ident]types.Object),
		Implicits: make(map[ast.Node]types.Object),
		Scopes:    make(map[ast.Node]*types.Scope),
	}
	files := []*ast.File{file}
	for i, source := range extra {
		file, err := parser.ParseFile(fset, fmt.Sprintf("platform%d.go", i), source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	conf := types.Config{Importer: importer.Default(), Sizes: &types.StdSizes{WordSize: wordSize, MaxAlign: wordSize}}
	if _, err := conf.Check("main", fset, files, info); err != nil {
		t.Fatal(err)
	}
	return fset, file, info
}

func obfuscatePortableLiteralSource(t *testing.T, source string, wordSize int64, extra ...string) string {
	t.Helper()
	fset, file, info := checkPortableLiteralSource(t, source, wordSize, extra...)
	next := 0
	literals.Obfuscate(rand.New(rand.NewSource(7)), file, portableExportInfo(file, info), nil, nil, nil, func(_ *rand.Rand, _ string) string {
		next++
		return fmt.Sprintf("portable%d", next)
	})
	var out strings.Builder
	if err := format.Node(&out, fset, file); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestPortableExportLiteralContexts(t *testing.T) {
	const source = `package main
import "runtime"
import "unsafe"
type Named string
const named Named = "named constant text"
const local = "local target text"
type Tagged struct { X string ` + "`preserved struct tag`" + ` }
func main() {
	_ = "portable raw prefix/" + runtime.GOOS + "/" + runtime.GOARCH
	_ = "portable local prefix/" + local
	var a Named = "named raw prefix/" + named
	var b Named = named + "named raw suffix/"
	_ = Named("named converted prefix/" + runtime.GOOS)
	const untouched = "untouched const/"
	var array [len("unevaluated length")]byte
	_ = [32]byte{len("unevaluated index"): 1}
	_ = unsafe.Sizeof("unevaluated sizeof")
	_ = "ordinary literal " + "concatenation"
	_, _, _ = a, b, array
}`
	output := obfuscatePortableLiteralSource(t, source, 8)
	for _, text := range []string{"portable raw prefix/", "portable local prefix/", "ordinary literal ", "concatenation"} {
		if strings.Contains(output, text) {
			t.Errorf("eligible literal %q was not obfuscated", text)
		}
	}
	for _, text := range []string{"runtime.GOOS", "runtime.GOARCH", "named raw prefix/", "named raw suffix/", "untouched const/", "preserved struct tag", "unevaluated length", "unevaluated index", "unevaluated sizeof"} {
		if !strings.Contains(output, text) {
			t.Errorf("constant context or reference %q was lost", text)
		}
	}
	for _, wordSize := range []int64{4, 8} {
		checkPortableLiteralSource(t, output, wordSize)
	}
}

func TestPortableExportCompositeConstants(t *testing.T) {
	const source = `package main
import "unsafe"
import "strconv"
func main() {
	_ = []byte{platformByte, 2, 3, 4, 5, 6, 7, 8}
	_ = &[]byte{byte(unsafe.Sizeof(uintptr(0))), 2, 3, 4, 5, 6, 7, 8}
	_ = []byte{strconv.IntSize, 2, 3, 4, 5, 6, 7, 8}
	_ = [unsafe.Sizeof(uintptr(0))*2]byte{1, 2, 3, 4, 5, 6, 7, 8}
	_ = &[platformLen]byte{1, 2, 3, 4, 5, 6, 7, 8}
	_ = "target prefix/" + platformString
}`
	platformSource := func(wordSize int64) string {
		return fmt.Sprintf("package main; const platformByte byte = %d; const platformLen = %d; const platformString = %q", wordSize, wordSize+8, fmt.Sprintf("target%d", wordSize))
	}
	var previous string
	for _, wordSize := range []int64{4, 8} {
		extra := platformSource(wordSize)
		output := obfuscatePortableLiteralSource(t, source, wordSize, extra)
		for _, text := range []string{"platformByte", "unsafe.Sizeof(uintptr(0))", "strconv.IntSize", "[unsafe.Sizeof(uintptr(0)) * 2]byte", "[platformLen]byte", "platformString"} {
			if !strings.Contains(output, text) {
				t.Errorf("word size %d: target-dependent expression %q was baked", wordSize, text)
			}
		}
		if previous != "" && output != previous {
			t.Error("different target constants produced different emitted source")
		}
		previous = output
		for _, targetSize := range []int64{4, 8} {
			checkPortableLiteralSource(t, output, targetSize, platformSource(targetSize))
		}
	}
}

func TestPortableExportInferredArrays(t *testing.T) {
	const source = `package main
import "strconv"
type Named [platformIndex+1]string
var inferred = [...]string{platformIndex: "array secret payload"}
var parenthesized = ([...]string{platformIndex: "parenthesized array payload"})
var imported = [...]string{strconv.IntSize: "imported array payload"}
var explicit = [platformIndex+1]string{platformIndex: "explicit array payload"}
var named = Named{platformIndex: "named array payload"}
func main() {}`
	platformSource := func(wordSize int64) string {
		return fmt.Sprintf("package main; const platformIndex = %d", wordSize)
	}
	var previous string
	for _, wordSize := range []int64{4, 8} {
		output := obfuscatePortableLiteralSource(t, source, wordSize, platformSource(wordSize))
		for _, text := range []string{"[...]string{platformIndex:", "[...]string{strconv.IntSize:", "[platformIndex + 1]string", "Named{"} {
			if !strings.Contains(output, text) {
				t.Errorf("word size %d: target-dependent array expression %q was lost", wordSize, text)
			}
		}
		if previous != "" && output != previous {
			t.Error("different target array keys produced different emitted source")
		}
		previous = output
		for _, targetSize := range []int64{4, 8} {
			checkPortableLiteralSource(t, output, targetSize, platformSource(targetSize))
		}
	}
}

func TestPortableExportConstantConversions(t *testing.T) {
	const source = `package main
import "runtime"
type Bytes []byte
type Runes []rune
type Named string
const named Named = "named platform text"
func values() {
	_ = []byte("portable prefix/" + runtime.GOOS)
	_ = []rune("portable prefix/" + runtime.GOOS)
	_ = Bytes("portable prefix/" + runtime.GOOS)
	_ = Runes("portable prefix/" + runtime.GOOS)
	_ = Bytes("dependent named prefix/" + named)
	_ = Runes(string(len("unevaluated conversion secret")) + runtime.GOOS)
	_ = Bytes("ordinary byte conversion secret")
	_ = Runes("ordinary rune conversion αβγ secret")
}`
	for _, wordSize := range []int64{4, 8} {
		output := obfuscatePortableLiteralSource(t, source, wordSize)
		for _, expr := range []string{
			`[]byte("portable prefix/" + runtime.GOOS)`,
			`[]rune("portable prefix/" + runtime.GOOS)`,
			`Bytes("portable prefix/" + runtime.GOOS)`,
			`Runes("portable prefix/" + runtime.GOOS)`,
			`Bytes("dependent named prefix/" + named)`,
			`Runes(string(len("unevaluated conversion secret")) + runtime.GOOS)`,
		} {
			if !strings.Contains(output, expr) {
				t.Errorf("word size %d: dependent constant conversion %s was changed", wordSize, expr)
			}
		}
		for _, text := range []string{"ordinary byte conversion secret", "ordinary rune conversion αβγ secret"} {
			if strings.Contains(output, text) {
				t.Errorf("ordinary conversion %q was not obfuscated", text)
			}
		}
		for _, targetSize := range []int64{4, 8} {
			checkPortableLiteralSource(t, output, targetSize)
		}
	}
}

func TestPortableExportInfo(t *testing.T) {
	const source = `package main
import "runtime"
import "unsafe"
const local = "local platform constant"
const platformIndex = len(runtime.GOOS)
var inferred = [...]string{platformIndex: "inferred array payload"}
func values() {
	_ = []byte("dependent prefix/" + runtime.GOOS)
	_ = []rune("dependent prefix/" + runtime.GOOS)
	_ = "opaque prefix/" + runtime.GOOS + runtime.GOARCH
	_ = local
	_ = "literal-only " + "constant"
	_ = string(unsafe.Sizeof(uintptr(0)) + 64)
	_ = len("unevaluated secret")
}`
	for _, wordSize := range []int64{4, 8} {
		t.Run(fmt.Sprint(wordSize), func(t *testing.T) {
			_, file, info := checkPortableLiteralSource(t, source, wordSize)
			before := *info
			before.Types = maps.Clone(info.Types)
			portable := portableExportInfo(file, info)
			if portable == info {
				t.Fatal("adapter returned the original Info")
			}
			if !reflect.DeepEqual(info, &before) {
				t.Fatal("adapter mutated original type information")
			}
			withoutTypes := *portable
			withoutTypes.Types = before.Types
			if !reflect.DeepEqual(&withoutTypes, &before) {
				t.Fatal("adapter changed non-Types information")
			}
			for node := range ast.Preorder(file) {
				expr, ok := node.(ast.Expr)
				if !ok {
					continue
				}
				switch expr := expr.(type) {
				case *ast.BinaryExpr:
					_, literal := expr.X.(*ast.BasicLit)
					_, literalRight := expr.Y.(*ast.BasicLit)
					wantValue := info.Types[expr].Value != nil && (info.Types[expr].Type != types.Typ[types.String] || literal && literalRight)
					if got := portable.Types[expr].Value != nil; got != wantValue {
						t.Errorf("binary at %v: has constant value %v, want %v", expr.Pos(), got, wantValue)
					}
				case *ast.SelectorExpr:
					if portable.Types[expr].Value != nil {
						t.Errorf("selector %s still has a constant value", expr.Sel.Name)
					}
				case *ast.CallExpr:
					if value := info.Types[expr]; value.Value != nil {
						wantValue := value.Type != types.Typ[types.String]
						if got := portable.Types[expr].Value != nil; got != wantValue {
							t.Errorf("call at %v: has constant value %v, want %v", expr.Pos(), got, wantValue)
						}
					}
				case *ast.Ident:
					if expr.Name == "local" && info.Uses[expr] != nil && portable.Types[expr].Value != nil {
						t.Error("local constant reference still has a constant value")
					}
				}
			}
			for expr := range portable.Types {
				delete(portable.Types, expr)
				break
			}
			if !reflect.DeepEqual(info, &before) {
				t.Fatal("Types map aliases original")
			}
		})
	}
}
