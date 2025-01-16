// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	_ "embed"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-quicktest/qt"
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
	tdir := b.TempDir()

	// We collect extra metrics.
	var memoryAllocs, cachedTime, systemTime int64

	outputBin := filepath.Join(tdir, "output")
	sourceDir := filepath.Join(tdir, "src")
	qt.Assert(b, qt.IsNil(os.Mkdir(sourceDir, 0o777)))

	writeSourceFile := func(name string, content []byte) {
		err := os.WriteFile(filepath.Join(sourceDir, name), content, 0o666)
		qt.Assert(b, qt.IsNil(err))
	}
	writeSourceFile("go.mod", []byte("module test/main"))
	writeSourceFile("main.go", benchSourceMain)

	rxGarbleAllocs := regexp.MustCompile(`(?m)^garble allocs: ([0-9]+)`)

	b.ResetTimer()
	b.StopTimer()
	for i := range b.N {
		// First we do a fresh build, using empty cache directories,
		// and the second does an incremental rebuild reusing the same cache directories.
		goCache := filepath.Join(tdir, "go-cache")
		qt.Assert(b, qt.IsNil(os.RemoveAll(goCache)))
		qt.Assert(b, qt.IsNil(os.Mkdir(goCache, 0o777)))
		garbleCache := filepath.Join(tdir, "garble-cache")
		qt.Assert(b, qt.IsNil(os.RemoveAll(garbleCache)))
		qt.Assert(b, qt.IsNil(os.Mkdir(garbleCache, 0o777)))
		env := []string{
			"RUN_GARBLE_MAIN=true",
			"GOCACHE=" + goCache,
			"GARBLE_CACHE=" + garbleCache,
			"GARBLE_WRITE_ALLOCS=true",
		}
		if prof := flag.Lookup("test.cpuprofile").Value.String(); prof != "" {
			// Ensure the directory is empty and created, and pass it along, so that the garble
			// sub-processes can also write CPU profiles.
			// Collect and then merge the profiles as follows:
			//
			//    go test -run=- -vet=off -bench=. -benchtime=5x -cpuprofile=cpu.pprof
			//    go tool pprof -proto cpu.pprof cpu.pprof-subproc/* >merged.pprof
			dir, err := filepath.Abs(prof + "-subproc")
			qt.Assert(b, qt.IsNil(err))
			err = os.RemoveAll(dir)
			qt.Assert(b, qt.IsNil(err))
			err = os.MkdirAll(dir, 0o777)
			qt.Assert(b, qt.IsNil(err))
			env = append(env, "GARBLE_WRITE_CPUPROFILES="+dir)
		}
		if prof := flag.Lookup("test.memprofile").Value.String(); prof != "" {
			// Same as before, but for allocation profiles.
			dir, err := filepath.Abs(prof + "-subproc")
			qt.Assert(b, qt.IsNil(err))
			err = os.RemoveAll(dir)
			qt.Assert(b, qt.IsNil(err))
			err = os.MkdirAll(dir, 0o777)
			qt.Assert(b, qt.IsNil(err))
			env = append(env, "GARBLE_WRITE_MEMPROFILES="+dir)
		}
		args := []string{"build", "-v", "-o=" + outputBin, sourceDir}

		for _, cached := range []bool{false, true} {
			// The cached rebuild will reuse all dependencies,
			// but rebuild the main package itself.
			if cached {
				writeSourceFile("rebuild.go", fmt.Appendf(nil, "package main\nvar v%d int", i))
			}

			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(cmd.Environ(), env...)
			cmd.Dir = sourceDir

			cachedStart := time.Now()
			b.StartTimer()
			out, err := cmd.CombinedOutput()
			b.StopTimer()
			if cached {
				cachedTime += time.Since(cachedStart).Nanoseconds()
			}

			qt.Assert(b, qt.IsNil(err), qt.Commentf("output: %s", out))
			if !cached {
				// Ensure that we built all packages, as expected.
				qt.Assert(b, qt.IsTrue(rxBuiltRuntime.Match(out)))
			} else {
				// Ensure that we only rebuilt the main package, as expected.
				qt.Assert(b, qt.IsFalse(rxBuiltRuntime.Match(out)))
			}
			qt.Assert(b, qt.IsTrue(rxBuiltMain.Match(out)))

			matches := rxGarbleAllocs.FindAllSubmatch(out, -1)
			if !cached {
				// The non-cached version should have at least a handful of
				// sub-processes; catch if our logic breaks.
				qt.Assert(b, qt.IsTrue(len(matches) > 5))
			}
			for _, match := range matches {
				allocs, err := strconv.ParseInt(string(match[1]), 10, 64)
				qt.Assert(b, qt.IsNil(err))
				memoryAllocs += allocs
			}

			systemTime += int64(cmd.ProcessState.SystemTime())
		}
	}
	// We can't use "allocs/op" as it's reserved for ReportAllocs.
	b.ReportMetric(float64(memoryAllocs)/float64(b.N), "mallocs/op")
	b.ReportMetric(float64(cachedTime)/float64(b.N), "cached-ns/op")
	b.ReportMetric(float64(systemTime)/float64(b.N), "sys-ns/op")
	info, err := os.Stat(outputBin)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(info.Size()), "bin-B")
}

func BenchmarkAbiOriginalNames(b *testing.B) {
	// Benchmark two thousand obfuscated names in _originalNamePairs
	// and a variety of input strings to reverse.
	// As an example, the cmd/go binary ends up with about 2200 entries
	// in _originalNamePairs as of November 2024, so it's a realistic figure.
	// Structs with tens of fields are also relatively normal.
	salt := []byte("some salt bytes")
	for n := range 2000 {
		name := fmt.Sprintf("name_%d", n)
		garbled := hashWithCustomSalt(salt, name)
		_originalNamePairs = append(_originalNamePairs, garbled, name)
	}
	_originalNamesInit()
	// Pick twenty obfuscated names at random to use as inputs below.
	// Use a deterministic random source so it's stable between benchmark runs.
	rnd := rand.New(rand.NewPCG(1, 2))
	var chosen []string
	for i := 0; i < len(_originalNamePairs); i += 2 {
		chosen = append(chosen, _originalNamePairs[i])
	}
	rnd.Shuffle(len(chosen), func(i, j int) {
		chosen[i], chosen[j] = chosen[j], chosen[i]
	})
	chosen = chosen[:20]

	inputs := []string{
		// non-obfuscated names and types
		"Error",
		"int",
		"*[]*interface {}",
		"*map[uint64]bool",
		// an obfuscated name
		chosen[0],
		// an obfuscated *pkg.Name
		fmt.Sprintf("*%s.%s", chosen[1], chosen[2]),
		// big struct with more than a dozen string field types
		fmt.Sprintf("struct { %s string }", strings.Join(chosen[3:], " string ")),
	}

	var inputBytes int
	for _, input := range inputs {
		inputBytes += len(input)
	}
	b.SetBytes(int64(inputBytes))
	b.ReportAllocs()
	b.ResetTimer()

	// We use a parallel benchmark because internal/abi's Name method
	// is meant to be called by any goroutine at any time.
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for _, input := range inputs {
				_originalNames(input)
			}
		}
	})
	_originalNamePairs = []string{}
	_originalNamesReplacer = nil
}
