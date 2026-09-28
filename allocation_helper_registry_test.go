package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func missingAllocationHelper(expected map[string]bool, registry []string) string {
	for name := range expected {
		if !slices.Contains(registry, name) {
			return name
		}
	}
	return ""
}

func TestAllocationHelperRegistryMatchesPinnedRuntime(t *testing.T) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(strings.TrimSpace(string(out)), "src/runtime/malloc_generated.go")
	file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	expected := make(map[string]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		for _, prefix := range []string{"mallocgcSmallNoScanSC", "mallocgcSmallScanNoHeaderSC", "mallocgcTinySC"} {
			if suffix, ok := strings.CutPrefix(name, prefix); ok && suffix != "" {
				numbered := true
				for _, ch := range suffix {
					if ch < '0' || ch > '9' {
						numbered = false
					}
				}
				if numbered {
					expected[name] = true
				}
			}
		}
	}
	if len(expected) == 0 {
		t.Fatal("no generated allocation helpers found in pinned runtime")
	}
	if name := missingAllocationHelper(expected, builtinSymbols["runtime"]); name != "" {
		t.Errorf("missing generated runtime helper %s", name)
	}
	for name := range expected {
		if isToolchainNameDependency("runtime", name) {
			t.Errorf("helper %s remains exempt from renaming", name)
		}
	}
	// Prove the same check rejects a table that lost a generated helper.
	missing := slices.Clone(builtinSymbols["runtime"])
	for name := range expected {
		missing = slices.DeleteFunc(missing, func(value string) bool { return value == name })
		if got := missingAllocationHelper(expected, missing); got != name {
			t.Fatalf("removed %s, completeness check reported %s", name, got)
		}
		break
	}
}
