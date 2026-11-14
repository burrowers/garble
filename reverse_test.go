// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunTool(t *testing.T) {
	oldCache := sharedCache
	t.Cleanup(func() { sharedCache = oldCache })
	oldLog := log.Writer()
	t.Cleanup(func() { log.SetOutput(oldLog) })
	var debugLog bytes.Buffer
	log.SetOutput(&debugLog)
	dir := t.TempDir()
	lpkg := &listedPackage{
		Name: "p", ImportPath: "example/p", Dir: dir,
		CompiledGoFiles: []string{"original.go"}, GarbleActionID: [32]byte{1},
	}
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	sharedCache.ListedPackages.set(lpkg.ImportPath, lpkg)
	original := filepath.Join(dir, "original.go")
	originalSource := "package p\ntype OriginalType struct{}\nfunc f() {}\n"
	if err := os.WriteFile(original, []byte(originalSource), 0o600); err != nil {
		t.Fatal(err)
	}
	name := hashWithPackage(lpkg, "OriginalType")
	filename := hashWithPackage(lpkg, fmt.Sprintf("original.go:%d", strings.LastIndex(originalSource, "{"))) + ".go"
	input := filepath.Join(dir, "input.go")

	for _, mode := range []string{"failure", "success", "unchanged", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			body := "var x " + name + " = 1\n"
			if mode == "success" {
				body = "func f() *" + name + " { return new(" + name + ") }\n"
			}
			if mode == "unchanged" {
				body = "var x = undefined\n"
			}
			reportedFile := filename
			if mode == "unchanged" {
				reportedFile = "plain.go"
			}
			source := fmt.Sprintf("package p\ntype %s struct{}\n//line %s:3\n%s", name, reportedFile, body)
			if err := os.WriteFile(input, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "unavailable" {
				if err := os.Remove(original); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			cmd := exec.Command("go", "tool", "compile", "-m", "-o", filepath.Join(dir, "p.o"), input)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := runTool(cmd)
			if mode == "success" {
				if err != nil || !strings.Contains(stdout.String(), name) || stderr.Len() != 0 {
					t.Fatalf("successful diagnostics changed: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("compiler exit status was lost: %v", err)
			}
			if stdout.Len() == 0 {
				t.Fatal("original compiler diagnostic was lost")
			}
			if mode == "failure" {
				if !strings.Contains(stdout.String(), name) || !strings.Contains(stderr.String(), "OriginalType") || !strings.Contains(stderr.String(), "original.go:3") {
					t.Fatalf("missing raw or reversed diagnostic: stdout: %s\nstderr: %s", &stdout, &stderr)
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("unexpected reversal output: %s", &stderr)
			}
			if mode == "unavailable" && !strings.Contains(debugLog.String(), "could not reverse build errors:") {
				t.Fatal("missing debug log for failed reversal")
			}
		})
	}
}
