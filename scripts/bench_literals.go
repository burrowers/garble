// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

//go:build ignore

// bench_literals benchmarks the build and run-time impact of each literal
// obfuscator across a range of string sizes.
//
// It generates one Go module per string size, each containing many strings of
// that size. For each obfuscator, it measures:
//   - Build time: time to "garble -literals build" versus "go build"
//   - Run time: time to execute the resulting binary (strings are decrypted at init)
//
// The garble binary is built with the garble_testing tag to allow forcing a
// specific obfuscator via GARBLE_TEST_LITERALS_OBFUSCATOR_MAP.
//
// Each build iteration writes a unique dummy var to bust the build cache
// without rebuilding the entire runtime. A 5s timeout kills any build or run
// that takes too long, reporting "timeout" on stderr and skipping.
//
// Output is in Go benchmark format, compatible with benchstat.
//
// Usage: go run ./scripts/bench_literals.go [-count N] [-timeout duration] [-run regexp]
//
// Then compare all obfuscators against the go baseline with:
//
//	benchstat -col /obf results.txt
//
// Or, if you obtained all results but want to show just one:
//
//	benchstat -col /obf -filter '/obf:(go OR swap)' out
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var rng *mathrand.ChaCha8

func init() {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	rng = mathrand.NewChaCha8(seed)
}

var (
	count     = flag.Int("count", 1, "number of iterations for each benchmark")
	timeout   = flag.Duration("timeout", 5*time.Second, "timeout per build or run invocation")
	runFilter = flag.String("run", "", "regexp to filter which obfuscators to run (always includes go baseline)")
)

// obfuscatorNames maps index to name, matching internal/literals/obfuscators.go.
var obfuscatorNames = []string{
	"simple",
	"swap",
	"split",
	"shuffle",
	"seed",
}

// stringSizes to benchmark, from just above MinSize (8) to the maxSize limit (2048).
var stringSizes = []int{
	16, 64, 256, 1024, 2048,
}

