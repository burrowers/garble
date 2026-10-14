// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strings"

	"mvdan.cc/garble/internal/literals"
)

// The compiler's embedcfg resolves each pattern to filenames and filenames to
// absolute paths. Read that same manifest rather than expanding globs ourselves.
type embedConfig struct {
	Patterns map[string][]string
	Files    map[string]string
}

func (tf *transformer) obfuscateEmbeds(files []*ast.File, flags []string) error {
	var cfg embedConfig
	cfgPath := flagValue(flags, "-embedcfg")
	if cfgPath == "" {
		for _, f := range files {
			for _, group := range f.Comments {
				for _, c := range group.List {
					if strings.HasPrefix(c.Text, "//go:embed ") {
						return fmt.Errorf("//go:embed without -embedcfg")
					}
				}
			}
		}
		return nil
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("invalid embedcfg: %w", err)
	}
	used := make(map[string]bool)
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, raw := range gen.Specs {
				spec := raw.(*ast.ValueSpec)
				doc := spec.Doc
				if doc == nil {
					doc = gen.Doc
				}
				if doc == nil {
					continue
				}
				var patterns []string
				for _, comment := range doc.List {
					if !strings.HasPrefix(comment.Text, "//go:embed ") {
						continue
					}
					words, err := cmdgoQuotedSplit(strings.TrimPrefix(comment.Text, "//go:embed "))
					if err != nil {
						return fmt.Errorf("invalid //go:embed: %w", err)
					}
					if len(words) == 0 {
						return fmt.Errorf("empty //go:embed directive")
					}
					patterns = append(patterns, words...)
				}
				if len(patterns) == 0 {
					continue
				}
				if len(spec.Names) != 1 || len(spec.Values) != 0 {
					return fmt.Errorf("unsupported //go:embed declaration")
				}
				obj := tf.info.Defs[spec.Names[0]]
				if obj == nil {
					return fmt.Errorf("missing type for embedded variable %s", spec.Names[0].Name)
				}
				typ := obj.Type().Underlying()
				isString := false
				switch t := typ.(type) {
				case *types.Basic:
					isString = t.Info()&types.IsString != 0
					if !isString {
						return fmt.Errorf("embed does not support %s (%s)", spec.Names[0].Name, obj.Type())
					}
				case *types.Slice:
					elem, ok := t.Elem().Underlying().(*types.Basic)
					if !ok || elem.Kind() != types.Byte {
						return fmt.Errorf("embed does not support %s (%s)", spec.Names[0].Name, obj.Type())
					}
				default:
					if !isEmbedFS(obj.Type()) {
						return fmt.Errorf("embed does not support %s (%s)", spec.Names[0].Name, obj.Type())
					}
					for _, pattern := range patterns {
						matches, ok := cfg.Patterns[pattern]
						if !ok || len(matches) == 0 {
							return fmt.Errorf("embed: no files for pattern %q", pattern)
						}
						for _, name := range matches {
							used[name] = true
						}
					}
					continue
				}
				var names []string
				for _, pattern := range patterns {
					matches, ok := cfg.Patterns[pattern]
					if !ok || len(matches) == 0 {
						return fmt.Errorf("embed: no files for pattern %q", pattern)
					}
					names = append(names, matches...)
				}
				if len(names) != 1 {
					return fmt.Errorf("embed: %s matches %d files; string and []byte require one", spec.Names[0].Name, len(names))
				}
				name := names[0]
				path, ok := cfg.Files[name]
				if !ok {
					return fmt.Errorf("embed: no file path for %q", name)
				}
				plain, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("embed: %q: %w", name, err)
				}
				// Defer decoder generation to the literal pass, which owns its proxy declarations.
				var expr ast.Expr = &ast.BasicLit{Kind: token.STRING, Value: `""`, ValuePos: spec.Names[0].Pos()}
				if tf.embeddedValues == nil {
					tf.embeddedValues = make(map[ast.Expr]literals.EmbeddedValue)
				}
				tf.embeddedValues[expr] = literals.EmbeddedValue{Data: plain, String: isString}
				if _, named := types.Unalias(obj.Type()).(*types.Named); named {
					expr = &ast.CallExpr{Fun: spec.Type, Args: []ast.Expr{expr}}
				}
				spec.Values = []ast.Expr{expr}
				// Keep comments attached to the AST, but remove only consumed directives.
				// The compiler must not attempt to embed plaintext beside our initializer.
				filtered := doc.List[:0]
				for _, comment := range doc.List {
					if !strings.HasPrefix(comment.Text, "//go:embed ") {
						filtered = append(filtered, comment)
					}
				}
				doc.List = filtered
				used[name] = true
			}
		}
	}
	for name := range cfg.Files {
		if !used[name] {
			return fmt.Errorf("embed: unhandled embedded file %q", name)
		}
	}
	return nil
}
