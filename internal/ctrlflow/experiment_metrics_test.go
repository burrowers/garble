package ctrlflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This opt-in probe keeps compiler measurements out of the normal test path.
func TestExperimentMetrics(t *testing.T) {
	directive := os.Getenv("CF_EXPERIMENT_DIRECTIVE")
	if directive == "" {
		t.Skip("set CF_EXPERIMENT_DIRECTIVE to run the compiler probe")
	}
	dir := os.Getenv("CF_EXPERIMENT_OUTPUT")
	if dir == "" {
		t.Fatal("CF_EXPERIMENT_OUTPUT must name an evidence directory")
	}
	const fixture = `package main
var visits int
//garble:controlflow DIRECTIVE
//go:noinline
func compute(x uint32, b bool) uint32 {
 if b { visits++ } else { visits+=2 }
 if x&1!=0 { visits+=3 } else { visits+=4 }
 if x&2!=0 { visits+=5 } else { visits+=6 }
 if x&4!=0 { visits+=7 } else { visits+=8 }
 return (x+1)*2
}
func main() { for x:=uint32(0); x<100; x++ { if compute(x,true)!=(x+1)*2 || compute(x,false)!=(x+1)*2 { panic("result") } }; if visits!=3604 { panic(visits) } }
`
	for _, variant := range []string{"flatten_passes=0", "flatten_passes=1", directive} {
		for _, seed := range []int64{1, 2, 3} {
			dst := filepath.Join(dir, fmt.Sprintf("%s-seed%d", strings.ReplaceAll(variant, " ", "_"), seed))
			if err := os.MkdirAll(dst, 0700); err != nil {
				t.Fatal(err)
			}
			src := renderExperiment(t, strings.Replace(fixture, "DIRECTIVE", variant, 1), seed)
			src = strings.Replace(src, "func compute(", "//go:noinline\nfunc compute(", 1)
			path := filepath.Join(dst, "main.go")
			if err := os.WriteFile(path, []byte(src), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, "go.mod"), []byte("module probe\n\ngo 1.27\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("go", "build", "-gcflags=-S", "-o", filepath.Join(dst, "probe"), path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if err := os.WriteFile(filepath.Join(dst, "assembly.txt"), out, 0600); err != nil {
				t.Fatal(err)
			}
			cmd = exec.Command(filepath.Join(dst, "probe"))
			out, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute: %v\n%s", err, out)
			}
			bench := `package main
import "testing"
var sink uint32
func BenchmarkCompute(b *testing.B) { for i:=0;i<b.N;i++ { sink=compute(uint32(i),i&1!=0) } }
`
			if err := os.WriteFile(filepath.Join(dst, "probe_test.go"), []byte(bench), 0600); err != nil {
				t.Fatal(err)
			}
			cmd = exec.Command("go", "test", "-run=^$", "-bench=BenchmarkCompute", "-benchmem", "-benchtime=1000000x", "-count=3")
			cmd.Dir = dst
			out, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bench: %v\n%s", err, out)
			}
			if err := os.WriteFile(filepath.Join(dst, "bench.txt"), out, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", dst)
		}
	}
}
