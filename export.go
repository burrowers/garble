// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
	"mvdan.cc/garble/internal/literals"
)

func commandExport(args []string) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	tags := flags.String("tags", "", "Build tags selecting the source to export")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: garble [garble flags] export [-tags=tags] directory packages")
		fmt.Fprintln(os.Stderr, "\nExport a single-configuration, pure-Go module for use with the normal Go toolchain.")
	}
	if err := flags.Parse(args); err != nil {
		return errJustExit(2)
	}
	if flags.NArg() < 2 {
		flags.Usage()
		return errJustExit(2)
	}
	if flagTiny || flagDebugDir != "" || flagControlFlow {
		return fmt.Errorf("export does not support -tiny, -debugdir or experimental control flow")
	}
	if os.Getenv("GOFLAGS") != "" {
		return fmt.Errorf("export requires an empty GOFLAGS; use export -tags to select build tags")
	}
	out, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("export destination already exists: %s", out)
	} else if !os.IsNotExist(err) {
		return err
	}
	if flagWordList != "" {
		data, err := os.ReadFile(flagWordList)
		if err != nil {
			return err
		}
		words, count, err := parseWordList(string(data))
		if err != nil {
			return err
		}
		sharedCache = &sharedCacheType{Words: words, WordCount: count}
	}
	buildFlags := []string{}
	if *tags != "" {
		buildFlags = append(buildFlags, "-tags="+*tags)
	}
	config := &packages.Config{
		Mode:       packages.NeedName | packages.NeedFiles | packages.NeedModule | packages.NeedImports | packages.NeedDeps,
		BuildFlags: buildFlags,
	}
	roots, err := packages.Load(config, flags.Args()[1:]...)
	if err != nil {
		return err
	}
	if err := exportPackageErrors(roots); err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("export: no packages matched")
	}
	module := roots[0].Module
	if module == nil {
		return fmt.Errorf("export requires packages in a Go module")
	}
	for _, pkg := range roots {
		if pkg.Module == nil || pkg.Module.Path != module.Path || pkg.Module.Dir != module.Dir {
			return fmt.Errorf("export requires packages from one module")
		}
	}
	// Build a module dependency closure without including third-party or runtime source.
	selected := make(map[string]bool)
	packages.Visit(roots, func(pkg *packages.Package) bool {
		if pkg.Module == nil || pkg.Module.Path != module.Path || pkg.Module.Dir != module.Dir {
			return false
		}
		selected[pkg.PkgPath] = true
		return true
	}, nil)
	paths := make([]string, 0, len(selected))
	for pkgPath := range selected {
		paths = append(paths, pkgPath)
	}
	slices.Sort(paths)
	config.Mode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedModule |
		packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedImports | packages.NeedEmbedFiles
	config.ParseFile = func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
		return parser.ParseFile(fset, filename, src, parser.ParseComments)
	}
	pkgs, err := packages.Load(config, paths...)
	if err != nil {
		return err
	}
	if err := exportPackageErrors(pkgs); err != nil {
		return err
	}
	slices.SortFunc(pkgs, func(a, b *packages.Package) int {
		return strings.Compare(a.PkgPath, b.PkgPath)
	})
	plan := &sourceExporter{module: module.Path, paths: make(map[string]string), embedded: make(map[*types.TypeName]bool)}
	for i, pkg := range pkgs {
		if pkg.Name == "main" {
			return fmt.Errorf("export requires libraries, not main package %s", pkg.PkgPath)
		}
		if len(pkg.OtherFiles) > 0 || !slices.Equal(pkg.GoFiles, pkg.CompiledGoFiles) {
			return fmt.Errorf("export: cgo and assembly are not supported in %s", pkg.PkgPath)
		}
		newPath := pkg.PkgPath
		rel := strings.TrimPrefix(pkg.PkgPath, module.Path)
		internalBoundaries := 0
		for _, part := range strings.Split(rel, "/") {
			if part == "internal" {
				internalBoundaries++
			}
		}
		if internalBoundaries > 1 {
			return fmt.Errorf("export does not support nested internal packages: %s", pkg.PkgPath)
		}
		if before, _, internal := strings.Cut(rel, "/internal/"); internal {
			// Keep the internal visibility boundary and the lexical package init order.
			newPath = fmt.Sprintf("%s%s/internal/p%06d_%s", module.Path, before, i, hashWithCustomSalt([]byte("export path"), pkg.PkgPath))
		}
		plan.paths[pkg.PkgPath] = newPath
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
					typ := pkg.TypesInfo.TypeOf(field.Type)
					if ptr, ok := typ.(*types.Pointer); ok {
						typ = ptr.Elem()
					}
					if named := namedType(typ); named != nil && named.Exported() {
						plan.embedded[named] = true
					}
				}
			}
		}
	}
	goMod, err := os.ReadFile(module.GoMod)
	if err != nil {
		return err
	}
	parsedMod, err := modfile.Parse(module.GoMod, goMod, nil)
	if err != nil {
		return err
	}
	for _, replace := range parsedMod.Replace {
		if replace.New.Version == "" {
			return fmt.Errorf("export does not support local module replacements: %s", replace.Old.Path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(out), ".export-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	moduleDir := filepath.Join(stage, filepath.FromSlash(module.Path))
	if err := os.MkdirAll(moduleDir, 0o777); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), goMod, 0o666); err != nil {
		return err
	}
	if data, err := os.ReadFile(filepath.Join(module.Dir, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(moduleDir, "go.sum"), data, 0o666); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, pkg := range pkgs {
		if err := plan.writePackage(stage, pkg); err != nil {
			return err
		}
		// A package may carry a license in any ancestor directory inside its module.
		for dir := pkg.Dir; ; dir = filepath.Dir(dir) {
			rel, err := filepath.Rel(module.Dir, dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("export: package outside module: %s", pkg.Dir)
			}
			if err := exportLicenses(dir, filepath.Join(moduleDir, rel)); err != nil {
				return err
			}
			if dir == module.Dir {
				break
			}
		}
	}
	cmd := exec.Command("go", append(append([]string{"build"}, buildFlags...), "./...")...)
	cmd.Dir = moduleDir
	cmd.Env = append(cmd.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building exported source: %w", err)
	}
	// Never merge with or overwrite a tree created while the export was running.
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("export destination already exists: %s", out)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(stage, out)
}

func exportPackageErrors(pkgs []*packages.Package) error {
	var messages []string
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, err := range pkg.Errors {
			messages = append(messages, err.Error())
		}
	})
	if len(messages) == 0 {
		return nil
	}
	slices.Sort(messages)
	return fmt.Errorf("loading export packages:\n%s", strings.Join(slices.Compact(messages), "\n"))
}

