// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestExportTargetSignature(t *testing.T) {
	module := &packages.Module{Path: "example.com/m", Dir: filepath.Join(t.TempDir(), "module")}
	goroot := filepath.Join(t.TempDir(), "go")
	makeGraph := func() []*packages.Package {
		std := &packages.Package{PkgPath: "runtime", Dir: filepath.Join(goroot, "src", "runtime"), GoFiles: []string{"runtime_linux.go"}}
		dep := &packages.Package{PkgPath: "example.com/dep", Dir: "/dependency", Module: &packages.Module{Path: "example.com/dep", Dir: "/dependency"}, GoFiles: []string{"dep.go"}}
		child := &packages.Package{PkgPath: module.Path + "/internal/child", Dir: filepath.Join(module.Dir, "internal/child"), Module: module, GoFiles: []string{"child.go"}}
		root := &packages.Package{PkgPath: module.Path, Dir: module.Dir, Name: "m", Module: module, GoFiles: []string{"b.go", "a.go"}, OtherFiles: []string{"b.s", "a.s"}, EmbedFiles: []string{"b.txt", "a.txt"}, Imports: map[string]*packages.Package{"runtime": std, dep.PkgPath: dep, child.PkgPath: child}}
		return []*packages.Package{root, child}
	}
	target := exportTarget{GOOS: "linux", GOARCH: "amd64"}
	baseline, paths, err := exportTargetSignature(target, makeGraph(), module, goroot)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{module.Path, module.Path + "/internal/child"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for _, tt := range []struct {
		name   string
		mutate func([]*packages.Package)
		target exportTarget
		equal  bool
	}{
		{name: "standard-library source ignored", mutate: func(p []*packages.Package) { p[0].Imports["runtime"].GoFiles = []string{"runtime_windows.go"} }, target: exportTarget{GOOS: "windows", GOARCH: "amd64"}, equal: true},
		{name: "file ordering ignored", mutate: func(p []*packages.Package) {
			slices.Reverse(p[0].GoFiles)
			slices.Reverse(p[0].OtherFiles)
			slices.Reverse(p[0].EmbedFiles)
			slices.Reverse(p)
		}, target: target, equal: true},
		{name: "equivalent architecture sizes", target: exportTarget{GOOS: "linux", GOARCH: "arm64"}, equal: true},
		{name: "32-bit architecture", target: exportTarget{GOOS: "linux", GOARCH: "386"}},
		{name: "module membership", mutate: func(p []*packages.Package) { p[1].Module = nil }, target: target},
		{name: "32-bit ARM architecture", target: exportTarget{GOOS: "linux", GOARCH: "arm"}},
		{name: "module source", mutate: func(p []*packages.Package) { p[0].GoFiles[0] = "windows.go" }, target: target},
		{name: "dependency source", mutate: func(p []*packages.Package) { p[0].Imports["example.com/dep"].GoFiles = []string{"dep_windows.go"} }, target: target},
		{name: "dependency without module", mutate: func(p []*packages.Package) {
			p[0].Imports["example.com/dep"].Module = nil
			p[0].Imports["example.com/dep"].GoFiles = []string{"vendor_windows.go"}
		}, target: target},
		{name: "embed assets", mutate: func(p []*packages.Package) { p[0].EmbedFiles[0] = "other.txt" }, target: target},
		{name: "assembly source", mutate: func(p []*packages.Package) { p[0].OtherFiles[0] = "other.s" }, target: target},
		{name: "imports", mutate: func(p []*packages.Package) { delete(p[0].Imports, "runtime") }, target: target},
		{name: "module closure", mutate: func(p []*packages.Package) { delete(p[0].Imports, module.Path+"/internal/child"); p[1] = p[0] }, target: target},
	} {
		t.Run(tt.name, func(t *testing.T) {
			graph := makeGraph()
			if tt.mutate != nil {
				tt.mutate(graph)
			}
			got, _, err := exportTargetSignature(tt.target, graph, module, goroot)
			if err != nil {
				t.Fatal(err)
			}
			if (got == baseline) != tt.equal {
				t.Fatalf("signature equality = %v, want %v", got == baseline, tt.equal)
			}
		})
	}
}

