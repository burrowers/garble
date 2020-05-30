package main

import (
	"go/ast"
	"go/token"
	"log"
	"strconv"

	"golang.org/x/tools/go/ast/astutil"
)

func obfuscateStrings(files []*ast.File) []*ast.File {

	rmConst := func(cursor *astutil.Cursor) bool {
		node := cursor.Node()

		t, ok := node.(*ast.GenDecl)
		if !ok {
			return true
		}

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

	obfusStrings := func(cursor *astutil.Cursor) bool {
		node := cursor.Node()

		v, ok := node.(*ast.File)
		if ok && !addedToPkg {
			v.Decls = append(v.Decls, funcStmt())
			v.Decls = append(v.Decls, keyStmt(key))
			astutil.AddImport(fset, v, "crypto/aes") // TODO: this panics if file has no existing imports
			astutil.AddImport(fset, v, "crypto/cipher")

			addedToPkg = true

			return true
		}

		if !(cursor.Name() == "Values" || cursor.Name() == "Rhs") {
			return true // we don't want to obfuscate literals in Print Functions etc.
		}

		lit, ok := node.(*ast.BasicLit)
		if !ok {
			return true
		}

		if lit.Kind != token.STRING {
			return true // we only want to obfuscate strings for now
		}

		value := lit.Value

		ciphertext, err := encAes([]byte(value), key)
		if err != nil {
			log.Println("Could not encrypt string:", err)
			return true
		}

		cursor.Replace(ciphertextStmt(ciphertext))

		return true
	}

	for _, file := range files {
		file = astutil.Apply(file, rmConst, obfusStrings).(*ast.File)
	}

	return files
}

// ast definitions for injection
var (
	aesCipherStmt = &ast.AssignStmt{
		Lhs: []ast.Expr{
			&ast.Ident{
				Name: "block",
			},
			&ast.Ident{
				Name: "_",
			},
		},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{
			&ast.CallExpr{
				Fun: &ast.SelectorExpr{
					X: &ast.Ident{
						Name: "aes",
					},
					Sel: &ast.Ident{
						Name: "NewCipher",
					},
				},
				Args: []ast.Expr{
					&ast.Ident{
						Name: "garbleKey",
					},
				},
			},
		},
	}

	aesGcmCipherStmt = &ast.AssignStmt{
		Lhs: []ast.Expr{
			&ast.Ident{
				Name: "aesgcm",
			},
			&ast.Ident{
				Name: "_",
			},
		},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{
			&ast.CallExpr{
				Fun: &ast.SelectorExpr{
					X: &ast.Ident{
						Name: "cipher",
					},
					Sel: &ast.Ident{
						Name: "NewGCM",
					},
				},
				Args: []ast.Expr{
					&ast.Ident{
						Name: "block",
					},
				},
			},
		},
	}

	plaintextStmt = &ast.AssignStmt{
		Lhs: []ast.Expr{
			&ast.Ident{
				Name: "plaintext",
			},
			&ast.Ident{
				Name: "_",
			},
		},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{
			&ast.CallExpr{
				Fun: &ast.SelectorExpr{
					X: &ast.Ident{
						Name: "aesgcm",
					},
					Sel: &ast.Ident{
						Name: "Open",
					},
				},
				Args: []ast.Expr{
					&ast.Ident{
						Name: "nil",
					},
					&ast.SliceExpr{
						X: &ast.Ident{
							Name: "ciphertext",
						},
						High: &ast.BasicLit{
							Kind:  token.INT,
							Value: "12",
						},
						Slice3: false,
					},
					&ast.SliceExpr{
						X: &ast.Ident{
							Name: "ciphertext",
						},
						Low: &ast.BasicLit{
							Kind:  token.INT,
							Value: "12",
						},
						Slice3: false,
					},
					&ast.Ident{
						Name: "nil",
					},
				},
			},
		},
	}

	returnStmt = &ast.ReturnStmt{
		Results: []ast.Expr{
			&ast.CallExpr{
				Fun: &ast.Ident{
					Name: "string",
				},
				Args: []ast.Expr{
					&ast.Ident{
						Name: "plaintext",
					},
				},
			},
		},
	}
)

func funcStmt() *ast.FuncDecl {
	stmts := []ast.Stmt{
		aesCipherStmt,
		aesGcmCipherStmt,
		plaintextStmt,
		returnStmt,
	}

	return &ast.FuncDecl{
		Name: &ast.Ident{
			Name: "garbleDecrypt",
		},
		Type: &ast.FuncType{
			Params: &ast.FieldList{
				List: []*ast.Field{
					{
						Names: []*ast.Ident{
							{
								Name: "ciphertext",
							},
						},
						Type: &ast.ArrayType{
							Elt: &ast.Ident{
								Name: "byte",
							},
						},
					},
				},
			},
			Results: &ast.FieldList{
				List: []*ast.Field{
					{
						Type: &ast.Ident{
							Name: "string",
						},
					},
				},
			},
		},
		Body: &ast.BlockStmt{
			List: stmts,
		},
	}

}

func ciphertextStmt(ciphertext []byte) *ast.CallExpr {
	ciphertextLit := byteToByteLit(ciphertext)
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "garbleDecrypt",
		},
		Args: []ast.Expr{
			&ast.CompositeLit{
				Type: &ast.ArrayType{
					Elt: &ast.Ident{
						Name: "byte",
					},
				},
				Elts: ciphertextLit,
			},
		},
	}
}

func byteToByteLit(buffer []byte) []ast.Expr {

	var bufferInt []int
	var result []ast.Expr

	for _, c := range buffer {
		bufferInt = append(bufferInt, int(c))
	}

	for _, x := range bufferInt {

		result = append(result, &ast.BasicLit{
			Kind:  token.INT,
			Value: strconv.Itoa(x),
		})
	}

	return result
}

func keyStmt(key []byte) (decl *ast.GenDecl) {
	keyLit := byteToByteLit(key)

	decl = &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{
				Names: []*ast.Ident{
					{
						Name: "garbleKey",
					},
				},
				Values: []ast.Expr{
					&ast.CompositeLit{
						Type: &ast.ArrayType{
							Elt: &ast.Ident{
								Name: "byte",
							},
						},
						Elts:       keyLit,
						Incomplete: false,
					},
				},
			},
		},
	}

	return
}