type sourceExporter struct {
	module   string
	paths    map[string]string
	embedded map[*types.TypeName]bool
}

func (e *sourceExporter) name(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil || obj.Name() == "_" {
		return ""
	}
	pkgPath, name := obj.Pkg().Path(), obj.Name()
	if _, ok := e.paths[pkgPath]; !ok {
		return ""
	}
	if _, ok := obj.(*types.PkgName); ok {
		return ""
	}
	if fn, ok := obj.(*types.Func); ok {
		if name == "init" && fn.Signature().Recv() == nil {
			return ""
		}
		// Exported methods must still implement untouched, possibly unknown interfaces.
		if fn.Signature().Recv() != nil && obj.Exported() {
			return ""
		}
	}
	if field, ok := obj.(*types.Var); ok && field.IsField() && obj.Exported() {
		return ""
	}
	if named, ok := obj.(*types.TypeName); ok && e.embedded[named] {
		return ""
	}
	rel := strings.TrimPrefix(pkgPath, e.module)
	internal := strings.Contains(rel+"/", "/internal/")
	if obj.Exported() && !internal {
		return ""
	}
	return hashWithCustomSalt([]byte("export object "+pkgPath+"|"), name)
}

func (e *sourceExporter) writePackage(root string, pkg *packages.Package) error {
	dir := filepath.Join(root, filepath.FromSlash(e.paths[pkg.PkgPath]))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	for i, file := range pkg.Syntax {
		exportComments(file)
		if flagLiterals {
			seed := sha256.Sum256(append([]byte(pkg.PkgPath+"|"+filepath.Base(pkg.CompiledGoFiles[i])), flagSeed.bytes...))
			rand := mathrand.New(mathrand.NewSource(int64(binary.BigEndian.Uint64(seed[:]))))
			file = literals.Obfuscate(rand, file, pkg.TypesInfo, nil, nil, nil, randomName)
			tf := transformer{pkg: pkg.Types, info: pkg.TypesInfo}
			tf.useAllImports(file)
		}
		for node := range ast.Preorder(file) {
			id, ok := node.(*ast.Ident)
			if !ok {
				continue
			}
			obj := pkg.TypesInfo.ObjectOf(id)
			if field, ok := obj.(*types.Var); ok && field.Embedded() {
				obj = namedType(field.Type())
			}
			if name := e.name(obj); name != "" {
				id.Name = name
			}
			// Type-switch declarations have one implicit object per case, not one Def.
			if obj == nil && id.Name != "_" {
				if _, defined := pkg.TypesInfo.Defs[id]; defined && id != file.Name {
					id.Name = hashWithCustomSalt([]byte("export object "+pkg.PkgPath+"|"), id.Name)
				}
			}
		}
		for _, imp := range file.Imports {
			oldPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if newPath, ok := e.paths[oldPath]; ok && newPath != oldPath {
				if imp.Name == nil {
					imp.Name = ast.NewIdent(pkg.TypesInfo.PkgNameOf(imp).Name())
				}
				imp.Path.Value = strconv.Quote(newPath)
			}
		}
		var printed bytes.Buffer
		if err := printer.Fprint(&printed, pkg.Fset, file); err != nil {
			return err
		}
		data, err := compactExportSource(printed.Bytes())
		if err != nil {
			return err
		}
		filename := filepath.Join(dir, filepath.Base(pkg.CompiledGoFiles[i]))
		if err := os.WriteFile(filename, data, 0o666); err != nil {
			return err
		}
	}
	for _, name := range pkg.EmbedFiles {
		rel, err := filepath.Rel(pkg.Dir, name)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("export: invalid embedded file: %s", name)
		}
		if strings.HasSuffix(name, ".go") {
			return fmt.Errorf("export cannot embed Go source: %s", name)
		}
		if err := exportCopyFile(name, filepath.Join(dir, rel)); err != nil {
			return err
		}
	}
	return nil
}

