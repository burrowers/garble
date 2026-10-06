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
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa/ssautil"
)

func renderOutline(t *testing.T, source string, seed int64) string {
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

func TestOutliningExecution(t *testing.T) {
	const source = `package main
//garble:controlflow flatten_passes=0 outline_ops=1 outline_noinline=1
func compute(n uint32) uint32 { a:=n+7; b:=a^0x13579; return (b*3)-n }
func reference(n uint32) uint32 { a:=n+7; b:=a^0x13579; return (b*3)-n }
func main(){for _,n:=range []uint32{0,1,7,255,65535,2147483648,4294967295}{if compute(n)!=reference(n){panic(n)}}}
`
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34} {
		got := renderOutline(t, source, seed)
		if !strings.Contains(got, "_garble_outline_") || !strings.Contains(got, "//go:noinline") {
			t.Fatal("helper or noinline absent")
		}
		if got != renderOutline(t, source, seed) {
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
}

func TestOutliningDisabledAndExclusions(t *testing.T) {
	plain := `package main
//garble:controlflow flatten_passes=0
func compute(n uint32) uint32 { return (n+1)*3 }
func main(){}
`
	if renderOutline(t, plain, 1) != strings.ReplaceAll(renderOutline(t, strings.Replace(plain, "flatten_passes=0", "flatten_passes=0 outline_ops=0", 1), 1), " outline_ops=0", "") {
		t.Fatal("disabled source changed")
	}
	for _, body := range []string{
		"if n==0{return 1};return n+1",
		"defer func(){}();return n+1",
		"p:=new(uint32);*p=n;return *p+1",
		"return n<<1",
	} {
		source := "package main\n//garble:controlflow flatten_passes=0 outline_ops=1\nfunc compute(n uint32) uint32 {" + body + "}\nfunc main(){}"
		if strings.Contains(renderOutline(t, source, 1), "_garble_outline_") {
			t.Fatal("unsupported function outlined")
		}
	}
}

func TestOutliningWidthsAndComposition(t *testing.T) {
	for _, typ := range []string{"uint8", "uint16", "uint32", "uint64"} {
		source := "package main\n//garble:controlflow flatten_passes=0 outline_ops=1 outline_noinline=1\nfunc compute(n " + typ + ") " + typ + " {return ((n+7)^17)*3-n}\nfunc reference(n " + typ + ") " + typ + " {return ((n+7)^17)*3-n}\nfunc _garble_outline_0(){}\nfunc main(){for i:=0;i<256;i++{n:=" + typ + "(i);if compute(n)!=reference(n){panic(i)}}}"
		got := renderOutline(t, source, 1)
		if !strings.Contains(got, "func _garble_outline_0_(") {
			t.Fatal("helper name collision")
		}
		path := filepath.Join(t.TempDir(), "main.go")
		if err := os.WriteFile(path, []byte(got), 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("go", "run", path).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	for _, flags := range []string{"flatten_passes=1", "flatten_passes=0 trash_blocks=1", "flatten_passes=0 block_splits=1", "flatten_passes=0 junk_jumps=1", "flatten_passes=0 flatten_hardening=xor"} {
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, "main.go", "package main\n//garble:controlflow outline_ops=1 "+flags+"\nfunc compute(n uint32) uint32 {return n+1}", parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("test/main", "main"), []*ast.File{f}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := Obfuscate(fs, p, []*ast.File{f}, rand.New(rand.NewSource(1))); err == nil {
			t.Fatal("composition accepted")
		}
	}
}

func TestOutliningCompilerProbe(t *testing.T) {
	output := os.Getenv("CF_OUTLINE_OUTPUT")
	if output == "" {
		t.Skip("set CF_OUTLINE_OUTPUT for optimized assembly")
	}
	for _, mode := range []string{"", " outline_ops=1", " outline_ops=1 outline_noinline=1"} {
		for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34} {
			source := "package main\n//garble:controlflow flatten_passes=0" + mode + "\nfunc compute(n uint32) uint32 { a:=n+7; b:=a^0x13579; return (b*3)-n }\nfunc main(){println(compute(7))}"
			got := renderOutline(t, source, seed)
			got = strings.Replace(got, "func compute(", "//go:noinline\nfunc compute(", 1)
			dir := filepath.Join(output, strings.ReplaceAll(mode, " ", "_")+"-"+string(rune('A'+seed)))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "main.go")
			if err := os.WriteFile(path, []byte(got), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "probe")
			if out, err := exec.Command("go", "build", "-o", binary, path).CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			out, err := exec.Command("go", "tool", "objdump", "-s", "^main.(compute|_garble_outline_0)$", binary).CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "assembly.txt"), out, 0600); err != nil {
				t.Fatal(err)
			}
			out, err = exec.Command(binary).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "237662" {
				t.Fatalf("unexpected execution %v %s", err, out)
			}
		}
	}
}
