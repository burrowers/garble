package ctrlflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func regionFixture(t *testing.T) *ssa.Function {
	t.Helper()
	const src = `package test
func compute(n int) int {
 a,b:=n,n+1
 if n&1==0 { a+=3; b-=2 } else { a-=2; b+=3 }
 if n&2==0 { a*=2; b+=5 } else { a+=5; b*=2 }
 for i:=0;i<n%8;i++ { a+=i; b-=i }
 if n&4==0 { a-=7; b+=11 } else { a+=11; b-=7 }
 if n&8==0 { a^=b } else { b^=a }
 return a*b
}`
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "test.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("test", "test"), []*ast.File{f}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p.Func("compute")
}

func TestConnectedRegionInvariants(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34} {
		fn := regionFixture(t)
		regions := selectPartialRegions(fn, rand.New(rand.NewSource(seed)))
		if len(regions) != 2 {
			t.Fatalf("seed %d selected %d regions", seed, len(regions))
		}
		used := make(map[*ssa.BasicBlock]bool)
		for _, r := range regions {
			if len(r.blocks) > 4 || len(r.edges) < 2 {
				t.Fatal("region budget violated")
			}
			seen := make(map[*ssa.BasicBlock]bool)
			var visit func(*ssa.BasicBlock)
			visit = func(b *ssa.BasicBlock) {
				if seen[b] || !r.blocks[b] {
					return
				}
				seen[b] = true
				for _, s := range b.Succs {
					visit(s)
				}
			}
			visit(r.root)
			if len(seen) != len(r.blocks) {
				t.Fatal("disconnected region")
			}
			for b := range r.blocks {
				if used[b] {
					t.Fatal("overlapping regions")
				}
				used[b] = true
				for _, s := range b.Succs {
					if reaches(s, b, make(map[*ssa.BasicBlock]bool)) {
						t.Fatal("cycle block selected")
					}
				}
				if b != r.root {
					for _, p := range b.Preds {
						if !r.blocks[p] {
							t.Fatal("second entry")
						}
					}
				}
			}
		}
		var cycleEdges []regionEdge
		for _, b := range fn.Blocks {
			for i, s := range b.Succs {
				if reaches(s, b, make(map[*ssa.BasicBlock]bool)) {
					cycleEdges = append(cycleEdges, regionEdge{b, s, i})
				}
			}
		}
		applyPartialFlattening(fn, rand.New(rand.NewSource(seed)))
		for _, e := range cycleEdges {
			if e.from.Succs[e.index] != e.target {
				t.Fatal("loop edge routed")
			}
		}
	}
}

func TestConnectedRegionExecution(t *testing.T) {
	const src = `package main
//garble:controlflow flatten_passes=1 flatten_regions=2
func compute(n int) int {
 a,b:=n,n+1
 if n&1==0 { a+=3; b-=2 } else { a-=2; b+=3 }
 if n&2==0 { a*=2; b+=5 } else { a+=5; b*=2 }
 for i:=0;i<n%8;i++ { a+=i; b-=i }
 if n&4==0 { a-=7; b+=11 } else { a+=11; b-=7 }
 if n&8==0 { a^=b } else { b^=a }
 return a*b
}
func reference(n int) int {
 a,b:=n,n+1
 if n&1==0 { a+=3; b-=2 } else { a-=2; b+=3 }
 if n&2==0 { a*=2; b+=5 } else { a+=5; b*=2 }
 for i:=0;i<n%8;i++ { a+=i; b-=i }
 if n&4==0 { a-=7; b+=11 } else { a+=11; b-=7 }
 if n&8==0 { a^=b } else { b^=a }
 return a*b
}
func main(){for n:=-64;n<64;n++{if compute(n)!=reference(n){panic(n)}}}
`
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34} {
		runExperiment(t, renderExperiment(t, src, seed))
	}
}
