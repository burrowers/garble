package literals

import (
	"go/ast"
	"go/token"
	mathrand "math/rand"
	"strconv"

	ah "mvdan.cc/garble/internal/asthelper"
)

const (
	// proxyValuesPerStruct sets the scaffold budget from actual occupancy;
	// one or two randomized extra structs keep small trees from being uniform.
	proxyValuesPerStruct = 64
	//minChildCount defines the minimum number of child elements that can be assigned to proxy structs.
	minChildCount = 1
	// maxChildCount defines the maximum number of child elements that can be assigned to proxy structs.
	maxChildCount = 3
	// minJunkValueCount defines the minimum number of junk values that can be added to a structure.
	minJunkValueCount = 1
	// maxJunkValueCount defines the maximum number of junk values that can be added to a structure.
	maxJunkValueCount = 3
	// minJunkArraySize defines the minimum size of a randomized byte array for junk data generation.
	minJunkArraySize = 1
	// maxJunkArraySize defines the maximum size of a randomized byte array for junk data generation.
	maxJunkArraySize = 8
	// maxProxyValuesPerRoot bounds the number of real hidden values in one
	// independently initialized proxy tree.
	maxProxyValuesPerRoot = 256
)

// proxyValue represents a named field with a type and value used in a proxy structure.
type proxyValue struct {
	name     string
	typ, val ast.Expr
	path     *ast.SelectorExpr
	clones   []*ast.SelectorExpr
}

type proxyStruct struct {
	typeName, name string
	isPointer      bool

	values []*proxyValue

	children []*proxyStruct
	parent   *proxyStruct
}

type proxyRoot struct {
	root           *proxyStruct
	flattenStructs []*proxyStruct
	hiddenCount    int
	pending        []*proxyValue
}

type proxyDispatcher struct {
	rand     *mathrand.Rand
	nameFunc NameProviderFunc

	roots        []*proxyRoot
	pendingPaths map[*ast.SelectorExpr]*proxyValue

	// Keep the conversion helper within the same retention boundary as its decoders.
	stringAdapter     ast.Expr
	stringAdapterType *ast.FuncType
}

func (d *proxyDispatcher) initialize(proxyRoot *proxyRoot, index int) {
	rootIndex := strconv.Itoa(index)
	base := (proxyRoot.hiddenCount + proxyValuesPerStruct - 1) / proxyValuesPerStruct
	flattenStructs := make([]*proxyStruct, base+d.rand.Intn(min(base, 2)+1))
	for i := range flattenStructs {
		flattenStructs[i] = &proxyStruct{
			typeName:  d.nameFunc(d.rand, "proxyRoot"+rootIndex+"StructName"+strconv.Itoa(i)),
			name:      d.nameFunc(d.rand, "proxyRoot"+rootIndex+"StructFieldName"+strconv.Itoa(i)),
			isPointer: d.rand.Intn(2) == 0,
		}
	}

	root := &proxyStruct{
		name:     d.nameFunc(d.rand, "proxyRoot"+rootIndex+"Name"),
		typeName: d.nameFunc(d.rand, "proxyRoot"+rootIndex+"Type"),
		parent:   nil,
	}

	unassigned := append([]*proxyStruct(nil), flattenStructs...)
	queue := []*proxyStruct{root}
	for len(unassigned) > 0 && len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		childCount := min(d.rand.Intn(maxChildCount-minChildCount)+minChildCount, len(unassigned))

		for i := 0; i < childCount; i++ {
			child := unassigned[0]
			unassigned = unassigned[1:]

			child.parent = current
			current.children = append(current.children, child)
			queue = append(queue, child)
		}
	}

	proxyRoot.root = root
	proxyRoot.flattenStructs = append(flattenStructs, root)
	for _, value := range proxyRoot.pending {
		strct := proxyRoot.flattenStructs[d.rand.Intn(len(proxyRoot.flattenStructs))]
		strct.values = append(strct.values, value)
		path := buildPath(strct, value.name).(*ast.SelectorExpr)
		// The caller positioned the placeholder before this subtree existed.
		// Preserve that site throughout the path so comments stay in place.
		withPos(path.X, value.path.Sel.Pos())
		value.path.X = path.X
		for _, clone := range value.clones {
			clone.X = cloneProxyPath(path.X)
			withPos(clone.X, clone.Sel.Pos())
		}
	}
	proxyRoot.pending = nil
}

// buildPath creates an AST expression that represents a field access path
// from the root of a nested struct hierarchy to a specific field.
// Example: root.child.grandchild.fieldName
func buildPath(strct *proxyStruct, valueName string) ast.Expr {
	var stack []*proxyStruct
	for s := strct; s != nil; s = s.parent {
		stack = append(stack, s)
	}

	var expr ast.Expr = ast.NewIdent(stack[len(stack)-1].name)
	for i := len(stack) - 2; i >= 0; i-- {
		expr = &ast.SelectorExpr{
			X:   expr,
			Sel: ast.NewIdent(stack[i].name),
		}
	}

	return &ast.SelectorExpr{
		X:   expr,
		Sel: ast.NewIdent(valueName),
	}
}

// cloneProxyPath keeps cached paths independent when callers assign positions.
func cloneProxyPath(expr ast.Expr) ast.Expr {
	switch expr := expr.(type) {
	case *ast.Ident:
		return ast.NewIdent(expr.Name)
	case *ast.SelectorExpr:
		return &ast.SelectorExpr{X: cloneProxyPath(expr.X), Sel: ast.NewIdent(expr.Sel.Name)}
	default:
		panic("unexpected proxy path")
	}
}

