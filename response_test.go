// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestToolResponseFile(t *testing.T) {
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

func TestReadToolArgs(t *testing.T) {
	want := []string{"compile", "", `C:\directory with spaces\`, "quotes'\"$`", "line\nbreak", "日本語", "@literal"}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "args.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readToolArgs([]string{"@" + file})
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("arguments: %q, %v; want %q", got, err, want)
	}
	for _, invalid := range []string{`[]`, `null`, `[""]`, `{}`, `not json`} {
		if err := os.WriteFile(file, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readToolArgs([]string{"@" + file}); err == nil {
			t.Errorf("accepted invalid tool arguments %s", invalid)
		}
	}
	if _, err := readToolArgs([]string{"@" + file + ".missing"}); err == nil {
		t.Error("accepted missing response file")
	}
	if _, err := readToolArgs(nil); err == nil {
		t.Error("accepted missing tool")
	}
}

func TestOutgoingToolResponseFile(t *testing.T) {
	out, err := exec.Command("go", "env", "GOTOOLDIR").Output()
	if err != nil {
		t.Fatal(err)
	}
	asm := filepath.Join(strings.TrimSpace(string(out)), "asm")
	// The assembler accepts arbitrary include directories; -V exits without
	// assembling source, but only after the preceding flags have been parsed.
	cmd := exec.Command(asm, "-I", strings.Repeat("long", 5000), "-I", `C:\with spaces\`, "-I", "quotes'\"$`\n日本語", "-V=full")
	cleanup, err := toolResponseFile(cmd, "asm")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(cmd.Args) != 2 || !strings.HasPrefix(cmd.Args[1], "@") {
		t.Fatalf("oversized arguments not replaced: %d arguments", len(cmd.Args))
	}
	file := cmd.Args[1][1:]
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "version go") {
		t.Fatalf("assembler response file: %v\n%s", err, out)
	}
	cleanup()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("response file not cleaned up: %v", err)
	}
	for _, tool := range []string{"asm", "unknown"} {
		args := []string{"tool", "-V=full"}
		if tool == "unknown" {
			args = append(args, strings.Repeat("x", 40000))
		}
		cmd := exec.Command(args[0], args[1:]...)
		cleanup, err := toolResponseFile(cmd, tool)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if !slices.Equal(cmd.Args, args) {
			t.Errorf("rewrote arguments for %s", tool)
		}
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
