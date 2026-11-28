package main

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

const reflectFlowSource = `package flow
import "reflect"

func aEntry(x, y any) { bMiddle(x, y) }
func bMiddle(x, y any) { zLeaf(x, y) }
func zLeaf(x, y any) { reflect.TypeOf(y) }

func aGeneric[T, U any]() { zGeneric[U, T]() }
func bInstantiate() { aGeneric[GenericKept, GenericUnused]() }
func zGeneric[T, U any]() { reflect.TypeFor[U]() }
type GenericKept struct{ Label int }
type GenericUnused struct{ Unused int }

func bClosure(x, y uint8, choose bool) {
	f := func(a, b uint8) { zClosureSink(b) }
	if choose { f = func(a, b uint8) {} }
	f(x, y)
}
func zClosureSink(x uint8) { reflect.TypeOf(x) }

type Dispatch interface{ Consume(a, b uint16) }
func aFallback(x, y uint16, f func(uint16, uint16)) { f(x, y) }
func bExact(x, y uint16, d Dispatch) { d.Consume(x, y) }
func zSignature(x, y uint16) { reflect.TypeOf(y) }

type Earlier interface{ Take(a, b uint64) }
type EarlierImpl struct{}
func (EarlierImpl) Take(x, y uint64) { zEarlySink(x) }
func aEarlier(x, y uint64, d Earlier) { d.Take(x, y) }
func zEarlySink(x uint64) { reflect.TypeOf(x) }
func zEarlySignature(x, y uint64) { reflect.TypeOf(y) }

type Later interface{ Take(a, b uint32) }
type LaterImpl struct{}
func (LaterImpl) Take(x, y uint32) { zLateForward(x) }
func aLater(x, y uint32, d Later) { d.Take(x, y) }
func zLateForward(x uint32) { zzLateSink(x) }
func zLateSignature(x, y uint32) { reflect.TypeOf(y) }
func zzLateSink(x uint32) { reflect.TypeOf(x) }

type Box struct{ Item any }
type Stored struct{ Kept int }
var shared Box
func aStore() { shared = Box{Item: Stored{}} }
type Original struct{ Field int }
type Converted Original
func bConvert(x Original) Converted { return Converted(x) }
func zObjects() { reflect.TypeFor[Box](); reflect.TypeFor[Original]() }
`

func reflectFlowFixture(tb testing.TB, source string) *ssa.Package {
	tb.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "flow.go", source, 0)
	if err != nil {
		tb.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("test/flow", "flow"), []*ast.File{file}, 0)
	if err != nil {
		tb.Fatal(err)
	}
	return pkg
}

func setupReflectFlowCache(tb testing.TB) {
	oldCache := sharedCache
	tb.Cleanup(func() { sharedCache = oldCache })
	sharedCache = &sharedCacheType{BinaryContentID: []byte("reflect dataflow test"), ListedPackages: newListedPackages()}
}

func reflectFlowInspector(pkg *ssa.Package) *reflectInspector {
	consume := pkg.Pkg.Scope().Lookup("Dispatch").Type().Underlying().(*types.Interface).Method(0)
	return &reflectInspector{
		pkg:                  pkg.Pkg,
		lpkg:                 &listedPackage{ImportPath: pkg.Pkg.Path()},
		propagatedInstr:      make(map[ssa.Instruction]bool),
		walkedInstantiations: make(map[types.Type]bool),
		result: pkgCache{
			ReflectAPIs: map[string]map[int]bool{
				"reflect.TypeOf":         {0: true},
				reflectFuncName(consume): {0: true},
			},
			ReflectedObjects:   make(map[string]bool),
			ReflectObjectNames: make(map[string]string),
		},
	}
}

func TestReflectWorklistOrder(t *testing.T) {
	a := reflectFact{kind: reflectAPIFact, name: "a"}
	b := reflectFact{kind: reflectTypeParamFact, name: "b"}
	c := reflectFact{kind: reflectObjectFact, name: "c"}
	work := reflectWorklist{watchers: map[reflectFact][]int{a: {0}, b: {1}, c: {0}}}
	var order []int
	var ready bool
	work.run(4, func(i int) {
		order = append(order, i)
		switch {
		case i == 0 && ready:
			ready = false
			work.changed(b)
		case i == 1 && len(order) > 4:
			work.changed(c)
		case i == 2:
			ready = true
			work.changed(a)
			work.changed(a)
		}
	})
	if diff := cmp.Diff([]int{0, 1, 2, 3, 0, 1, 0}, order); diff != "" {
		t.Fatalf("visit order (-want +got):\n%s", diff)
	}
}

func TestReflectDataflow(t *testing.T) {
	setupReflectFlowCache(t)
	pkg := reflectFlowFixture(t, reflectFlowSource)
	ri := reflectFlowInspector(pkg)
	ri.recordReflection(pkg)
	for name, want := range map[string]map[int]bool{
		"aEntry":    {1: true},
		"bMiddle":   {1: true},
		"bClosure":  {1: true},
		"aFallback": {1: true},
		"bExact":    {0: true},
		"aEarlier":  {0: true},          // The named fact wins before the signature is read.
		"aLater":    {0: true, 1: true}, // A later named fact does not retract position 1.
	} {
		if diff := cmp.Diff(want, ri.result.ReflectAPIs[pkg.Pkg.Path()+"."+name]); diff != "" {
			t.Errorf("%s parameters (-want +got):\n%s", name, diff)
		}
	}
	wantTypeParams := map[string]map[int]bool{
		"test/flow.aGeneric": {0: true},
		"test/flow.zGeneric": {1: true},
	}
	if diff := cmp.Diff(wantTypeParams, ri.result.ReflectTypeParams); diff != "" {
		t.Errorf("type parameters (-want +got):\n%s", diff)
	}
	names := slices.Collect(maps.Values(ri.result.ReflectObjectNames))
	for name, want := range map[string]bool{
		"Stored": true, "Converted": true, "GenericKept": true, "GenericUnused": false,
	} {
		if got := slices.Contains(names, name); got != want {
			t.Errorf("preserves %s: got %t, want %t", name, got, want)
		}
	}
}

func BenchmarkReflectDataflow(b *testing.B) {
	setupReflectFlowCache(b)
	for _, config := range []struct {
		name             string
		chain, unrelated int
	}{
		{"fixture", 0, 0}, {"sparse", 8, 256}, {"reverse-chain", 128, 0}, {"no-new-facts", 0, 256},
	} {
		b.Run(config.name, func(b *testing.B) {
			var source strings.Builder
			if config.name == "no-new-facts" {
				source.WriteString("package flow\ntype Dispatch interface{ Consume(a, b uint16) }\n")
			} else {
				source.WriteString(reflectFlowSource)
			}
			source.WriteString("\nfunc sink(x int) {}\n")
			for i := range config.chain {
				callee := fmt.Sprintf("chain%03d", i+1)
				if i == config.chain-1 {
					callee = "zLeaf"
				}
				fmt.Fprintf(&source, "\nfunc chain%03d(x,y any) { %s(x,y) }\n", i, callee)
			}
			for i := range config.unrelated {
				fmt.Fprintf(&source, "\nfunc unrelated%03d(x int) { sink(x) }\n", i)
			}
			pkg := reflectFlowFixture(b, source.String())
			b.ReportAllocs()
			for b.Loop() {
				reflectFlowInspector(pkg).recordReflection(pkg)
			}
		})
	}
}