func TestExportTargetPackages(t *testing.T) {
	excluded := packages.Error{Kind: packages.ListError, Msg: "build constraints exclude all Go files in /module/only"}
	for _, tt := range []struct {
		name                      string
		errors                    []packages.Error
		ignored                   []string
		dependency, absent, fatal bool
	}{
		{name: "excluded root", errors: []packages.Error{excluded}, absent: true},
		{name: "excluded dependency", errors: []packages.Error{excluded}, dependency: true, absent: true},
		{name: "ignored Go files", errors: []packages.Error{{Kind: packages.ListError, Msg: "no Go files in /module/only"}}, ignored: []string{"only_windows.go"}, absent: true},
		{name: "empty directory", errors: []packages.Error{{Kind: packages.ListError, Msg: "no Go files in /module/only"}}, fatal: true},
		{name: "missing import", errors: []packages.Error{{Kind: packages.ListError, Msg: "no required module provides package missing.example/pkg"}}, dependency: true, fatal: true},
		{name: "syntax error", errors: []packages.Error{{Kind: packages.ParseError, Msg: "expected 'package', found 'EOF'"}}, fatal: true},
		{name: "constraint message wrong error kind", errors: []packages.Error{{Kind: packages.ParseError, Msg: excluded.Msg}}, fatal: true},
		{name: "constraint message substring", errors: []packages.Error{{Kind: packages.ListError, Msg: "invalid metadata: " + excluded.Msg}}, fatal: true},
		{name: "exclusion must not hide error", errors: []packages.Error{excluded, {Kind: packages.ListError, Msg: "missing import"}}, absent: true, fatal: true},
		{name: "valid root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pkg := &packages.Package{PkgPath: "example.com/only", Errors: tt.errors, IgnoredFiles: tt.ignored}
			roots := []*packages.Package{pkg}
			if tt.dependency {
				roots = []*packages.Package{{PkgPath: "example.com/root", Imports: map[string]*packages.Package{pkg.PkgPath: pkg}}}
			}
			selected, unsupported, err := exportTargetPackages(roots)
			if (err != nil) != tt.fatal {
				t.Fatalf("error = %v, want fatal %v", err, tt.fatal)
			}
			if err != nil {
				return
			}
			if tt.dependency && tt.absent {
				if !unsupported {
					t.Fatal("excluded dependency did not mark target unsupported")
				}
				return
			}
			if unsupported {
				t.Fatal("unexpected unsupported target")
			}
			if (len(selected) == 0) != tt.absent {
				t.Fatalf("selected roots = %v, want absent %v", selected, tt.absent)
			}
		})
	}
	// An absent root must not discard another matched package.
	keep := &packages.Package{PkgPath: "example.com/keep"}
	selected, unsupported, err := exportTargetPackages([]*packages.Package{{PkgPath: "example.com/only", Errors: []packages.Error{excluded}}, keep})
	if err != nil || unsupported || len(selected) != 1 || selected[0] != keep {
		t.Fatalf("mixed roots: %v, %v, %v", selected, unsupported, err)
	}
	excludedDependency := &packages.Package{PkgPath: "example.com/only", Errors: []packages.Error{excluded}}
	importingRoot := &packages.Package{PkgPath: "example.com/root", Imports: map[string]*packages.Package{excludedDependency.PkgPath: excludedDependency}}
	_, unsupported, err = exportTargetPackages([]*packages.Package{importingRoot, excludedDependency})
	if err != nil || !unsupported {
		t.Fatalf("explicit excluded root is also a dependency: unsupported=%v, err=%v", unsupported, err)
	}
}

