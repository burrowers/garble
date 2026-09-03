// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package patcher

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionedToolName(t *testing.T) {
	t.Parallel()

	name := versionedToolName("compile", "go1.27.0", "patch-a")
	if name != versionedToolName("compile", "go1.27.0", "patch-a") {
		t.Fatal("versioned tool name is not deterministic")
	}
	for _, different := range []string{
		versionedToolName("compile", "go1.27.1", "patch-a"),
		versionedToolName("compile", "go1.27.0", "patch-b"),
		versionedToolName("link", "go1.27.0", "patch-a"),
	} {
		if different == name {
			t.Fatalf("versioned tool name collision: %q", name)
		}
	}
	if strings.ContainsAny(name, `/\`) {
		t.Fatalf("versioned tool name is not a base name: %q", name)
	}
}

func TestToolWorkspaceDirUsesOutputKey(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	one := toolWorkspaceDir(tempDir, filepath.Join("cache", "compile-version-a"))
	two := toolWorkspaceDir(tempDir, filepath.Join("cache", "compile-version-b"))
	if one == two {
		t.Fatalf("distinct cached tools share workspace %q", one)
	}
	if filepath.Dir(one) != tempDir || filepath.Dir(two) != tempDir {
		t.Fatalf("workspaces are not rooted in temporary directory: %q, %q", one, two)
	}
}

func TestGarbleMappingSourceIsOverlaid(t *testing.T) {
	t.Parallel()

	const file = "cmd/internal/objabi/garble.go"
	if !makeFileSet(compilerOverlayFiles)[file] {
		t.Fatalf("%q is not included in the compiler overlay", file)
	}
	if !makeFileSet(linkerOverlayFiles)[file] {
		t.Fatalf("%q is not included in the linker overlay", file)
	}
	if file := "cmd/internal/obj/x86/seh.go"; !makeFileSet(compilerOverlayFiles)[file] {
		t.Fatalf("%q is not included in the compiler and assembler overlay", file)
	}
	if file := "cmd/internal/obj/ppc64/obj9.go"; !makeFileSet(compilerOverlayFiles)[file] {
		t.Fatalf("%q is not included in the compiler and assembler overlay", file)
	}
	if file := "cmd/internal/objabi/pkgspecial.go"; !makeFileSet(linkerOverlayFiles)[file] {
		t.Fatalf("%q is not included in the linker overlay", file)
	}
}
