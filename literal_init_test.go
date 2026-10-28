// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quicktest/qt"

	"mvdan.cc/garble/internal/patcher"
)

func TestLiteralInitManifestCompiler(t *testing.T) {
	dir := t.TempDir()
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	qt.Assert(t, qt.IsNil(err))
	version, err := exec.Command("go", "env", "GOVERSION").Output()
	qt.Assert(t, qt.IsNil(err))
	cache, err := os.UserCacheDir()
	qt.Assert(t, qt.IsNil(err))
	compiler, err := patcher.PatchCompiler("", strings.TrimSpace(string(goroot)), strings.TrimSpace(string(version)), filepath.Join(cache, "garble"), dir)
	qt.Assert(t, qt.IsNil(err))
	source := filepath.Join(dir, "case.go")
	if err := os.WriteFile(source, []byte("package p\nvar value = factory()\n//go:noinline\nfunc factory() string { return \"value\" }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("inactive-stock-parity", func(t *testing.T) {
		toolDir, err := exec.Command("go", "env", "GOTOOLDIR").Output()
		qt.Assert(t, qt.IsNil(err))
		object := filepath.Join(dir, "inactive.o")
		args := []string{"-p", "p", "-o", object, source}
		var stock []byte
		for _, tool := range []string{filepath.Join(strings.TrimSpace(string(toolDir)), "compile"), compiler} {
			if runtime.GOOS == "windows" && !strings.HasSuffix(tool, ".exe") {
				tool += ".exe"
			}
			cmd := exec.Command(tool, args...)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GARBLE_LITERAL_INITS_FILE=") && !strings.HasPrefix(entry, "GARBLE_PKGPATH_MAP=") && !strings.HasPrefix(entry, "GARBLE_SYMBOL_MAP=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("inactive compilation: %v\n%s", err, output)
			}
			data, err := os.ReadFile(object)
			qt.Assert(t, qt.IsNil(err))
			if stock == nil {
				stock = data
			} else if !bytes.Equal(stock, data) {
				t.Fatal("inactive patched compiler differs from stock compiler")
			}
		}
	})
	certificates := map[string]string{"value": "factory"}
	for i := range 10000 {
		certificates[fmt.Sprintf("unused%06d", i)] = fmt.Sprintf("factory%06d", i)
	}
	data, err := json.Marshal(certificates)
	qt.Assert(t, qt.IsNil(err))
	if len(data) <= 131072 {
		t.Fatal("manifest does not exceed Linux single-string limit")
	}
	t.Logf("manifest bytes: %d", len(data))
	// Match objabi.RelocType.IsDirectCall, never an indirect or weak call.
	eagerFactoryCall := regexp.MustCompile(`(?m)\bt=(?:R_CALL(?:ARM|ARM64|LOONG64|MIPS|POWER)?|R_RISCV_(?:CALL|JAL|JAL_TRAMP)) p\.factory\+0$`)
	for _, tc := range []struct{ name, data, diagnostic string }{
		{"aggregate-array", `{"value":"factory"}`, ""},
		{"aggregate-struct", `{"value":"factory"}`, ""},
		{"aggregate-map", `{"value":"factory"}`, ""},
		{"aggregate-interface", `{"value":"factory"}`, "invalid certified literal initializer"},
		{"aggregate-slice", `{"value":"factory"}`, "invalid certified literal initializer"},
		{"large", string(data), ""},
		{"large-moved", string(data), ""},
		{"darwin-arm64-pic", string(data), ""},
		{"dynlink", string(data), ""},
		{"linkshared", string(data), ""},
		{"linkname-darwin-arm64", string(data), ""},
		{"linkname", string(data), ""},
		{"linkname-single", string(data), ""},
		{"linknamestd", string(data), ""},
		{"linknamestd-single", string(data), ""},
		{"mismatched-factory", `{"value":"otherFactory"}`, "invalid certified literal factory"},
		{"invalid-assignment", `{"value":"factory"}`, "invalid certified literal initializer"},
		{"malformed", "{bad json", "invalid literal init certificate"},
		{"unreadable", "", "cannot read literal init certificate"},
		{"malformed-noinit", "{bad json", "invalid literal init certificate"},
		{"unreadable-noinit", "", "cannot read literal init certificate"},
		{"malformed-shared", "{bad json", "invalid literal init certificate"},
		{"malformed-disabled", "{bad json", "invalid literal init certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if !strings.HasPrefix(tc.name, "unreadable") {
				if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			text := "package p\nvar value = factory()\n//go:noinline\nfunc factory() string { return \"value\" }\n"
			if strings.HasPrefix(tc.name, "aggregate-") {
				result := map[string]string{"aggregate-map": "map[string]rune", "aggregate-array": "[2]string", "aggregate-struct": "struct{ Text string }", "aggregate-interface": "any", "aggregate-slice": "[]string"}[tc.name]
				body := result + "{}"
				if result == "any" {
					body = `"value"`
				}
				text = "package p\nvar value = factory()\n//go:noinline\nfunc factory() " + result + " { return " + body + " }\n"
			}
			linked := strings.HasPrefix(tc.name, "linkname")
			if linked {
				directive := "//go:linkname value sharedAliasReview"
				if tc.name == "linkname-single" {
					directive = "//go:linkname value"
				} else if tc.name == "linknamestd" {
					directive = "//go:linknamestd value sharedAliasReview"
				} else if tc.name == "linknamestd-single" {
					directive = "//go:linknamestd value"
				}
				text = "package p\nimport _ \"unsafe\"\n" + directive + "\nvar value = factory()\n//go:noinline\nfunc factory() string { return \"value\" }\n"
			}
			if tc.name == "invalid-assignment" {
				text = "package p\nvar value = factory(1)\n//go:noinline\nfunc factory(n int) string { return \"value\" }\n"
			}
			if strings.HasSuffix(tc.name, "-noinit") {
				text = "package p\n"
			}
			if err := os.WriteFile(source, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-S", "-p", "p", "-o", filepath.Join(dir, tc.name+".o")}
			if strings.HasSuffix(tc.name, "-shared") || tc.name == "darwin-arm64-pic" {
				args = append(args, "-shared")
			}
			if tc.name == "dynlink" || tc.name == "linkshared" {
				args = append(args, "-"+tc.name)
			}
			if strings.HasSuffix(tc.name, "-disabled") {
				args = append(args, "-d=wrapglobalmapctl=1")
			}
			cmd := exec.Command(compiler, append(args, source)...)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GARBLE_LITERAL_INITS=") && !strings.HasPrefix(entry, "GARBLE_LITERAL_INITS_FILE=") && !strings.HasPrefix(entry, "GARBLE_PKGPATH_MAP=") && !strings.HasPrefix(entry, "GARBLE_SYMBOL_MAP=") && !strings.HasPrefix(entry, "GOOS=") && !strings.HasPrefix(entry, "GOARCH=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			if strings.Contains(tc.name, "darwin-arm64") {
				cmd.Env = append(cmd.Env, "GOOS=darwin", "GOARCH=arm64")
			}
			cmd.Env = append(cmd.Env, "GARBLE_LITERAL_INITS_FILE="+path)
			output, err := cmd.CombinedOutput()
			if tc.diagnostic != "" {
				if err == nil || !strings.Contains(string(output), tc.diagnostic) {
					t.Fatalf("compiler did not fail closed: err=%v\n%s", err, output)
				}
			} else if linked || tc.name == "dynlink" || tc.name == "linkshared" {
				if err != nil || strings.Contains(string(output), "R_KEEP") || strings.Contains(string(output), "map.init.0") || !eagerFactoryCall.Match(output) {
					t.Fatalf("linknamed initializer was not kept eager: err=%v\n%s", err, output)
				}
			} else if err != nil || !strings.Contains(string(output), "R_KEEP") || !strings.Contains(string(output), "map.init.0") {
				t.Fatalf("file manifest did not certify initializer: err=%v\n%s", err, output)
			}
			if tc.name == "large-moved" {
				original, err := os.ReadFile(filepath.Join(dir, "large.o"))
				if err != nil {
					t.Fatal(err)
				}
				moved, err := os.ReadFile(filepath.Join(dir, "large-moved.o"))
				if err != nil {
					t.Fatal(err)
				}
				if string(original) != string(moved) {
					t.Fatal("compiler output depends on manifest path")
				}
			}
		})
	}
}