func exportComments(file *ast.File) {
	docs := make(map[*ast.CommentGroup]bool)
	docs[file.Doc] = true
	for node := range ast.Preorder(file) {
		switch node := node.(type) {
		case *ast.FuncDecl:
			if node.Name.IsExported() {
				docs[node.Doc] = true
			}
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if spec.Name.IsExported() {
						docs[node.Doc], docs[spec.Doc], docs[spec.Comment] = true, true, true
					}
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if name.IsExported() {
							docs[node.Doc], docs[spec.Doc], docs[spec.Comment] = true, true, true
						}
					}
				}
			}
		case *ast.Field:
			names := node.Names
			if len(names) == 0 {
				if name := exportEmbeddedName(node.Type); name != nil {
					names = []*ast.Ident{name}
				}
			}
			for _, name := range names {
				if name.IsExported() {
					docs[node.Doc], docs[node.Comment] = true, true
				}
			}
		}
	}
	kept := make([]*ast.CommentGroup, 0, len(file.Comments))
	for _, group := range file.Comments {
		header := group.End() < file.Package
		legal := false
		for _, comment := range group.List {
			lower := strings.ToLower(comment.Text)
			if strings.Contains(lower, "copyright") || strings.Contains(lower, "license") || strings.Contains(lower, "spdx-") {
				legal = true
				break
			}
		}
		var comments []*ast.Comment
		for _, comment := range group.List {
			text := comment.Text
			// Export is a snapshot of the selected build, not a set of alternative files.
			if strings.HasPrefix(text, "//go:build") || strings.HasPrefix(text, "// +build") ||
				strings.HasPrefix(text, "//line ") || strings.HasPrefix(text, "/*line ") {
				continue
			}
			if header || docs[group] || legal || isGoDirective(text) {
				comments = append(comments, comment)
			}
		}
		if len(comments) > 0 {
			kept = append(kept, &ast.CommentGroup{List: comments})
		}
	}
	file.Comments = kept
}

func exportEmbeddedName(expr ast.Expr) *ast.Ident {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr
	case *ast.SelectorExpr:
		return expr.Sel
	case *ast.StarExpr:
		return exportEmbeddedName(expr.X)
	case *ast.IndexExpr:
		return exportEmbeddedName(expr.X)
	case *ast.IndexListExpr:
		return exportEmbeddedName(expr.X)
	}
	return nil
}

// Explicit semicolons replace scanner-inserted newlines; retained line comments
// still need their own newline, particularly compiler directives and godoc.
func compactExportSource(src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file := fset.AddFile("export.go", -1, len(src))
	var scan scanner.Scanner
	var scanErr error
	scan.Init(file, src, func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%s: %s", pos, msg)
		}
	}, scanner.ScanComments)
	var out bytes.Buffer
	var previous token.Token
	previousEnd := 0
	for {
		pos, tok, lit := scan.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON {
			out.WriteString("; ")
			continue
		}
		if lit == "" {
			lit = tok.String()
		}
		if tok == token.COMMENT && out.Len() > 0 && out.Bytes()[out.Len()-1] != '\n' {
			out.WriteByte('\n')
		}
		offset := file.Offset(pos)
		if tok == token.COMMENT && previous == token.COMMENT && bytes.Count(src[previousEnd:offset], []byte("\n")) > 1 {
			out.WriteByte('\n')
		}
		previous, previousEnd = tok, offset+len(lit)
		out.WriteString(lit)
		if tok == token.COMMENT {
			out.WriteByte('\n')
		} else {
			out.WriteByte(' ')
		}
	}
	out.WriteByte('\n')
	return out.Bytes(), scanErr
}

func exportCopyFile(from, to string) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("export requires a regular file: %s", from)
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o777); err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o666)
}

func exportLicenses(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := strings.ToUpper(entry.Name())
		ext := filepath.Ext(name)
		if entry.IsDir() || (ext != "" && ext != ".TXT" && ext != ".MD" && ext != ".RST") {
			continue
		}
		base := strings.TrimSuffix(name, ext)
		legal := false
		for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "AUTHORS", "PATENTS"} {
			if base == prefix || strings.HasPrefix(base, prefix+"-") {
				legal = true
				break
			}
		}
		if !legal {
			continue
		}
		source, target := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
		// Ancestor licenses may be visited repeatedly, but must never replace source or assets.
		if existing, err := os.ReadFile(target); err == nil {
			original, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			if !bytes.Equal(existing, original) {
				return fmt.Errorf("export: license destination collision: %s", target)
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := exportCopyFile(source, target); err != nil {
			return err
		}
	}
	return nil
}
