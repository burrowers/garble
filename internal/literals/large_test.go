// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"strings"
	"testing"
)

func TestLargeLiteral(t *testing.T) {
	value := strings.Repeat("LARGE_LITERAL_SECRET_194", 4096)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "large.go", "package p; var s = "+fmt.Sprintf("%q", value), 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object)}
	if _, err := new(types.Config).Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	file = Obfuscate(mathrand.New(mathrand.NewSource(123)), file, info, nil, nil, func(r *mathrand.Rand, base string) string { return fmt.Sprintf("%s%d", base, r.Uint64()) })
	var src bytes.Buffer
	if err := printer.Fprint(&src, fset, file); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src.String(), "LARGE_LITERAL_SECRET_194") {
		t.Fatal("large literal left in plaintext")
	}
	count := 0
	for range ast.Preorder(file) {
		count++
	}
	if count > 2000 {
		t.Fatalf("large literal expanded into %d AST nodes", count)
	}
}

func TestBlobEncoding(t *testing.T) {
	for _, size := range []int{0, 1, MinSize - 1, MaxSize, MaxSize + 1, 1 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			plain := bytes.Repeat([]byte{0, 255, 42}, (size+2)/3)[:size]
			encoded := EncodeBlob(mathrand.New(mathrand.NewSource(123)), plain)
			if !bytes.Equal(encoded, EncodeBlob(mathrand.New(mathrand.NewSource(123)), plain)) {
				t.Fatal("encoding is not deterministic")
			}
			if string(encoded[:len(BlobMagic)]) != BlobMagic || len(encoded) != BlobHeaderSize+size {
				t.Fatal("incorrect blob header")
			}
			key := encoded[len(BlobMagic):BlobHeaderSize]
			decoded := bytes.Clone(encoded[BlobHeaderSize:])
			for i := range decoded {
				decoded[i] ^= key[i%len(key)]
			}
			if !bytes.Equal(decoded, plain) {
				t.Fatal("decoded contents differ")
			}
		})
	}
}
