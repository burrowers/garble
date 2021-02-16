// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

// BenchmarkBuild is a parallel benchmark for 'garble build' on a fairly simple
// main package with a handful of standard library depedencies.
//
// We use a real garble binary and exec it, to simulate what the real user would
// run. The real obfuscation and compilation will happen in sub-processes
// anyway, so skipping one exec layer doesn't help us in any way.
//
// At the moment, each iteration takes 1-2s on a laptop, so we can't make the
// benchmark include any more features unless we make it significantly faster.
func BenchmarkBuild(b *testing.B) {
	tdir, err := ioutil.TempDir("", "garble-bench")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tdir)

	garbleBin := filepath.Join(tdir, "garble")
	if runtime.GOOS == "windows" {
		garbleBin += ".exe"
	}

	if err := exec.Command("go", "build", "-o="+garbleBin).Run(); err != nil {
		b.Fatalf("building garble: %v", err)
	}

	// We collect extra metrics.
	var n, userTime, systemTime int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cmd := exec.Command(garbleBin, "build", "./testdata/bench")
			if out, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("%v: %s", err, out)
			}

			atomic.AddInt64(&n, 1)
			atomic.AddInt64(&userTime, int64(cmd.ProcessState.UserTime()))
			atomic.AddInt64(&systemTime, int64(cmd.ProcessState.SystemTime()))
		}
	})
	b.ReportMetric(float64(userTime)/float64(n), "user-ns/op")
	b.ReportMetric(float64(systemTime)/float64(n), "sys-ns/op")
	info, err := os.Stat(garbleBin)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(info.Size()), "bin-B")
}
