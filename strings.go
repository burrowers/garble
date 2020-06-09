package main

import (
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

func obfuscateLiterals(files []*ast.File) []*ast.File {
	pre := func(cursor *astutil.Cursor) bool {
		t, ok := cursor.Node().(*ast.GenDecl)
		if !ok {
			return true
		}

		// constants are not possibly if we want to obfuscate literals, therfore
		// remove all constants and replace them by variables
		if t.Tok == token.CONST {
			t.Tok = token.VAR
		}
		return true
	}

	var (
		key        = genAesKey()
		fset       = token.NewFileSet()
		addedToPkg bool // we only want to inject the code and imports once
	)
	post := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.File:
			if !addedToPkg {
				x.Decls = append(x.Decls, funcStmt)
				x.Decls = append(x.Decls, keyStmt(key))

				if x.Imports == nil {
					newDecls := []ast.Decl{cryptoAesImportSpec}
					newDecls = append(newDecls, x.Decls...)
					x.Decls = newDecls
				} else {
					astutil.AddImport(fset, x, "crypto/aes")
					astutil.AddImport(fset, x, "crypto/cipher")
				}

				addedToPkg = true
				return true
			}
		case *ast.BasicLit:
			if !(cursor.Name() == "Values" || cursor.Name() == "Rhs" || cursor.Name() == "Value" || cursor.Name() == "Args") {
				return true // we don't want to obfuscate imports etc.
			}
			if x.Kind != token.STRING {
				return true // TODO: garble literals other than strings
			}

			value, err := strconv.Unquote(x.Value)
			if err != nil {
				panic(fmt.Sprintf("cannot unquote string: %v", err))
			}

			ciphertext, err := encAES([]byte(value), key)
			if err != nil {
				panic(fmt.Sprintf("cannot encrypt string: %v", err))
			}

			cursor.Replace(ciphertextStmt(ciphertext))
		}

		return true
	}

	for i := range files {
		files[i] = astutil.Apply(files[i], pre, post).(*ast.File)
	}

	return files
}

// AST definitions for injection
var (
	aesCipherStmt = &ast.AssignStmt{
		Lhs: []ast.Expr{
			&ast.Ident{Name: "block"},
			&ast.Ident{Name: "err"},
		},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{&ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   &ast.Ident{Name: "aes"},
				Sel: &ast.Ident{Name: "NewCipher"},
			},
			Args: []ast.Expr{&ast.Ident{Name: "garbleKey"}},
		}},
	}

	aesGcmCipherStmt = &ast.AssignStmt{
		Lhs: []ast.Expr{
			&ast.Ident{Name: "aesgcm"},
			&ast.Ident{Name: "err"},
		},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{&ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   &ast.Ident{Name: "cipher"},
				Sel: &ast.Ident{Name: "NewGCM"},
			},
			Args: []ast.Expr{&ast.Ident{Name: "block"}},
		}},
	}

	plaintextStmt = &ast.AssignStmt{
		Lhs: []ast.Expr{
			&ast.Ident{Name: "plaintext"},
			&ast.Ident{Name: "err"},
		},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{&ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   &ast.Ident{Name: "aesgcm"},
				Sel: &ast.Ident{Name: "Open"},
			},
			Args: []ast.Expr{
				&ast.Ident{Name: "nil"},
				&ast.SliceExpr{
					X: &ast.Ident{Name: "ciphertext"},
					High: &ast.BasicLit{
						Kind:  token.INT,
						Value: "12",
					},
				},
				&ast.SliceExpr{
					X: &ast.Ident{Name: "ciphertext"},
					Low: &ast.BasicLit{
						Kind:  token.INT,
						Value: "12",
					},
				},
				&ast.Ident{Name: "nil"},
			},
		}},
	}

	returnStmt = &ast.ReturnStmt{Results: []ast.Expr{
		&ast.CallExpr{
			Fun:  &ast.Ident{Name: "string"},
			Args: []ast.Expr{&ast.Ident{Name: "plaintext"}},
		},
	}}
)

func decErrStmt() *ast.IfStmt {
	return &ast.IfStmt{
		Cond: &ast.BinaryExpr{
			X:  &ast.Ident{Name: "err"},
			Op: token.NEQ,
			Y:  &ast.Ident{Name: "nil"},
		},
		Body: &ast.BlockStmt{List: []ast.Stmt{
			&ast.ExprStmt{X: &ast.CallExpr{
				Fun: &ast.Ident{Name: "panic"},
				Args: []ast.Expr{&ast.BinaryExpr{
					X: &ast.BasicLit{
						Kind:  token.STRING,
						Value: `"garble: literal couldn't be decrypted: "`,
					},
					Op: token.ADD,
					Y: &ast.CallExpr{Fun: &ast.SelectorExpr{
						X:   &ast.Ident{Name: "err"},
						Sel: &ast.Ident{Name: "Error"},
					}},
				}},
			}},
		}},
	}
}

var funcStmt = &ast.FuncDecl{
	Name: &ast.Ident{Name: "garbleDecrypt"},
	Type: &ast.FuncType{
		Params: &ast.FieldList{List: []*ast.Field{{
			Names: []*ast.Ident{{Name: "ciphertext"}},
			Type: &ast.ArrayType{
				Elt: &ast.Ident{Name: "byte"},
			},
		}}},
		Results: &ast.FieldList{List: []*ast.Field{{
			Type: &ast.Ident{Name: "string"},
		}}},
	},
	Body: &ast.BlockStmt{List: []ast.Stmt{
		aesCipherStmt,
		decErrStmt(),
		aesGcmCipherStmt,
		decErrStmt(),
		plaintextStmt,
		decErrStmt(),
		returnStmt,
	}},
}

func ciphertextStmt(ciphertext []byte) *ast.CallExpr {
	ciphertextLit := dataAsByteSlice(ciphertext)

	return &ast.CallExpr{
		Fun:  &ast.Ident{Name: "garbleDecrypt"},
		Args: []ast.Expr{ciphertextLit},
	}
}

// dataAsByteSlice turns a byte slice like []byte{1, 2, 3} into an AST
// expression which encodes it, such as []byte("\x01\x02\x03").
func dataAsByteSlice(data []byte) *ast.CallExpr {
	var b strings.Builder

	b.WriteByte('"')
	hexstr := hex.EncodeToString(data)
	for i := 0; i < len(hexstr); i += 2 {
		b.WriteString("\\x" + hexstr[i:i+2])
	}
	b.WriteByte('"')

	return &ast.CallExpr{
		Fun: &ast.ArrayType{
			Elt: &ast.Ident{Name: "byte"},
		},
		Args: []ast.Expr{&ast.BasicLit{
			Kind:  token.STRING,
			Value: b.String(),
		}},
	}
}

func keyStmt(key []byte) *ast.GenDecl {
	keyLit := dataAsByteSlice(key)
	return &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{
			Names:  []*ast.Ident{{Name: "garbleKey"}},
			Values: []ast.Expr{keyLit},
		}},
	}
}

var cryptoAesImportSpec = &ast.GenDecl{
	Tok: token.IMPORT,
	Specs: []ast.Spec{
		&ast.ImportSpec{Path: &ast.BasicLit{
			Kind:  token.STRING,
			Value: `"crypto/aes"`,
		}},
		&ast.ImportSpec{Path: &ast.BasicLit{
			Kind:  token.STRING,
			Value: `"crypto/cipher"`,
		}},
	},
}