func (d *proxyDispatcher) HideValue(val, typ ast.Expr) ast.Expr {
	var root *proxyRoot
	if len(d.roots) > 0 {
		root = d.roots[len(d.roots)-1]
	}
	if root == nil || root.hiddenCount >= maxProxyValuesPerRoot {
		root = &proxyRoot{}
		d.roots = append(d.roots, root)
	}

	valueName := d.nameFunc(d.rand, "proxyRoot"+strconv.Itoa(len(d.roots)-1)+"Value"+strconv.Itoa(root.hiddenCount))
	// Callers embed this selector before occupancy is known. Keep its identity
	// and field position when the final randomized path is attached.
	path := &ast.SelectorExpr{X: ast.NewIdent("pendingProxy"), Sel: ast.NewIdent(valueName)}
	value := &proxyValue{
		name: valueName,
		typ:  typ,
		val:  val,
		path: path,
	}
	root.pending = append(root.pending, value)
	if d.pendingPaths == nil {
		d.pendingPaths = make(map[*ast.SelectorExpr]*proxyValue)
	}
	d.pendingPaths[path] = value
	root.hiddenCount++
	return path
}

// Cloned placeholders must receive the final path at their own source site.
func (d *proxyDispatcher) clonePath(expr ast.Expr) ast.Expr {
	clone := cloneProxyPath(expr)
	if selector, ok := expr.(*ast.SelectorExpr); ok {
		if value := d.pendingPaths[selector]; value != nil {
			path := clone.(*ast.SelectorExpr)
			value.clones = append(value.clones, path)
			d.pendingPaths[path] = value
		}
	}
	return clone
}

func (d *proxyDispatcher) generateStructLiteral(s *proxyStruct) ast.Expr {
	var fields []ast.Expr
	for _, child := range s.children {
		expr := d.generateStructLiteral(child)
		if child.isPointer {
			expr = &ast.UnaryExpr{
				Op: token.AND,
				X:  expr,
			}
		}
		fields = append(fields, &ast.KeyValueExpr{
			Key:   ast.NewIdent(child.name),
			Value: expr,
		})
	}

	for _, val := range s.values {
		fields = append(fields, &ast.KeyValueExpr{
			Key:   ast.NewIdent(val.name),
			Value: val.val,
		})
	}

	d.rand.Shuffle(len(fields), func(i, j int) {
		fields[i], fields[j] = fields[j], fields[i]
	})

	return &ast.CompositeLit{
		Type: ast.NewIdent(s.typeName),
		Elts: fields,
	}
}

// junkValue generates and returns a proxyValue containing a randomized byte array of variable size
func (d *proxyDispatcher) junkValue() *proxyValue {
	size := d.rand.Intn(maxJunkArraySize-minJunkArraySize+1) + minJunkArraySize
	data := make([]byte, size)
	d.rand.Read(data)

	dummyDataExpr := &ast.CompositeLit{
		Type: &ast.ArrayType{
			Len: ah.IntLit(size),
			Elt: ast.NewIdent("byte"),
		},
		Elts: ah.DataToArray(data).Elts,
	}
	return &proxyValue{
		name: d.nameFunc(d.rand, "junkValue"),
		typ: &ast.ArrayType{
			Len: ah.IntLit(size),
			Elt: ast.NewIdent("byte"),
		},
		val: dummyDataExpr,
	}
}

func (d *proxyDispatcher) AddToFile(file *ast.File) {
	if len(d.roots) == 0 {
		return
	}

	for index, root := range d.roots {
		d.initialize(root, index)
		for _, strct := range root.flattenStructs {
			dummyCount := d.rand.Intn(maxJunkValueCount-minJunkValueCount+1) + minJunkValueCount
			for range dummyCount {
				strct.values = append(strct.values, d.junkValue())
			}

			structType := &ast.StructType{Fields: &ast.FieldList{}}
			for _, child := range strct.children {
				var typ ast.Expr = ast.NewIdent(child.typeName)
				if child.isPointer {
					typ = &ast.StarExpr{X: typ}
				}
				structType.Fields.List = append(structType.Fields.List, &ast.Field{
					Names: []*ast.Ident{ast.NewIdent(child.name)},
					Type:  typ,
				})
			}

			for _, value := range strct.values {
				structType.Fields.List = append(structType.Fields.List, &ast.Field{
					Names: []*ast.Ident{ast.NewIdent(value.name)},
					Type:  value.typ,
				})
			}

			d.rand.Shuffle(len(structType.Fields.List), func(i, j int) {
				structType.Fields.List[i], structType.Fields.List[j] = structType.Fields.List[j], structType.Fields.List[i]
			})

			file.Decls = append(file.Decls, &ast.GenDecl{
				Tok: token.TYPE,
				Specs: []ast.Spec{&ast.TypeSpec{
					Name: ast.NewIdent(strct.typeName),
					Type: structType,
				}},
			})
		}

		file.Decls = append(file.Decls, &ast.GenDecl{
			Tok: token.VAR,
			Specs: []ast.Spec{&ast.ValueSpec{
				Names:  []*ast.Ident{ast.NewIdent(root.root.name)},
				Values: []ast.Expr{d.generateStructLiteral(root.root)},
			}},
		})
	}
}

func newProxyDispatcher(rand *mathrand.Rand, nameFunc NameProviderFunc) *proxyDispatcher {
	return &proxyDispatcher{
		rand:     rand,
		nameFunc: nameFunc,
	}
}
