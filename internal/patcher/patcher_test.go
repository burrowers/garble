// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package patcher

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestTrimToolCache(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Now()
	recent := now.Add(-time.Hour)
	stale := now.Add(-toolTrimAge - time.Hour)
	for name, mtime := range map[string]time.Time{
		// In use; the version and lock files are as old as the last build.
		"compile-aaaa":         recent,
		"compile-aaaa.version": stale,
		"compile-aaaa.lock":    stale,
		// Unused, including a lock left by a failed build.
		"compile-bbbb":         stale,
		"compile-bbbb.version": stale,
		"compile-bbbb.lock":    stale,
		"link-cccc.lock":       stale,
		// Unversioned tools from older Garble versions go regardless of age.
		"link":         recent,
		"link.version": recent,
		"link.lock":    recent,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	trimToolCache(dir, now)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	want := []string{"compile-aaaa", "compile-aaaa.lock", "compile-aaaa.version"}
	if !slices.Equal(got, want) {
		t.Fatalf("kept %q, want %q", got, want)
	}
}

func TestMarkToolUsed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "compile-aaaa")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for _, test := range []struct {
		mtime, want time.Time
	}{
		{now.Add(-toolUsedInterval / 2), now.Add(-toolUsedInterval / 2)},
		{now.Add(-2 * toolUsedInterval), now},
	} {
		if err := os.Chtimes(path, test.mtime, test.mtime); err != nil {
			t.Fatal(err)
		}
		markToolUsed(path, test.mtime, now)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.ModTime(); !got.Equal(test.want) {
			t.Errorf("mtime %v became %v, want %v", test.mtime, got, test.want)
		}
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

func TestNormalizeGoRootKeepsSelectedToolchain(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "custom-module-cache", "golang.org", "toolchain@v0.0.1-go1.27.0.test-amd64")
	for path, content := range map[string]string{
		"bin/go":          "selected go command",
		"src/version.txt": "go1.27 selected source",
		"VERSION":         "go1.27.0",
	} {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o777); err != nil {
			t.Fatal(err)
		}
	}

	normalized, err := normalizeGoRoot(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if normalized == root {
		t.Fatal("module-cache GOROOT was not mirrored")
	}
	for path, want := range map[string]string{
		"bin/go":          "selected go command",
		"src/version.txt": "go1.27 selected source",
		"VERSION":         "go1.27.0",
	} {
		got, err := os.ReadFile(filepath.Join(normalized, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("mirrored %s = %q, want %q", path, got, want)
		}
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

func TestPatchedProductionFilesAreOverlaid(t *testing.T) {
	t.Parallel()

	_, _, patchFiles, _, _, _, err := loadToolchainPatches("go1.27")
	if err != nil {
		t.Fatal(err)
	}
	overlaid := makeFileSet(append(slices.Clone(compilerOverlayFiles), linkerOverlayFiles...))
	for file := range patchFiles {
		if strings.HasSuffix(file, "_test.go") {
			t.Errorf("toolchain patch contains dead test file %q", file)
			continue
		}
		if !overlaid[file] {
			t.Errorf("patched production file %q is absent from tool overlays", file)
		}
	}
}
