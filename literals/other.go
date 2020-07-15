package literals

import (
	"go/ast"
	"go/token"
)

func obfuscateString(data string) *ast.CallExpr {
	obfuscator := randObfuscator()
	block := obfuscator.Obfuscate([]byte(data))
	block.List = append(block.List, &ast.ReturnStmt{
		Results: []ast.Expr{&ast.CallExpr{
			Fun:  &ast.Ident{Name: "string"},
			Args: []ast.Expr{&ast.Ident{Name: "data"}},
		}},
	})

	return callExpr(&ast.Ident{Name: "string"}, block)
}

func obfuscateBytes(data []byte) *ast.CallExpr {
	obfuscator := randObfuscator()
	block := obfuscator.Obfuscate(data)
	block.List = append(block.List, &ast.ReturnStmt{
		Results: []ast.Expr{&ast.Ident{Name: "data"}},
	})
	return callExpr(&ast.ArrayType{Elt: &ast.Ident{Name: "byte"}}, block)
}

func obfuscateBool(data bool) *ast.BinaryExpr {
	var dataUint8 uint8 = 0
	if data {
		dataUint8 = 1
	}

	return &ast.BinaryExpr{
		X:  obfuscateUint8(dataUint8),
		Op: token.EQL,
		Y: &ast.BasicLit{
			Kind:  token.INT,
			Value: "1",
		},
	}
}
