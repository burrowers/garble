// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

//go:embed testdata/bench/main.go
var benchSourceMain []byte

var (
	rxBuiltRuntime = regexp.MustCompile(`(?m)^runtime$`)
	rxBuiltMain    = regexp.MustCompile(`(?m)^test/main$`)
)

// BenchmarkBuild is a benchmark for 'garble build' on a fairly simple
// main package with a handful of standard library depedencies.
//
// We use a real garble binary and exec it, to simulate what the real user would
// run. The real obfuscation and compilation will happen in sub-processes
// anyway, so skipping one exec layer doesn't help us in any way.
//
// The benchmark isn't parallel, because in practice users build once at a time,
// and each build already spawns concurrent processes and goroutines to do work.
//
// At the moment, each iteration takes 1-2s on a laptop, so we can't make the
// benchmark include any more features unless we make it significantly faster.
func BenchmarkBuild(b *testing.B) {
	// As of Go 1.17, using -benchtime=Nx with N larger than 1 results in two
	// calls to BenchmarkBuild, with the first having b.N==1 to discover
	// sub-benchmarks. Unfortunately, we do a significant amount of work both
	// during setup and during that first iteration, which is pointless.
	// To avoid that, detect the scenario in a hacky way, and return early.
	// See https://github.com/golang/go/issues/32051.
	benchtime := flag.Lookup("test.benchtime").Value.String()
	if b.N == 1 && strings.HasSuffix(benchtime, "x") && benchtime != "1x" {
		return
	}

	garbleBin := filepath.Join(b.TempDir(), "garble")
	if runtime.GOOS == "windows" {
		garbleBin += ".exe"
	}
	err := exec.Command("go", "build", "-o="+garbleBin).Run()
	qt.Assert(b, err, qt.IsNil)

	// We collect extra metrics.
	var cachedTime, userTime, systemTime int64

	outputBin := filepath.Join(b.TempDir(), "output")
	sourceDir := filepath.Join(b.TempDir(), "src")
	err = os.Mkdir(sourceDir, 0o777)
	qt.Assert(b, err, qt.IsNil)

	writeSourceFile := func(name string, content []byte) {
		err := os.WriteFile(filepath.Join(sourceDir, name), content, 0o666)
		qt.Assert(b, err, qt.IsNil)
	}
	writeSourceFile("go.mod", []byte("module test/main"))
	writeSourceFile("main.go", benchSourceMain)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// First we do a fresh build, using a new GOCACHE.
		// and the second does an incremental rebuild reusing the cache.
		gocache, err := os.MkdirTemp(b.TempDir(), "gocache-*")
		qt.Assert(b, err, qt.IsNil)
		env := append(os.Environ(), "GOGARBLE=*", "GOCACHE="+gocache)
		args := []string{"build", "-v", "-o=" + outputBin, sourceDir}

		cmd := exec.Command(garbleBin, args...)
		cmd.Env = env
		cmd.Dir = sourceDir

		out, err := cmd.CombinedOutput()
		qt.Assert(b, err, qt.IsNil, qt.Commentf("output: %s", out))
		// Ensure that we built all packages, as expected.
		qt.Assert(b, rxBuiltRuntime.Match(out), qt.IsTrue)
		qt.Assert(b, rxBuiltMain.Match(out), qt.IsTrue)

		// The cached rebuild will reuse all dependencies,
		// but rebuild the main package itself.
		cachedStart := time.Now()
		writeSourceFile("rebuild.go", []byte(fmt.Sprintf("package main\nvar v%d int", i)))

		cmd = exec.Command(garbleBin, args...)
		cmd.Env = env
		cmd.Dir = sourceDir

		out, err = cmd.CombinedOutput()
		qt.Assert(b, err, qt.IsNil, qt.Commentf("output: %s", out))
		// Ensure that we only rebuilt the main package, as expected.
		qt.Assert(b, rxBuiltRuntime.Match(out), qt.IsFalse)
		qt.Assert(b, rxBuiltMain.Match(out), qt.IsTrue)

		cachedTime += time.Since(cachedStart).Nanoseconds()
		userTime += int64(cmd.ProcessState.UserTime())
		systemTime += int64(cmd.ProcessState.SystemTime())
	}
	b.ReportMetric(float64(cachedTime)/float64(b.N), "cached-ns/op")
	b.ReportMetric(float64(userTime)/float64(b.N), "user-ns/op")
	b.ReportMetric(float64(systemTime)/float64(b.N), "sys-ns/op")
	info, err := os.Stat(outputBin)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(info.Size()), "bin-B")
}
