// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"fmt"
	"go/build/constraint"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

type exportFileVariant struct {
	data    []byte
	targets map[exportTarget]bool
}

func writeExportBuilds(stage string, module *packages.Module, builds []exportLoadedBuild, plan *sourceExporter) error {
	allTargets := make(map[exportTarget]bool)
	files := make(map[string][]exportFileVariant)
	constraints := make(map[string]constraint.Expr)
	assets := make(map[string][]byte)
	for _, build := range builds {
		for _, target := range build.targets {
			allTargets[target] = true
		}
		group, err := os.MkdirTemp(stage, ".variants-")
		if err != nil {
			return err
		}
		for _, pkg := range build.packages {
			// Record constraints before comment filtering and literal rewriting mutate the AST.
			for _, filename := range pkg.CompiledGoFiles {
				source, err := os.ReadFile(filename)
				if err != nil {
					return err
				}
				expr, err := exportFileConstraint(source)
				if err != nil {
					return fmt.Errorf("export: %s: %w", filename, err)
				}
				name := filepath.Join(filepath.FromSlash(plan.paths[pkg.PkgPath]), filepath.Base(filename))
				constraints[name] = expr
			}
			if err := plan.writePackage(group, pkg); err != nil {
				return err
			}
			for dir := pkg.Dir; ; dir = filepath.Dir(dir) {
				rel, err := filepath.Rel(module.Dir, dir)
				if err != nil || !filepath.IsLocal(rel) {
					return fmt.Errorf("export: package outside module: %s", pkg.Dir)
				}
				if err := exportLicenses(dir, filepath.Join(group, filepath.FromSlash(module.Path), rel)); err != nil {
					return err
				}
				if dir == module.Dir {
					break
				}
			}
		}
		err = filepath.WalkDir(group, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(group, name)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			if _, source := constraints[rel]; !source {
				if previous, exists := assets[rel]; exists && !bytes.Equal(previous, data) {
					return fmt.Errorf("export: conflicting asset across targets: %s", rel)
				}
				assets[rel] = data
				return nil
			}
			variants := files[rel]
			for i := range variants {
				if bytes.Equal(variants[i].data, data) {
					for _, target := range build.targets {
						variants[i].targets[target] = true
					}
					files[rel] = variants
					return nil
				}
			}
			variant := exportFileVariant{data: data, targets: make(map[exportTarget]bool)}
			for _, target := range build.targets {
				variant.targets[target] = true
			}
			files[rel] = append(variants, variant)
			return nil
		})
		if err != nil {
			return err
		}
		if err := os.RemoveAll(group); err != nil {
			return err
		}
	}
	for name, data := range assets {
		if err := writeExportOutput(stage, name, data); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(files))
	splitPackages := make(map[string]bool)
	for name, variants := range files {
		names = append(names, name)
		if len(variants) > 1 {
			splitPackages[filepath.Dir(name)] = true
		}
	}
	slices.Sort(names)
	order := make(map[string]int)
	for _, name := range names {
		dir := filepath.Dir(name)
		index := order[dir]
		order[dir]++
		variants := files[name]
		slices.SortFunc(variants, func(a, b exportFileVariant) int { return bytes.Compare(a.data, b.data) })
		for i, variant := range variants {
			expr := constraints[name]
			if len(variant.targets) != len(allTargets) || splitPackages[dir] {
				targetExpr := exportTargetsConstraint(variant.targets)
				if expr == nil {
					expr = targetExpr
				} else {
					expr = &constraint.AndExpr{X: expr, Y: targetExpr}
				}
			}
			outputName := name
			if splitPackages[dir] {
				// Renumber the whole package when splitting a file, retaining lexical init order.
				outputName = filepath.Join(dir, fmt.Sprintf("f%06d_v%03d.go", index, i))
			}
			data := variant.data
			if expr != nil {
				const header = "// DO NOT EDIT: code obfuscated by garble\n\n"
				if !bytes.HasPrefix(data, []byte(header)) {
					return fmt.Errorf("export: missing generated header: %s", name)
				}
				data = append([]byte(header+"//go:build "+expr.String()+"\n\n"), data[len(header):]...)
			}
			if err := writeExportOutput(stage, outputName, data); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeExportOutput(root, name string, data []byte) error {
	if !filepath.IsLocal(name) {
		return fmt.Errorf("export: invalid output path: %s", name)
	}
	dest := filepath.Join(root, name)
	if existing, err := os.ReadFile(dest); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return fmt.Errorf("export: conflicting output: %s", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o666)
}

func exportFileHeader(source []byte) (legacy, modern []byte, err error) {
	// Follow go/build's parseFileHeader: legacy tags end at the last blank
	// line before any block comment or code; modern tags may follow block comments.
	// The original bytes matter because AST comments do not retain blank lines.
	end := 0
	ended, inBlock := false, false
Lines:
	for rest := bytes.TrimPrefix(source, []byte("\xef\xbb\xbf")); len(rest) > 0; {
		var line []byte
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			end = len(source) - len(rest)
			continue
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inBlock && constraint.IsGoBuild(string(line)) {
			if modern != nil {
				return nil, nil, fmt.Errorf("multiple //go:build comments")
			}
			modern = line
		}
		for len(line) > 0 {
			if inBlock {
				_, after, found := bytes.Cut(line, []byte("*/"))
				if !found {
					continue Lines
				}
				inBlock = false
				line = bytes.TrimSpace(after)
				continue
			}
			if bytes.HasPrefix(line, []byte("//")) {
				continue Lines
			}
			if bytes.HasPrefix(line, []byte("/*")) {
				inBlock = true
				line = bytes.TrimSpace(line[2:])
				continue
			}
			break Lines
		}
	}
	return source[:end], modern, nil
}

func exportFileConstraint(source []byte) (constraint.Expr, error) {
	header, modern, err := exportFileHeader(source)
	if err != nil {
		return nil, err
	}
	if modern != nil {
		return constraint.Parse(string(modern))
	}
	var legacy constraint.Expr
	for line := range bytes.SplitSeq(bytes.TrimPrefix(header, []byte("\xef\xbb\xbf")), []byte("\n")) {
		text := string(bytes.TrimSpace(line))
		if !constraint.IsPlusBuild(text) {
			continue
		}
		expr, err := constraint.Parse(text)
		if err != nil {
			continue // Like go/build, ignore malformed legacy constraints.
		}
		if legacy == nil {
			legacy = expr
		} else {
			legacy = &constraint.AndExpr{X: legacy, Y: expr}
		}
	}
	return legacy, nil
}

func exportTargetsConstraint(targets map[exportTarget]bool) constraint.Expr {
	pairs := make([]string, 0, len(targets))
	for target := range targets {
		pairs = append(pairs, target.GOOS+"/"+target.GOARCH)
	}
	slices.Sort(pairs)
	var result constraint.Expr
	for _, pair := range pairs {
		goos, goarch, _ := strings.Cut(pair, "/")
		var osExpr constraint.Expr = &constraint.TagExpr{Tag: goos}
		// Go's OS aliases must not make supposedly disjoint variants overlap.
		alias := map[string]string{"linux": "android", "darwin": "ios", "solaris": "illumos"}[goos]
		if alias != "" {
			osExpr = &constraint.AndExpr{X: osExpr, Y: &constraint.NotExpr{X: &constraint.TagExpr{Tag: alias}}}
		}
		var term constraint.Expr = &constraint.AndExpr{X: osExpr, Y: &constraint.TagExpr{Tag: goarch}}
		if result == nil {
			result = term
		} else {
			result = &constraint.OrExpr{X: result, Y: term}
		}
	}
	return result
}