func TestDiscoverExportBuilds(t *testing.T) {
	dir := t.TempDir()
	write := func(name, source string) {
		t.Helper()
		name = filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.27\nrequire example.com/dep v0.0.0\nreplace example.com/dep => ./dependency\n")
	write("root.go", "package m\nimport (_ \"runtime\"; _ \"example.com/dep\")\nvar MustNotCompile nonexistent\n")
	write("root_linux.go", "package m\nimport _ \"example.com/m/internal/linuxonly\"\n")
	write("internal/linuxonly/only_linux.go", "package linuxonly\n")
	write("publicwin/only_windows.go", "package publicwin\n")
	write("feature/feature.go", "//go:build exportfeature\n\npackage feature\n")
	write("dependency/go.mod", "module example.com/dep\n\ngo 1.27\n")
	write("dependency/dep.go", "package dep\n")
	write("dependency/dep_solaris.go", "package dep\nconst Variant = true\n")
	t.Chdir(dir)
	t.Setenv("GOFLAGS", "-this-must-be-cleared")
	t.Setenv("GOWORK", filepath.Join(dir, "nonexistent.work"))
	module, builds, err := discoverExportBuilds([]string{"./..."}, []string{"-tags=exportfeature"})
	if err != nil {
		t.Fatal(err)
	}
	if module.Path != "example.com/m" || module.Dir != dir {
		t.Fatalf("module = %+v", module)
	}
	cmd := exec.Command("go", "tool", "dist", "list")
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	supported := strings.Fields(string(out))
	slices.Sort(supported)
	primaryCmd := exec.Command("go", "env", "GOOS", "GOARCH")
	primaryCmd.Env = cmd.Env
	out, err = primaryCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	primary := strings.Join(strings.Fields(string(out)), "/")
	var covered []string
	for _, build := range builds {
		if !slices.IsSorted(build.paths) {
			t.Fatalf("unsorted paths %v", build.paths)
		}
		if !slices.Contains(build.paths, "example.com/m/feature") {
			t.Fatalf("build flags lost: %v", build.paths)
		}
		for _, target := range build.targets {
			pair := target.GOOS + "/" + target.GOARCH
			covered = append(covered, pair)
			if pair == primary && build.targets[0] != target {
				t.Fatalf("primary %s is not representative: %v", primary, build.targets)
			}
			if slices.Contains(build.paths, "example.com/m/publicwin") != (target.GOOS == "windows") {
				t.Fatalf("windows-only pattern expansion lost for %s: %v", pair, build.paths)
			}
			if slices.Contains(build.paths, "example.com/m/internal/linuxonly") != (target.GOOS == "linux" || target.GOOS == "android") {
				t.Fatalf("linux closure incorrect for %s: %v", pair, build.paths)
			}
		}
	}
	slices.Sort(covered)
	if !slices.Equal(covered, supported) {
		t.Fatalf("covered %v, supported %v", covered, supported)
	}
	// Root graph, platform-only dependency, and type sizes each split configurations;
	// platform-specific standard library source must not force one group per target.
	if len(builds) >= len(supported)/2 || len(builds) < 6 {
		t.Fatalf("unexpected grouping: %d targets, %d groups", len(supported), len(builds))
	}
	t.Logf("discovered %d targets in %d metadata groups", len(covered), len(builds))
}

func TestDiscoverExportBuildsExcludedRoot(t *testing.T) {
	dir := t.TempDir()
	write := func(name, source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/only\n\ngo 1.27\n")
	write("only_windows.go", "package only\nimport _ \"missing.invalid/pkg\"\n")
	t.Chdir(dir)
	t.Setenv("GOPROXY", "off")
	_, _, err := discoverExportBuilds([]string{"."}, nil)
	if err == nil || !strings.Contains(err.Error(), "target windows/") || !strings.Contains(err.Error(), "missing.invalid/pkg") {
		t.Fatalf("platform-specific missing import must be fatal with target diagnostics: %v", err)
	}
	write("only_windows.go", "package only\n")
	module, builds, err := discoverExportBuilds([]string{"."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if module.Path != "example.com/only" || len(builds) == 0 {
		t.Fatalf("missing platform-only root: %v, %v", module, builds)
	}
	count := 0
	for _, build := range builds {
		for _, target := range build.targets {
			count++
			if target.GOOS != "windows" {
				t.Fatalf("included absent target %v", target)
			}
		}
	}
	t.Logf("platform-only root: %d targets in %d metadata groups", count, len(builds))
}

func TestExportTargetEnv(t *testing.T) {
	t.Setenv("GOOS", "host-os")
	t.Setenv("GOARCH", "host-arch")
	t.Setenv("GOFLAGS", "-bad")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("GOWORK", "bad.work")
	t.Setenv("EXPORT_TARGET_SENTINEL", "kept")
	target := exportTarget{GOOS: "linux", GOARCH: "arm64"}
	cmd := exec.Command("go", "env", "GOOS", "GOARCH", "CGO_ENABLED", "GOWORK", "GOFLAGS")
	cmd.Env = target.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go env: %v\n%s", err, out)
	}
	if string(out) != "linux\narm64\n0\noff\n\n" {
		t.Fatalf("unexpected environment: %q", out)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "EXPORT_TARGET_SENTINEL=kept") {
		t.Fatal("lost inherited environment")
	}
}
