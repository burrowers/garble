// This script generate benchmarks for performance analysis of individual obfuscator literals.
// Note that only the speed of obfuscated methods is measured, initialization cost or build speed are not measured.

//go:build ignore

package main

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"mvdan.cc/garble/internal/literals"
)

const (
	minDataLen      = 8
	maxDataLen      = 64
	stepDataLen     = 4
	dataCountPerLen = 10
	moduleName      = "test/literals"
	garbleSeed      = "o9WDTZ4CN4w"

	// For benchmarking individual obfuscators, we use package=obfuscator mapping
	// and add a prefix to package name to make sure there are no collisions with system packages.
	packagePrefix = "literals_bench_"
)

func generateRunSrc() string {
	var sb strings.Builder

	sb.WriteString(`
var alwaysFalseFlag = false

func noop(i any) {
	if alwaysFalseFlag {
		println(i)
	}	
}

func Run() {
`)

	dataStr := strings.Repeat("X", maxDataLen)
	dataBytes := make([]string, maxDataLen)
	for i := 0; i < len(dataBytes); i++ {
		dataBytes[i] = strconv.Itoa(i)
	}

	for dataLen := minDataLen; dataLen <= maxDataLen; dataLen += stepDataLen {
		for y := 0; y < dataCountPerLen; y++ {
			fmt.Fprintf(&sb, "\tnoop(%q)\n", dataStr[:dataLen])
			fmt.Fprintf(&sb, "\tnoop([]byte{%s})\n", strings.Join(dataBytes[:dataLen], ", "))
		}
	}

	sb.WriteString("}\n")
	return sb.String()
}

func buildTestGarble(tdir string) string {
	garbleBin := filepath.Join(tdir, "garble")
	if runtime.GOOS == "windows" {
		garbleBin += ".exe"
	}

	output, err := exec.Command("go", "build", "-tags", "garble_testing", "-o="+garbleBin).CombinedOutput()
	if err != nil {
		log.Fatalf("garble build failed: %v\n%s", err, string(output))
	}

	return garbleBin
}

func handle(err error) {
	if err != nil {
		panic(err)
	}
}

func writeDateFile(tdir, obfName, src string) {
	pkgName := packagePrefix + obfName

	var sb strings.Builder
	fmt.Fprintf(&sb, "package %s\n\n", pkgName)
	sb.WriteString(src)

	dir := filepath.Join(tdir, pkgName)
	handle(os.MkdirAll(dir, 0o777))
	handle(os.WriteFile(filepath.Join(dir, "data.go"), []byte(sb.String()), 0o777))
}

func writeTestFile(dir, obfName string) {
	var sb strings.Builder
	sb.WriteString(`package main
import "testing"
`)
	pkgName := packagePrefix + obfName
	fmt.Fprintf(&sb, "import %q\n", moduleName+"/"+pkgName)
	fmt.Fprintf(&sb, `func Benchmark%s(b *testing.B) {
	for i := 0; i < b.N; i++ {
		%s.Run()		
	}
}
`, strings.ToUpper(obfName[:1])+obfName[1:], pkgName)

	handle(os.WriteFile(filepath.Join(dir, obfName+"_test.go"), []byte(sb.String()), 0o777))
}

func main() {
	tdir, err := os.MkdirTemp("", "literals-bench*")
	if err != nil {
		log.Fatalf("create temp directory failed: %v", err)
	}
	defer os.RemoveAll(tdir)

	if err := os.WriteFile(filepath.Join(tdir, "go.mod"), []byte("module "+moduleName), 0o777); err != nil {
		log.Fatalf("write go.mod failed: %v", err)
	}

	runSrc := generateRunSrc()
	writeTest := func(name string) {
		writeDateFile(tdir, name, runSrc)
		writeTestFile(tdir, name)
	}

	var packageToObfuscatorIndex []string
	for i, obf := range literals.Obfuscators {
		obfName := reflect.TypeOf(obf).Name()
		writeTest(obfName)
		packageToObfuscatorIndex = append(packageToObfuscatorIndex, fmt.Sprintf(packagePrefix+"%s=%d", obfName, i))
	}
	writeTest("all")

	garbleBin := buildTestGarble(tdir)
	args := append([]string{"-seed", garbleSeed, "-literals", "test", "-bench"}, os.Args[1:]...)
	cmd := exec.Command(garbleBin, args...)
	cmd.Env = append(cmd.Environ(),
		// Explicitly specify package for obfuscation to avoid affecting testing package.
		"GOGARBLE="+moduleName,
		"GARBLE_TEST_LITERALS_OBFUSCATOR_MAP="+strings.Join(packageToObfuscatorIndex, ","),
	)
	cmd.Dir = tdir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("run garble test failed: %v", err)
	}
}
