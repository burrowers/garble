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

func TestStateTransitions(t *testing.T) {
	const src = `package main
//garble:controlflow flatten_passes=1 state_transitions=1
func compute(n int) int { sum:=0; for i:=0; i<n; i++ { if i%2==0 { sum+=i } else { sum-=i } }; return sum }
func main() { for n:=0; n<40; n++ { expected:=0; for i:=0; i<n; i++ { if i%2==0 { expected+=i } else { expected-=i } }; if compute(n)!=expected { panic("state transition") } } }
`
	for _, seed := range []int64{1, 2, 3} {
		got := renderExperiment(t, src, seed)
		if !strings.Contains(got, "^") {
			t.Fatal("state-dependent XOR missing")
		}
		if got != renderExperiment(t, src, seed) {
			t.Fatal("same seed changed source")
		}
		runExperiment(t, got)
	}
}
