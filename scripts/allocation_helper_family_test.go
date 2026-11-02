//go:build ignore

// Run with: go test scripts/gen_go_std_tables.go scripts/allocation_helper_family_test.go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A generated table still referring to a helper absent from the runtime
// declarations must stop generation, rather than silently shrinking the map.
func TestAllocationHelperMissingDeclarationFailsClosed(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "src", "runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"malloc_generated.go":        "package runtime\nfunc mallocgcSmallNoScanSC2() {}\n",
		"malloc_tables_generated.go": "package runtime\nvar mallocNoScanTable = []any{mallocgcSmallNoScanSC2, mallocgcSmallNoScanSC3}\n",
		"linkname_shim.go":           "package runtime\n",
		"stubs.go":                   "package runtime\nfunc goexit()\n",
	} {
		if err := os.WriteFile(filepath.Join(runtimeDir, name), []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(recovered.(string), "mallocgcSmallNoScanSC3") {
			t.Errorf("missing generated helper must fail closed, got panic %v", recovered)
		}
	}()
	runtimeSourceContracts(versionedString{String: root, GoVersionLang: "go1.27"})
}
