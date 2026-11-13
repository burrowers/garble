// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestToolResponseFile(t *testing.T) {
	t.Skip("TODO: accept response files from the Go driver")
	// The Go driver keeps Garble's flags and command outside the response file.
	// A harmless untransformed tool lets us exercise the real subprocess entry.
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "arguments with spaces.json")
	data, err := json.Marshal([]string{goTool, "version"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-tiny", "toolexec", "@"+file)
	cmd.Env = append(os.Environ(), "RUN_GARBLE_MAIN=true")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "go version ") {
		t.Fatalf("response invocation: %v\n%s", err, out)
	}
}

func TestLongToolArguments(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a package with many source files")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/manyfiles\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Source arguments alone exceed Windows' command-line limit before Garble
	// expands them into the shared temporary directory.
	for i := range 400 {
		name := filepath.Join(dir, strings.Repeat("source", 15)+strconv.Itoa(i)+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() { println(\"many files work\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RUN_GARBLE_MAIN=true")
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "many files work\n" {
		t.Fatalf("many-file build: %v\n%s", err, out)
	}
}
