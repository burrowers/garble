// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/garble/internal/literals"
)

func isEmbedFS(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Name() == "FS" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "embed"
}

func (tf *transformer) obfuscateEmbedFiles(flags []string) ([]string, error) {
	cfgPath := flagValue(flags, "-embedcfg")
	if cfgPath == "" {
		return flags, nil
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var cfg embedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid embedcfg: %w", err)
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Files)) {
		path := cfg.Files[name]
		plain, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("embed: %q: %w", name, err)
		}
		encoded := literals.EncodeBlob(tf.obfRand, plain)
		// Use a basename independent of package-relative names, which may have slashes.
		hash := sha256.Sum256([]byte(name))
		newPath, err := tf.writeSourceFile(fmt.Sprintf("embed-%x", hash[:8]), fmt.Sprintf("embed-%x", hash[:8]), encoded)
		if err != nil {
			return nil, err
		}
		cfg.Files[name] = newPath
	}
	data, err = json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	newPath, err := tf.writeSourceFile("embedcfg.json", "embedcfg.json", data)
	if err != nil {
		return nil, err
	}
	return flagSetValue(flags, "-embedcfg", newPath), nil
}

// patchEmbedPackage adapts only methods that expose file contents or sizes.
// Keep embed.file's compiler-known layout and all path/directory logic intact.
// Assertions make Go-version layout changes fail at build time rather than leak ciphertext.
func patchEmbedPackage(file *ast.File) error {
	const helperTemplate = `package embed
const garbleEmbedMagic = EMBED_MAGIC
func garbleEmbedSize(s string) int {
 if len(s) >= len(garbleEmbedMagic)+32 && s[:len(garbleEmbedMagic)] == garbleEmbedMagic {
  return len(s)-len(garbleEmbedMagic)-32
 }
 return len(s)
}
func garbleEmbedDecode(s string) string {
 if len(s) < len(garbleEmbedMagic)+32 || s[:len(garbleEmbedMagic)] != garbleEmbedMagic { return s }
 return string(garbleBlobDecode(s))
}
func garbleBlobDecode(s string) []byte DECODE_BODY`
	helpSource := strings.Replace(helperTemplate, "EMBED_MAGIC", strconv.Quote(literals.BlobMagic), 1)
	helpSource = strings.Replace(helpSource, "DECODE_BODY", literals.BlobDecodeBody("s", "b", "key", "i"), 1)
	generated, err := parser.ParseFile(fset, "garble_embed.go", helpSource, 0)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				t, ok := spec.(*ast.TypeSpec)
				if !ok || t.Name.Name != "openFile" {
					continue
				}
				st, ok := t.Type.(*ast.StructType)
				if !ok || len(st.Fields.List) != 2 {
					return fmt.Errorf("unexpected embed.openFile layout")
				}
				st.Fields.List = append(st.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("data")}, Type: ast.NewIdent("string")})
				counts["struct"]++
			}
		case *ast.FuncDecl:
			if d.Recv == nil || len(d.Recv.List) != 1 || d.Body == nil {
				continue
			}
			recv, ok := d.Recv.List[0].Type.(*ast.StarExpr)
			var recvName string
			if ok {
				if id, ok := recv.X.(*ast.Ident); ok {
					recvName = id.Name
				}
			} else if id, ok := d.Recv.List[0].Type.(*ast.Ident); ok {
				recvName = id.Name
			}
			switch {
			case recvName == "file" && d.Name.Name == "Size":
				if len(d.Body.List) != 1 {
					return fmt.Errorf("unexpected embed.file.Size layout")
				}
				expr, _ := parser.ParseExpr("int64(garbleEmbedSize(f.data))")
				d.Body.List = []ast.Stmt{&ast.ReturnStmt{Results: []ast.Expr{expr}}}
				counts["size"]++
			case recvName == "FS" && d.Name.Name == "Open":
				ast.Inspect(d.Body, func(n ast.Node) bool {
					ret, ok := n.(*ast.ReturnStmt)
					if !ok || len(ret.Results) != 2 {
						return true
					}
					addr, ok := ret.Results[0].(*ast.UnaryExpr)
					if !ok || addr.Op != token.AND {
						return true
					}
					lit, ok := addr.X.(*ast.CompositeLit)
					if !ok {
						return true
					}
					id, ok := lit.Type.(*ast.Ident)
					if !ok || id.Name != "openFile" || len(lit.Elts) != 2 {
						return true
					}
					expr, _ := parser.ParseExpr("garbleEmbedDecode(file.data)")
					lit.Elts = append(lit.Elts, expr)
					counts["open"]++
					return false
				})
			case recvName == "FS" && d.Name.Name == "ReadFile":
				counts["readfile"] += replaceEmbedData(d.Body, "ofile")
			case recvName == "openFile" && (d.Name.Name == "Read" || d.Name.Name == "ReadAt" || d.Name.Name == "Seek"):
				counts[d.Name.Name] += replaceEmbedData(d.Body, "f")
			}
		}
	}
	for name, want := range map[string]int{"struct": 1, "size": 1, "open": 1, "readfile": 1, "Read": 2, "ReadAt": 2, "Seek": 2} {
		if counts[name] != want {
			return fmt.Errorf("unexpected embed.%s layout: matched %d, want %d", name, counts[name], want)
		}
	}
	file.Decls = append(file.Decls, generated.Decls...)
	return nil
}

func replaceEmbedData(root ast.Node, receiver string) int {
	count := 0
	ast.Inspect(root, func(n ast.Node) bool {
		outer, ok := n.(*ast.SelectorExpr)
		if !ok || outer.Sel.Name != "data" {
			return true
		}
		inner, ok := outer.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "f" {
			return true
		}
		id, ok := inner.X.(*ast.Ident)
		if !ok || id.Name != receiver {
			return true
		}
		outer.X = ast.NewIdent(receiver)
		count++
		return false
	})
	return count
}
