package ctrlflow

import (
	"bytes"
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

	"golang.org/x/tools/go/ssa/ssautil"
)

func renderDecision(t *testing.T, source string, seed int64) string {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "main.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("test/main", "main"), []*ast.File{f}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, generated, _, err := Obfuscate(fs, p, []*ast.File{f}, rand.New(rand.NewSource(seed)))
	if err != nil {
		t.Fatal(err)
	}
	f.Decls = append(f.Decls, generated.Decls...)
	f.Comments = nil
	var b bytes.Buffer
	if err := printer.Fprint(&b, fs, f); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const decisionSource = `package main
//garble:controlflow flatten_passes=0 decision_lowering=1
func compute(n uint32) uint32 {switch n {case 0:return 4;case 1:return n+7;case 2:return n+11;case 6:return 9;case 17:return n*2;default:return n*3}}
func reference(n uint32) uint32 {switch n {case 0:return 4;case 1:return n+7;case 2:return n+11;case 6:return 9;case 17:return n*2;default:return n*3}}
func main(){for _,n:=range []uint32{0,1,2,3,5,6,7,16,17,18,255,65535,2147483648,4294967295}{if compute(n)!=reference(n){panic(n)}}}
`

func TestDecisionExecution(t *testing.T) {
	variants := make(map[string]bool)
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34} {
		got := renderDecision(t, decisionSource, seed)
		variants[got] = true
		if got != renderDecision(t, decisionSource, seed) {
			t.Fatal("nondeterministic")
		}
		path := filepath.Join(t.TempDir(), "main.go")
		if err := os.WriteFile(path, []byte(got), 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("go", "run", path).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s\n%s", err, out, got)
		}
	}
	if len(variants) < 2 {
		t.Fatal("seed diversity missing")
	}
}

func TestDecisionExclusions(t *testing.T) {
	bodies := []string{
		"switch n {case n+1:return 1;case 2:return 3;case 4:return 5;default:return 9}",
		"switch n {case 0:n++;fallthrough;case 2:return n;case 4:return 5;default:return 9}",
		"a:=n;switch n {case 0:a++;case 2:a+=2;case 4:a+=3};return a",
	}
	for _, body := range bodies {
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, "test.go", "package test\nfunc compute(n uint32) uint32 {"+body+"}", 0)
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("test", "test"), []*ast.File{f}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if lowerDecision(p.Func("compute"), rand.New(rand.NewSource(1))) {
			t.Fatal("unsupported decision lowered")
		}
	}
	plain := strings.Replace(decisionSource, " decision_lowering=1", "", 1)
	if renderDecision(t, plain, 1) != strings.ReplaceAll(renderDecision(t, strings.Replace(plain, "flatten_passes=0", "flatten_passes=0 decision_lowering=0", 1), 1), " decision_lowering=0", "") {
		t.Fatal("disabled source changed")
	}
}

func TestDecisionCompilerProbe(t *testing.T) {
	output := os.Getenv("CF_DECISION_OUTPUT")
	if output == "" {
		t.Skip("set CF_DECISION_OUTPUT for optimized assembly")
	}
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34} {
		got := renderDecision(t, decisionSource, seed)
		got = strings.Replace(got, "func compute(", "//go:noinline\nfunc compute(", 1)
		dir := filepath.Join(output, strconv.FormatInt(seed, 10))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "main.go")
		if err := os.WriteFile(path, []byte(got), 0600); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(dir, "probe")
		if out, err := exec.Command("go", "build", "-o", binary, path).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if out, err := exec.Command(binary).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		out, err := exec.Command("go", "tool", "objdump", "-s", "^main.compute$", binary).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "assembly.txt"), out, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
