// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	garbleBin := filepath.Join(b.TempDir(), "garble")
	if runtime.GOOS == "windows" {
		garbleBin += ".exe"
	}

	if err := exec.Command("go", "build", "-o="+garbleBin).Run(); err != nil {
		b.Fatalf("building garble: %v", err)
	}

	for _, name := range [...]string{"Cache", "NoCache"} {
		b.Run(name, func(b *testing.B) {
			buildArgs := []string{"build", "-o=" + b.TempDir()}
			switch name {
			case "Cache":
				buildArgs = append(buildArgs, "./testdata/bench-cache")

				// Ensure the build cache is warm,
				// for the sake of consistent results.
				cmd := exec.Command(garbleBin, buildArgs...)
				if out, err := cmd.CombinedOutput(); err != nil {
					b.Fatalf("%v: %s", err, out)
				}
			case "NoCache":
				buildArgs = append(buildArgs, "./testdata/bench-nocache")
			default:
				b.Fatalf("unknown name: %q", name)
			}

			// We collect extra metrics.
			var userTime, systemTime int64

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					cmd := exec.Command(garbleBin, buildArgs...)
					if name == "NoCache" {
						gocache, err := os.MkdirTemp(b.TempDir(), "gocache-*")
						if err != nil {
							b.Fatal(err)
						}
						cmd.Env = append(os.Environ(), "GOCACHE="+gocache)
					}
					if out, err := cmd.CombinedOutput(); err != nil {
						b.Fatalf("%v: %s", err, out)
					}

					userTime += int64(cmd.ProcessState.UserTime())
					systemTime += int64(cmd.ProcessState.SystemTime())
				}
			})
			b.ReportMetric(float64(userTime)/float64(b.N), "user-ns/op")
			b.ReportMetric(float64(systemTime)/float64(b.N), "sys-ns/op")
			info, err := os.Stat(garbleBin)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(info.Size()), "bin-B")
		})
	}
}
