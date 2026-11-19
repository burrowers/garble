// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

type exportLoadedBuild struct {
	exportBuild
	packages []*packages.Package
}

func loadExportPlan(patterns, buildFlags []string) (*packages.Module, []exportLoadedBuild, *sourceExporter, error) {
	module, builds, err := discoverExportBuilds(patterns, buildFlags)
	if err != nil {
		return nil, nil, nil, err
	}
	plan := &sourceExporter{module: module.Path, paths: make(map[string]string), embedded: make(map[string]bool)}
	var loaded []exportLoadedBuild
	selected := make(map[string]bool)
	for _, build := range builds {
		config := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedModule |
				packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedImports | packages.NeedEmbedFiles,
			BuildFlags: buildFlags,
			Env:        build.targets[0].env(),
			ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
				return parser.ParseFile(fset, filename, src, parser.ParseComments)
			},
		}
		pkgs, err := packages.Load(config, build.paths...)
		if err == nil {
			err = exportPackageErrors(pkgs)
		}
		if err != nil {
			target := build.targets[0]
			return nil, nil, nil, fmt.Errorf("export %s/%s: %w", target.GOOS, target.GOARCH, err)
		}
		slices.SortFunc(pkgs, func(a, b *packages.Package) int { return strings.Compare(a.PkgPath, b.PkgPath) })
		for _, pkg := range pkgs {
			if pkg.Module == nil || pkg.Module.Path != module.Path || pkg.Module.Dir != module.Dir {
				return nil, nil, nil, fmt.Errorf("export requires packages from one module")
			}
			if err := plan.checkPackage(pkg); err != nil {
				return nil, nil, nil, err
			}
			selected[pkg.PkgPath] = true
		}
		loaded = append(loaded, exportLoadedBuild{exportBuild: build, packages: pkgs})
	}
	paths := make([]string, 0, len(selected))
	for pkgPath := range selected {
		paths = append(paths, pkgPath)
	}
	slices.Sort(paths)
	for i, pkgPath := range paths {
		newPath := pkgPath
		rel := strings.TrimPrefix(pkgPath, module.Path)
		if before, _, internal := strings.Cut(rel, "/internal/"); internal {
			// One sorted union keeps names and initialization order stable across targets.
			newPath = fmt.Sprintf("%s%s/internal/p%06d_%s", module.Path, before, i, hashWithCustomSalt([]byte("export path"), pkgPath))
		}
		plan.paths[pkgPath] = newPath
	}
	return module, loaded, plan, nil
}

func (e *sourceExporter) checkPackage(pkg *packages.Package) error {
	if pkg.Name == "main" {
		return fmt.Errorf("export requires libraries, not main package %s", pkg.PkgPath)
	}
	if len(pkg.OtherFiles) > 0 || !slices.Equal(pkg.GoFiles, pkg.CompiledGoFiles) {
		return fmt.Errorf("export: cgo and assembly are not supported in %s", pkg.PkgPath)
	}
	boundaries := 0
	for _, part := range strings.Split(strings.TrimPrefix(pkg.PkgPath, e.module), "/") {
		if part == "internal" {
			boundaries++
		}
	}
	if boundaries > 1 {
		return fmt.Errorf("export does not support nested internal packages: %s", pkg.PkgPath)
	}
	for _, file := range pkg.Syntax {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				directive := strings.Fields(comment.Text)
				if len(directive) > 0 && (directive[0] == "//go:linkname" || directive[0] == "//go:linknamestd") {
					return fmt.Errorf("export does not support %s directives in %s", strings.TrimPrefix(directive[0], "//"), pkg.PkgPath)
				}
			}
		}
		for node := range ast.Preorder(file) {
			if field, ok := node.(*ast.Field); ok && len(field.Names) == 0 {
				if named := namedType(pkg.TypesInfo.TypeOf(field.Type)); named != nil && named.Exported() {
					e.embedded[exportTypeIdentity(named)] = true
				}
			}
		}
	}
	return nil
}

func exportTypeIdentity(named *types.TypeName) string {
	if named.Pkg() == nil {
		return named.Name()
	}
	return named.Pkg().Path() + "." + named.Name()
}
