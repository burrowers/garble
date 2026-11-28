// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"fmt"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

type exportTarget struct {
	GOOS, GOARCH string
}

type exportBuild struct {
	targets []exportTarget
	paths   []string
}

// discoverExportBuilds lists metadata, not export data: cross-target discovery
// must not invoke a compiler or require a cross-target standard library cache.
func discoverExportBuilds(patterns, buildFlags []string) (*packages.Module, []exportBuild, error) {
	env := append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
	runGo := func(args ...string) ([]byte, error) {
		cmd := exec.Command("go", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("export: go %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return out, nil
	}
	data, err := runGo("env", "-json", "GOOS", "GOARCH", "GOROOT")
	if err != nil {
		return nil, nil, err
	}
	var primary struct{ GOOS, GOARCH, GOROOT string }
	if err := json.Unmarshal(data, &primary); err != nil {
		return nil, nil, fmt.Errorf("export: decoding go env: %w", err)
	}
	data, err = runGo("tool", "dist", "list")
	if err != nil {
		return nil, nil, err
	}
	pairs := strings.Fields(string(data))
	slices.Sort(pairs)
	pairs = slices.Compact(pairs)
	var module *packages.Module
	var builds []exportBuild
	groups := make(map[string]int)
	dirs := make(map[string]string)
	for _, pair := range pairs {
		goos, goarch, ok := strings.Cut(pair, "/")
		if !ok || goos == "" || goarch == "" || strings.Contains(goarch, "/") {
			return nil, nil, fmt.Errorf("export: invalid go tool dist target %q", pair)
		}
		target := exportTarget{GOOS: goos, GOARCH: goarch}
		config := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedModule | packages.NeedImports | packages.NeedDeps | packages.NeedEmbedFiles,
			Env:  target.env(), BuildFlags: buildFlags,
		}
		roots, err := packages.Load(config, patterns...)
		if err != nil {
			return nil, nil, fmt.Errorf("export: target %s metadata: %w", pair, err)
		}
		roots, unsupported, err := exportTargetPackages(roots)
		if err != nil {
			return nil, nil, fmt.Errorf("export: target %s: %w", pair, err)
		}
		if unsupported || len(roots) == 0 {
			continue
		}
		for _, pkg := range roots {
			if pkg.Module == nil {
				return nil, nil, fmt.Errorf("export: target %s requires packages in a Go module: %s", pair, pkg.PkgPath)
			}
			if module == nil {
				module = pkg.Module
			}
			if pkg.Module.Path != module.Path || pkg.Module.Dir != module.Dir {
				return nil, nil, fmt.Errorf("export: target %s requires packages from one module: %s", pair, pkg.PkgPath)
			}
		}
		signature, paths, err := exportTargetSignature(target, roots, module, primary.GOROOT)
		if err != nil {
			return nil, nil, fmt.Errorf("export: target %s: %w", pair, err)
		}
		var dirError error
		packages.Visit(roots, func(pkg *packages.Package) bool {
			if pkg.Module != nil && pkg.Module.Path == module.Path {
				if pkg.Module.Dir != module.Dir {
					dirError = fmt.Errorf("module directory changed for %s: %s, %s", pkg.PkgPath, module.Dir, pkg.Module.Dir)
					return false
				}
				if previous, ok := dirs[pkg.PkgPath]; ok && previous != pkg.Dir {
					dirError = fmt.Errorf("package directory changed for %s: %s, %s", pkg.PkgPath, previous, pkg.Dir)
					return false
				}
				dirs[pkg.PkgPath] = pkg.Dir
			}
			return true
		}, nil)
		if dirError != nil {
			return nil, nil, fmt.Errorf("export: target %s: %w", pair, dirError)
		}
		index, ok := groups[signature]
		if !ok {
			index = len(builds)
			groups[signature] = index
			builds = append(builds, exportBuild{paths: paths})
		}
		build := &builds[index]
		if target.GOOS == primary.GOOS && target.GOARCH == primary.GOARCH {
			build.targets = append([]exportTarget{target}, build.targets...)
		} else {
			build.targets = append(build.targets, target)
		}
	}
	if len(builds) == 0 {
		return nil, nil, fmt.Errorf("export: no packages matched any supported target")
	}
	return module, builds, nil
}

func exportTargetSignature(target exportTarget, roots []*packages.Package, module *packages.Module, goroot string) (string, []string, error) {
	sizes := types.SizesFor("gc", target.GOARCH)
	if sizes == nil {
		return "", nil, fmt.Errorf("export: unknown type sizes for %s/%s", target.GOOS, target.GOARCH)
	}
	type sourcePackage struct {
		Path, Dir, Name                          string
		GoFiles, OtherFiles, EmbedFiles, Imports []string
	}
	signature := struct {
		IntSize, MaxAlign int64
		Paths             []string
		Packages          []sourcePackage
	}{IntSize: sizes.Sizeof(types.Typ[types.Int])}
	for _, basic := range types.Typ {
		if basic != nil && basic.Info()&types.IsUntyped == 0 && basic.Kind() != types.Invalid {
			signature.MaxAlign = max(signature.MaxAlign, sizes.Alignof(basic))
		}
	}
	sorted := func(values []string) []string { values = slices.Clone(values); slices.Sort(values); return values }
	var paths []string
	packages.Visit(roots, func(pkg *packages.Package) bool {
		if pkg.Module != nil && pkg.Module.Path == module.Path && pkg.Module.Dir == module.Dir {
			paths = append(paths, pkg.PkgPath)
		}
		rel, err := filepath.Rel(filepath.Join(goroot, "src"), pkg.Dir)
		if pkg.Module == nil && pkg.Dir != "" && err == nil && filepath.IsLocal(rel) {
			return false
		}
		source := sourcePackage{Path: pkg.PkgPath, Dir: pkg.Dir, Name: pkg.Name, GoFiles: sorted(pkg.GoFiles), OtherFiles: sorted(pkg.OtherFiles), EmbedFiles: sorted(pkg.EmbedFiles)}
		for path, imported := range pkg.Imports {
			source.Imports = append(source.Imports, path+"="+imported.PkgPath)
		}
		slices.Sort(source.Imports)
		signature.Packages = append(signature.Packages, source)
		return true
	}, nil)
	slices.SortFunc(signature.Packages, func(a, b sourcePackage) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(paths)
	signature.Paths = paths
	encoded, err := json.Marshal(signature)
	return string(encoded), paths, err
}

// exportTargetPackages distinguishes a platform-excluded root from a broken graph.
// Dependency exclusions make the target unsupported; all other errors are fatal,
// even when another package in the same graph is platform-excluded.
func exportTargetPackages(roots []*packages.Package) ([]*packages.Package, bool, error) {
	excluded := make(map[*packages.Package]bool)
	var messages []string
	packages.Visit(roots, nil, func(pkg *packages.Package) {
		for _, err := range pkg.Errors {
			absent := err.Kind == packages.ListError && strings.HasPrefix(err.Msg, "build constraints exclude all Go files in ")
			if err.Kind == packages.ListError && strings.HasPrefix(err.Msg, "no Go files in ") {
				for _, file := range pkg.IgnoredFiles {
					if strings.HasSuffix(file, ".go") {
						absent = true
						break
					}
				}
			}
			if absent {
				excluded[pkg] = true
			} else {
				messages = append(messages, pkg.PkgPath+": "+err.Error())
			}
		}
	})
	if len(messages) > 0 {
		slices.Sort(messages)
		return nil, false, fmt.Errorf("loading export metadata:\n%s", strings.Join(slices.Compact(messages), "\n"))
	}
	var selected []*packages.Package
	for _, pkg := range roots {
		if !excluded[pkg] {
			selected = append(selected, pkg)
		}
	}
	unsupported := false
	packages.Visit(selected, func(pkg *packages.Package) bool {
		if excluded[pkg] {
			unsupported = true
			return false
		}
		return true
	}, nil)
	return selected, unsupported, nil
}

func (t exportTarget) env() []string {
	return append(os.Environ(), "GOOS="+t.GOOS, "GOARCH="+t.GOARCH,
		"CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
}
