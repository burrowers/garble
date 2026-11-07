package ctrlflow

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	mathrand "math/rand"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

type attackValue struct {
	n     int64
	known bool
}

// The attack knows original block boundaries, but learns state keys from instructions.
func attackEval(v ssa.Value, env map[ssa.Value]attackValue) attackValue {
	if n, ok := env[v]; ok {
		return n
	}
	switch v := v.(type) {
	case *ssa.Const:
		n, ok := constant.Int64Val(v.Value)
		return attackValue{n, ok}
	case *ssa.BinOp:
		x, y := attackEval(v.X, env), attackEval(v.Y, env)
		if !x.known || !y.known {
			return attackValue{}
		}
		switch v.Op {
		case token.XOR:
			return attackValue{x.n ^ y.n, true}
		case token.EQL:
			if x.n == y.n {
				return attackValue{1, true}
			}
			return attackValue{0, true}
		}
	}
	return attackValue{}
}

type attackNode struct {
	block *ssa.BasicBlock
	state int64
}
type attackEdge struct{ from, to *ssa.BasicBlock }

func recoverStateEdges(fn *ssa.Function, real map[*ssa.BasicBlock]bool) (map[attackEdge]bool, error) {
	phi, ok := fn.Blocks[0].Instrs[0].(*ssa.Phi)
	if !ok {
		return nil, fmt.Errorf("entry is not a dispatcher Phi")
	}
	route := func(from, start *ssa.BasicBlock, state int64) (attackNode, error) {
		env := map[ssa.Value]attackValue{phi: {state, true}}
		seen := make(map[attackNode]bool)
		pred, block := from, start
		for !real[block] {
			node := attackNode{block, env[phi].n}
			if seen[node] {
				return attackNode{}, fmt.Errorf("synthetic cycle")
			}
			seen[node] = true
			for _, instr := range block.Instrs {
				switch instr := instr.(type) {
				case *ssa.Phi:
					idx := -1
					for i, p := range block.Preds {
						if p == pred {
							idx = i
							break
						}
					}
					if idx < 0 || idx >= len(instr.Edges) {
						return attackNode{}, fmt.Errorf("missing Phi predecessor")
					}
					value := attackEval(instr.Edges[idx], env)
					if !value.known {
						return attackNode{}, fmt.Errorf("unknown state")
					}
					env[instr] = value
				case *ssa.BinOp:
					value := attackEval(instr, env)
					if !value.known {
						return attackNode{}, fmt.Errorf("unknown synthetic calculation")
					}
					env[instr] = value
				}
			}
			var next *ssa.BasicBlock
			switch end := block.Instrs[len(block.Instrs)-1].(type) {
			case *ssa.Jump:
				next = block.Succs[0]
			case *ssa.If:
				cond := attackEval(end.Cond, env)
				if !cond.known {
					return attackNode{}, fmt.Errorf("unknown synthetic condition")
				}
				if cond.n != 0 {
					next = block.Succs[0]
				} else {
					next = block.Succs[1]
				}
			default:
				return attackNode{}, fmt.Errorf("unsupported synthetic terminator")
			}
			pred, block = block, next
		}
		return attackNode{block, env[phi].n}, nil
	}
	entry, err := route(fn.Blocks[0], fn.Blocks[0].Succs[0], 0)
	if err != nil {
		return nil, err
	}
	queue := []attackNode{entry}
	seen := make(map[attackNode]bool)
	recovered := make(map[attackEdge]bool)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if seen[node] {
			continue
		}
		seen[node] = true
		if len(seen) > len(real)*4 {
			return nil, fmt.Errorf("state-space budget exceeded")
		}
		for _, succ := range node.block.Succs {
			dst, err := route(node.block, succ, node.state)
			if err != nil {
				return nil, err
			}
			recovered[attackEdge{node.block, dst.block}] = true
			queue = append(queue, dst)
		}
	}
	return recovered, nil
}

func attackFixture(t *testing.T, body string) *ssa.Function {
	t.Helper()
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "attack.go", "package attack\nfunc compute(n int) int {"+body+"}", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("attack", "attack"), []*ast.File{f}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p.Func("compute")
}

func TestStatePropagationAttack(t *testing.T) {
	fixtures := []string{
		"if n<0 { return -n }; if n<8 { return n+3 }; return n*2",
		"s:=0; for i:=0;i<n;i++ { if i&1==0 { s+=i } else { s-=i } }; return s",
		"a,b:=1,2; if n&1==0 { a=n; b=n+1 } else { a=n+2; b=n+3 }; return a*b",
		"s:=0; for i:=0;i<n;i++ { for j:=0;j<i;j++ { if j==3 { break }; s+=j }; if i==9 { continue }; s+=i }; return s",
		"switch n { case 0: return 4; case 1,2: return n+7; case 6: return 9; default: return n*3 }",
		"s:=0; for i:=0;i<n;i++ { if i>10 { return s }; s+=i }; return s+1",
	}
	total := 0
	for i, fixture := range fixtures {
		for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34} {
			for _, dependent := range []bool{false, true} {
				fn := attackFixture(t, fixture)
				real := make(map[*ssa.BasicBlock]bool)
				want := make(map[attackEdge]bool)
				for _, b := range fn.Blocks {
					real[b] = true
					for _, s := range b.Succs {
						want[attackEdge{b, s}] = true
					}
				}
				applyFlattening(fn, mathrand.New(mathrand.NewSource(seed)), dependent)
				got, err := recoverStateEdges(fn, real)
				if err != nil {
					t.Fatalf("fixture %d seed %d dependent %v: %v", i, seed, dependent, err)
				}
				if len(got) != len(want) {
					t.Fatalf("edge count %d != %d", len(got), len(want))
				}
				for e := range want {
					if !got[e] {
						t.Fatal("missing original edge")
					}
				}
				total += len(want)
				t.Logf("fixture=%d seed=%d dependent=%v recovered=%d/%d", i, seed, dependent, len(got), len(want))
			}
		}
	}
	t.Logf("recovered %d original edge instances across all runs", total)
}

func TestStateAttackUnknown(t *testing.T) {
	fn := attackFixture(t, "if n>0 { return n+1 }; return n-1")
	if attackEval(fn.Params[0], nil).known {
		t.Fatal("parameter must stay unknown")
	}
	dynamic := &ssa.BinOp{Op: token.XOR, X: fn.Params[0], Y: makeSsaInt(1)}
	if attackEval(dynamic, nil).known {
		t.Fatal("dynamic XOR must stay unknown")
	}
	real := make(map[*ssa.BasicBlock]bool)
	for _, b := range fn.Blocks {
		real[b] = true
	}
	applyFlattening(fn, mathrand.New(mathrand.NewSource(1)), true)
	phi := fn.Blocks[0].Instrs[0].(*ssa.Phi)
	phi.Edges[0] = fn.Params[0]
	if _, err := recoverStateEdges(fn, real); err == nil {
		t.Fatal("dynamic dispatcher state must fail closed")
	}
}
