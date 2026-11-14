// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/garble/internal/patcher"
)

func TestMappedReflectHeaderWriteBarrier(t *testing.T) {
	dir := t.TempDir()
	goenv := func(key string) string {
		t.Helper()
		out, err := exec.Command("go", "env", key).CombinedOutput()
		if err != nil {
			t.Fatalf("go env %s: %v\n%s", key, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := patcher.PatchCompiler(os.Getenv(patcher.GoSrcEnv), goenv("GOROOT"), goenv("GOVERSION"), filepath.Join(cache, "garble"), dir)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	compile := func(mapping string, args ...string) string {
		t.Helper()
		cmd := exec.Command(compiler, args...)
		cmd.Env = append(os.Environ(), patcher.PkgPathMapEnv+"="+mapping, patcher.SymbolMapEnv+"=", patcher.LiteralInitsEnv+"=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("compile: %v\n%s", err, out)
		}
		return string(out)
	}
	header := write("header.go", "package reflect\ntype SliceHeader struct { Data uintptr; Len, Cap int }\ntype StringHeader struct { Data uintptr; Len int }\n")
	for _, tc := range []struct {
		name, path, mapping string
		barriers            int
	}{
		{"ordinary", "reflect", "", 2},
		{"mapped", "hashedreflect", "hashedreflect=reflect", 2},
		{"unrelated", "unrelated", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(dir, tc.name+".a")
			compile(tc.mapping, "-p", tc.path, "-pack", "-o", archive, header)
			importcfg := write(tc.name+".importcfg", "packagefile "+tc.path+"="+archive+"\n")
			probe := write(tc.name+".go", "package p\nimport reflect \""+tc.path+"\"\nvar S reflect.SliceHeader\nvar T reflect.StringHeader\nfunc Store(p uintptr) { S.Data = p; T.Data = p }\n")
			out := compile(tc.mapping, "-p", "p", "-importcfg", importcfg, "-d=wb", "-o", filepath.Join(dir, tc.name+".o"), probe)
			if got := strings.Count(out, "write barrier"); got != tc.barriers {
				t.Fatalf("reflect header stores generated %d write barriers, want %d:\n%s", got, tc.barriers, out)
			}
		})
	}
}
