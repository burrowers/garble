package ctrlflow

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/ssa/ssautil"
)

func renderExperiment(t *testing.T, src string, seed int64) string {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "main.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("test/main", "main"), []*ast.File{f}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, generated, _, err := Obfuscate(fs, p, []*ast.File{f}, mathrand.New(mathrand.NewSource(seed)))
	if err != nil {
		t.Fatal(err)
	}
	f.Decls = append(f.Decls, generated.Decls...)
	f.Comments = nil
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fs, f); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func runExperiment(t *testing.T, src string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run: %v\n%s\n%s", err, out, src)
	}
}

func TestLiveIntegers(t *testing.T) {
	const src = `package main
//garble:controlflow flatten_passes=0 live_integers=1
func add(x, y uint8) uint8 { return x+y }
func main() { for x:=0; x<256; x++ { for y:=0; y<256; y++ { if add(uint8(x),uint8(y)) != uint8(x+y) { panic("addition") } } } }
`
	for _, seed := range []int64{1, 2, 3} {
		got := renderExperiment(t, src, seed)
		if !strings.Contains(got, "^") {
			t.Fatal("live addition was not substituted")
		}
		if got != renderExperiment(t, src, seed) {
			t.Fatal("same seed changed source")
		}
		runExperiment(t, got)
	}
}
