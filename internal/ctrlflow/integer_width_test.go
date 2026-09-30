package ctrlflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"testing"

	"golang.org/x/tools/go/ssa/ssautil"
)

func TestLiveIntegerWidths(t *testing.T) {
	const src = `package main
type Word uint32
//garble:controlflow flatten_passes=0 live_integers=1
func compute(a,b uint64) uint64 { return a+b }
//garble:controlflow flatten_passes=0 live_integers=1
func named(a,b Word) Word { return a+b }
//garble:controlflow flatten_passes=0 live_integers=1
func small(a,b uint16) uint16 { return a+b }
func main() {
 vals:=[]uint64{0,1,255,65535,4294967295,9223372036854775808,18446744073709551615}
 for _,x:=range vals { for _,y:=range vals {
  if compute(x,y)!=x+y || named(Word(x),Word(y))!=Word(x)+Word(y) || small(uint16(x),uint16(y))!=uint16(x)+uint16(y) { panic("width") }
 } }
}
`
	runExperiment(t, renderExperiment(t, src, 42))
}

func TestLiveIntegerExclusions(t *testing.T) {
	const src = `package main
func signed(x,y int32) int32 { return x+y }
func floating(x,y float64) float64 { return x+y }
func variable(x,y uint) uint { return x+y }
func division(x,y uint32) uint32 { return x/y }
`
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "main.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("test/main", "main"), []*ast.File{file}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"signed", "floating", "variable", "division"} {
		fn := pkg.Func(name)
		before := len(fn.Blocks[0].Instrs)
		substituteLiveIntegers(fn, mathrand.New(mathrand.NewSource(1)))
		if len(fn.Blocks[0].Instrs) != before {
			t.Fatalf("transformed excluded %s", name)
		}
	}
}
