// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const funcPkgPathSource = `package runtime

func funcpkgpath(f funcInfo) string {
	name := funcNameForPrint(funcname(f))
	i := len(name) - 1
	for ; i > 0; i-- {
		if name[i] == '/' {
			break
		}
	}
	for ; i < len(name); i++ {
		if name[i] == '.' {
			break
		}
	}
	return name[:i]
}
`

func TestTinyFuncPkgPathSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "symtab.go")
	if err := os.WriteFile(path, []byte("package runtime\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tinyFuncPkgPathSource(path); err == nil {
		t.Fatal("unrecognized symtab source accepted")
	}

	if err := os.WriteFile(path, []byte(funcPkgPathSource), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := tinyFuncPkgPathSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(src, `if name == "" {`); got != 1 {
		t.Fatalf("guard inserted %d times, want 1", got)
	}
	guarded := `name := funcNameForPrint(funcname(f))
	if name == "" {`
	if !strings.Contains(src, guarded) {
		t.Fatalf("guard does not follow the name lookup:\n%s", src)
	}
	// The rest of the function must survive, including the loops which are
	// only correct once the name is known to be non-empty.
	if !strings.Contains(src, "return name[:i]") {
		t.Fatalf("original body lost:\n%s", src)
	}

	// A covered build compiles cmd/cover's copy of the file, where the counter
	// statement of the first block precedes the name declaration on the same
	// line as the signature.
	const coveredSource = `package runtime

func funcpkgpath(f funcInfo) string {goCover_909ca0096d51__12[5] = 1;
	name := funcNameForPrint(funcname(f))
	i := len(name) - 1
	return name[:i]
}
`
	if err := os.WriteFile(path, []byte(coveredSource), 0600); err != nil {
		t.Fatal(err)
	}
	src, err = tinyFuncPkgPathSource(path)
	if err != nil {
		t.Fatalf("covered source rejected: %v", err)
	}
	if got := strings.Count(src, `if name == "" {`); got != 1 {
		t.Fatalf("guard inserted %d times in covered source, want 1", got)
	}
	if !strings.Contains(src, "goCover_909ca0096d51__12[5] = 1;") {
		t.Fatalf("covered counter lost:\n%s", src)
	}
}
