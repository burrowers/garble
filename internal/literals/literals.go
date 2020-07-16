package literals

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"strconv"

	"golang.org/x/tools/go/ast/astutil"
)

func callExpr(resultType ast.Expr, block *ast.BlockStmt) *ast.CallExpr {
	return &ast.CallExpr{Fun: &ast.FuncLit{
		Type: &ast.FuncType{
			Params: &ast.FieldList{},
			Results: &ast.FieldList{
				List: []*ast.Field{
					{Type: resultType},
				},
			},
		},
		Body: block,
	}}
}

func randObfuscator() obfuscator {
	randPos := mathrand.Intn(len(obfuscators))
	return obfuscators[randPos]
}

func returnStmt(result ast.Expr) *ast.ReturnStmt {
	return &ast.ReturnStmt{
		Results: []ast.Expr{result},
	}
}

// Obfuscate replace literals with obfuscated lambda functions
func Obfuscate(files []*ast.File, info *types.Info, fset *token.FileSet, blacklist map[types.Object]struct{}) []*ast.File {
	pre := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {

		case *ast.GenDecl:
			if x.Tok != token.CONST {
				return true
			}
			for _, spec := range x.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					return false
				}

				for _, name := range spec.Names {
					obj := info.ObjectOf(name)

					// The object itself is blacklisted, e.g. a value that needs to be constant
					if _, ok := blacklist[obj]; ok {
						return false
					}
				}

				for _, val := range spec.Values {
					if _, ok := val.(*ast.BasicLit); !ok {
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

	post := func(cursor *astutil.Cursor) bool {
		switch x := cursor.Node().(type) {
		case *ast.CompositeLit:
			byteType := types.Universe.Lookup("byte").Type()

			switch x := info.TypeOf(x.Type).(type) {
			case *types.Array:
				if x.Elem() != byteType {
					return true
				}
			case *types.Slice:
				if x.Elem() != byteType {
					return true
				}
			default:
				return true
			}

			var data []byte
			for _, el := range x.Elts {
				lit, ok := el.(*ast.BasicLit)
				if !ok {
					return true
				}

				value, err := strconv.Atoi(lit.Value)
				if err != nil {
					return true
				}

				data = append(data, byte(value))
			}

			cursor.Replace(obfuscateBytes(data))
		case *ast.BasicLit:
			switch cursor.Name() {
			case "Values", "Rhs", "Value", "Args", "X", "Y", "Results":
			default:
				return true // we don't want to obfuscate imports etc.
			}

			switch x.Kind {
			case token.STRING:
				typeInfo := info.TypeOf(x)
				if typeInfo != types.Typ[types.String] && typeInfo != types.Typ[types.UntypedString] {
					return true
				}
				value, err := strconv.Unquote(x.Value)
				if err != nil {
					panic(fmt.Sprintf("cannot unquote string: %v", err))
				}

				cursor.Replace(obfuscateString(value))
			}
		}

		return true
	}

	for i := range files {
		files[i] = astutil.Apply(files[i], pre, post).(*ast.File)
	}
	return files
}

func obfuscateString(data string) *ast.CallExpr {
	obfuscator := randObfuscator()
	block := obfuscator.obfuscate([]byte(data))
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
	block := obfuscator.obfuscate(data)
	block.List = append(block.List, &ast.ReturnStmt{
		Results: []ast.Expr{&ast.Ident{Name: "data"}},
	})
	return callExpr(&ast.ArrayType{Elt: &ast.Ident{Name: "byte"}}, block)
}

// ConstBlacklist blacklist identifieres used in constant expressions
func ConstBlacklist(node ast.Node, info *types.Info, blacklist map[types.Object]struct{}) {
	blacklistObjects := func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}

		obj := info.ObjectOf(ident)
		blacklist[obj] = struct{}{}

		return true
	}

	switch x := node.(type) {
	// in a slice or array composite literal all explicit keys must be constant representable
	case *ast.CompositeLit:
		if _, ok := x.Type.(*ast.ArrayType); !ok {
			break
		}
		for _, elt := range x.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				ast.Inspect(kv.Key, blacklistObjects)
			}
		}
	// in an array type the length must be a constant representable
	case *ast.ArrayType:
		if x.Len != nil {
			ast.Inspect(x.Len, blacklistObjects)
		}
	// in a const declaration all values must be constant representable
	case *ast.GenDecl:
		if x.Tok != token.CONST {
			break
		}
		for _, spec := range x.Specs {
			spec := spec.(*ast.ValueSpec)

			for _, val := range spec.Values {
				ast.Inspect(val, blacklistObjects)
			}
		}
	}
}
