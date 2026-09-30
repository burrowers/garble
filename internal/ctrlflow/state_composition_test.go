package ctrlflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa/ssautil"
)

func TestStateUnsupportedComposition(t *testing.T) {
	for _, params := range []string{"flatten_passes=2", "flatten_passes=0", "flatten_hardening=xor", "block_splits=2", "junk_jumps=2", "trash_blocks=1"} {
		src := "package main\n//garble:controlflow state_transitions=1 " + params + "\nfunc compute(n int) int { if n>0 { return n+1 }; return n-1 }\n"
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, "main.go", src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		pkg, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("test/main", "main"), []*ast.File{file}, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = Obfuscate(fs, pkg, []*ast.File{file}, mathrand.New(mathrand.NewSource(1)))
		if err == nil || !strings.Contains(err.Error(), "state_transitions") {
			t.Fatalf("accepted %s: %v", params, err)
		}
	}
}
