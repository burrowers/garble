package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"

	"golang.org/x/tools/go/ast/astutil"
)

func isTypeDefStr(typ types.Type) bool {
	strType := types.Typ[types.String]

	if named, ok := typ.(*types.Named); ok {
		return types.Identical(named.Underlying(), strType)
	}

	return false
}

func containsTypeDefStr(expr ast.Expr, info *types.Info) bool {
	typ := info.TypeOf(expr)
	//log.Println(expr, typ, reflect.TypeOf(expr), reflect.TypeOf(typ))

	if sig, ok := typ.(*types.Signature); ok {
		for i := 0; i < sig.Params().Len(); i++ {
			if isTypeDefStr(sig.Params().At(i).Type()) {
				return true
			}
		}
	}

	if mapT, ok := typ.(*types.Map); ok {
		return isTypeDefStr(mapT.Elem()) || isTypeDefStr(mapT.Key())
	}

	if named, ok := typ.(*types.Named); ok {
		return isTypeDefStr(named)
	}

	return false
}

func obfuscateLiterals(files []*ast.File, info *types.Info) []*ast.File {
	pre := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.ValueSpec:
			return !containsTypeDefStr(x.Type, info)

		case *ast.AssignStmt:
			for _, expr := range x.Lhs {
				if index, ok := expr.(*ast.IndexExpr); ok {
					return !containsTypeDefStr(index.X, info)
				}

				if ident, ok := expr.(*ast.Ident); ok {
					return !containsTypeDefStr(ident, info)
				}
			}
		case *ast.CallExpr:
			return !containsTypeDefStr(x.Fun, info)

		case *ast.CompositeLit:
			if t, ok := x.Type.(*ast.MapType); ok {
				return !(containsTypeDefStr(t.Key, info) || containsTypeDefStr(t.Value, info))
			}

		case *ast.FuncDecl:
			if x.Type.Results == nil {
				return true
			}
			for _, result := range x.Type.Results.List {
				for _, name := range result.Names {
					return !containsTypeDefStr(name, info)
				}
			}

		case *ast.KeyValueExpr:
			if ident, ok := x.Key.(*ast.Ident); ok {
				return !containsTypeDefStr(ident, info)
			}
		case *ast.GenDecl:
			if x.Tok != token.CONST {
				return true
			}
			for _, spec := range x.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					return false
				}

				for _, val := range spec.Values {
					if v, ok := val.(*ast.BasicLit); !ok || v.Kind != token.STRING {
						return false // skip the block if it contains non basic literals
					}
				}

			}

			x.Tok = token.VAR
			// constants are not possible if we want to obfuscate literals, therefore
			// move all constant blocks which only contain strings to variables

		}
		return true
	}

	key := genAesKey()
	addedToPkg := false // we only want to inject the code and imports once
	post := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.File:
			if addedToPkg {
				break
			}
			x.Decls = append(x.Decls, funcStmt)
			x.Decls = append(x.Decls, keyStmt(key))
			astutil.AddImport(fset, x, "crypto/aes")
			astutil.AddImport(fset, x, "crypto/cipher")

			addedToPkg = true
		case *ast.BasicLit:
			switch cursor.Name() {
			case "Values", "Rhs", "Value", "Args":
			default:
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
	ciphertextLit := dataToByteSlice(ciphertext)

	return &ast.CallExpr{
		Fun:  &ast.Ident{Name: "garbleDecrypt"},
		Args: []ast.Expr{ciphertextLit},
	}
}

// dataToByteSlice turns a byte slice like []byte{1, 2, 3} into an AST
// expression
func dataToByteSlice(data []byte) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.ArrayType{
			Elt: &ast.Ident{Name: "byte"},
		},
		Args: []ast.Expr{&ast.BasicLit{
			Kind:  token.STRING,
			Value: fmt.Sprintf("%q", data),
		}},
	}
}

func keyStmt(key []byte) *ast.GenDecl {
	keyLit := dataToByteSlice(key)
	return &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{
			Names:  []*ast.Ident{{Name: "garbleKey"}},
			Values: []ast.Expr{keyLit},
		}},
	}
}
