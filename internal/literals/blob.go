// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"fmt"
	"go/ast"
	"go/parser"
	mathrand "math/rand"
	"strconv"

	ah "mvdan.cc/garble/internal/asthelper"
)

// BlobMagic distinguishes encoded embed.FS assets from unselected dependencies.
const BlobMagic = "\x00garble:literal:v1\x00"
const BlobHeaderSize = len(BlobMagic) + 32

// EncodeBlob stores the key beside the data. This is obfuscation, not encryption.
// Its linear byte cost and bounded AST size allow megabyte-sized assets.
func EncodeBlob(rand *mathrand.Rand, plain []byte) []byte {
	encoded := make([]byte, BlobHeaderSize+len(plain))
	copy(encoded, BlobMagic)
	key := encoded[len(BlobMagic):BlobHeaderSize]
	rand.Read(key)
	for i, b := range plain {
		encoded[BlobHeaderSize+i] = b ^ key[i%len(key)]
	}
	return encoded
}

// BlobDecodeBody is shared by the literal decoder and the embed.FS overlay.
// Callers supply final names when creating code after identifier typechecking.
func BlobDecodeBody(input, data, key, index string) string {
	return fmt.Sprintf(`{
 if len(%[1]s) < %[5]d || %[1]s[:%[6]d] != %[7]s { return []byte(%[1]s) }
 %[3]s := %[1]s[%[6]d:%[5]d]
 %[2]s := []byte(%[1]s[%[5]d:])
 for %[4]s := range %[2]s { %[2]s[%[4]s] ^= %[3]s[%[4]s %% len(%[3]s)] }
 return %[2]s[:len(%[2]s):len(%[2]s)]
}`, input, data, key, index, BlobHeaderSize, len(BlobMagic), strconv.Quote(BlobMagic))
}

type blob struct{}

func (blob) obfuscate(rand *mathrand.Rand, names *generatedNames, data []byte, _ []*externalKey) *ast.BlockStmt {
	input := names.name("blobInput")
	source := "func(" + input + " string) []byte " + BlobDecodeBody(input, names.name("blobData"), names.name("blobKey"), names.name("blobIndex"))
	expr, err := parser.ParseExpr(source)
	if err != nil {
		panic(err)
	}
	decoder := expr.(*ast.FuncLit)
	return ah.BlockStmt(ah.AssignDefineStmt(names.ident("data"), ah.CallExpr(decoder, ah.StringLit(string(EncodeBlob(rand, data))))))
}