func main() {
	flag.Parse()

	garbleDir, err := os.Getwd()
	if err != nil {
		fatal("getwd: %v", err)
	}

	tmpDir, err := os.MkdirTemp("", "garble-bench-literals-*")
	if err != nil {
		fatal("mkdtemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Build garble with garble_testing tag.
	garbleBin := filepath.Join(tmpDir, "garble")
	fmt.Fprintf(os.Stderr, "building garble with garble_testing tag...\n")
	runCmdNoTimeout(garbleDir, nil, "go", "build", "-tags=garble_testing", "-o", garbleBin, ".")

	// Generate one module per string size.
	sizeDirs := make(map[int]string)
	for _, size := range stringSizes {
		dir := filepath.Join(tmpDir, fmt.Sprintf("size_%d", size))
		must(os.MkdirAll(dir, 0o755))
		writeTestModule(dir, size)
		sizeDirs[size] = dir
	}

	// Warm the garble cache by building one program.
	// Garble caches transformed stdlib, so the first build is much slower.
	fmt.Fprintf(os.Stderr, "warming garble cache...\n")
	warmEnv := []string{"GARBLE_TEST_LITERALS_OBFUSCATOR_MAP=main=0"}
	warmBin := filepath.Join(tmpDir, "warmup_bin")
	runCmdNoTimeout(sizeDirs[16], warmEnv, garbleBin, "-literals", "build", "-o", warmBin, ".")

	// Baseline: go build.
	for _, size := range stringSizes {
		bin := filepath.Join(tmpDir, fmt.Sprintf("go_%d_bin", size))
		benchAndPrint("Build/obf=go", size, sizeDirs[size], nil, "go", "build", "-o", bin, ".")
	}
	for _, size := range stringSizes {
		bin := filepath.Join(tmpDir, fmt.Sprintf("go_%d_bin", size))
		benchRunAndPrint("Run/obf=go", size, bin)
	}

	var filterRe *regexp.Regexp
	if *runFilter != "" {
		var err error
		filterRe, err = regexp.Compile(*runFilter)
		if err != nil {
			fatal("bad -run regexp: %v", err)
		}
	}

	// Each obfuscator.
	for idx, name := range obfuscatorNames {
		if filterRe != nil && !filterRe.MatchString(name) {
			fmt.Fprintf(os.Stderr, "skipping obfuscator %s (filtered by -run)\n", name)
			continue
		}
		env := []string{fmt.Sprintf("GARBLE_TEST_LITERALS_OBFUSCATOR_MAP=main=%d", idx)}
		for _, size := range stringSizes {
			bin := filepath.Join(tmpDir, fmt.Sprintf("garble_%s_%d_bin", name, size))
			benchAndPrint("Build/obf="+name, size, sizeDirs[size], env, garbleBin, "-literals", "build", "-o", bin, ".")
		}
		for _, size := range stringSizes {
			bin := filepath.Join(tmpDir, fmt.Sprintf("garble_%s_%d_bin", name, size))
			benchRunAndPrint("Run/obf="+name, size, bin)
		}
	}
}

// benchAndPrint runs the build command *count times, printing one benchstat line per iteration.
func benchAndPrint(benchName string, size int, dir string, extraEnv []string, args ...string) {
	name := fmt.Sprintf("Benchmark%s/%dB", benchName, size)
	timedOut := false
	for range *count {
		if timedOut {
			break
		}
		// Write a unique entrypoint file each iteration to bust the build cache
		// without rebuilding the entire runtime.
		entry := fmt.Sprintf("package main\n\nvar _benchIter uint64 = %d\n", rng.Uint64())
		must(os.WriteFile(filepath.Join(dir, "bench_iter.go"), []byte(entry), 0o644))

		d, ok := runCmdWithTimeout(dir, extraEnv, args[0], args[1:]...)
		if !ok {
			fmt.Printf("%s 1 %d ns/op\n", name, timeout.Nanoseconds())
			fmt.Fprintf(os.Stderr, "    ^ timed out: %s %s\n", args[0], strings.Join(args[1:], " "))
			timedOut = true
			continue
		}
		fmt.Printf("%s 1 %d ns/op\n", name, d.Nanoseconds())
	}
}

// benchRunAndPrint runs the binary *count times, printing one benchstat line per iteration.
func benchRunAndPrint(benchName string, size int, bin string) {
	name := fmt.Sprintf("Benchmark%s/%dB", benchName, size)
	if _, err := os.Stat(bin); err != nil {
		// Binary wasn't built (build timed out).
		fmt.Printf("%s 1 %d ns/op\n", name, timeout.Nanoseconds())
		fmt.Fprintf(os.Stderr, "    ^ binary not built (build timed out)\n")
		return
	}
	for range *count {
		d, ok := runCmdWithTimeout("", nil, bin)
		if !ok {
			fmt.Printf("%s 1 %d ns/op\n", name, timeout.Nanoseconds())
			fmt.Fprintf(os.Stderr, "    ^ timed out: %s\n", bin)
			return
		}
		fmt.Printf("%s 1 %d ns/op\n", name, d.Nanoseconds())
	}
}

// runCmdWithTimeout runs the command with the configured timeout.
// It returns the elapsed time and true on success, or zero and false on timeout.
// The command is started in its own process group so that on timeout,
// the entire tree (including child processes like subcompilers) is killed.
func runCmdWithTimeout(dir string, extraEnv []string, name string, args ...string) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)

	// Kill the entire process group, not just the leader.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		return 0, false
	}
	if err != nil {
		fatal("command %s %v failed: %v", name, args, err)
	}
	return elapsed, true
}

// runCmdNoTimeout runs a command without any timeout (used for setup steps).
func runCmdNoTimeout(dir string, extraEnv []string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("command %s %v failed: %v", name, args, err)
	}
}

// numStrings is the number of strings per size in each test program.
// High enough to amortize per-build overhead and produce stable timings,
// low enough to keep individual builds fast.
const numStrings = 20

func writeTestModule(dir string, stringSize int) {
	goMod := "module test/bench\n\ngo 1.26\n"
	must(os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644))

	var b strings.Builder
	b.WriteString("package main\n\nimport \"os\"\n\n")

	for i := range numStrings {
		str := randomHex(stringSize)
		fmt.Fprintf(&b, "var s%d = %q\n", i, str)
	}

	b.WriteString("\nfunc main() {\n")
	b.WriteString("\tf, _ := os.Create(os.DevNull)\n")
	for i := range numStrings {
		fmt.Fprintf(&b, "\tf.WriteString(s%d)\n", i)
	}
	b.WriteString("\tf.Close()\n")
	b.WriteString("}\n")

	must(os.WriteFile(filepath.Join(dir, "main.go"), []byte(b.String()), 0o644))
}

func randomHex(n int) string {
	raw := make([]byte, (n+1)/2)
	rng.Read(raw)
	return hex.EncodeToString(raw)[:n]
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
