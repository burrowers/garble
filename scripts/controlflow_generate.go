//go:build ignore

package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"math/rand"
	"os"
	"strings"

	"golang.org/x/tools/go/ssa/ssautil"
	"mvdan.cc/garble/internal/ctrlflow"
)

var bodies = []string{
	`x:=n; if n&1!=0 { x+=3 } else { x-=2 }; if n&2!=0 { x*=2 } else { x+=5 }; if n&4!=0 { x-=7 } else { x+=11 }; return x`,
	`s:=uint32(0); for i:=uint32(0);i<n%24;i++ { if i&1==0 { s+=i } else { s-=i } }; return s+n`,
	`a,b:=uint32(1),uint32(2); if n&1==0 { a=n+3; b=n+1 } else { a=n+2; b=n+7 }; if n&2==0 { a+=b } else { b+=a }; return a*b`,
	`s:=uint32(0); for i:=uint32(0);i<n%16;i++ { for j:=uint32(0);j<i;j++ { if j==3 { break }; s+=j }; if i==9 { continue }; s+=i }; return s+n`,
	`switch n { case 0: return 4; case 1,2: return n+7; case 6: return 9; case 17: return n*2; default: return n*3 }`,
	`s:=uint32(0); for i:=uint32(0);i<n%32;i++ { if i>10 { return s+n }; s+=i }; return s+1`,
}

func main() {
	directive := flag.String("directive", "", "empty means ordinary source")
	seed := flag.Int64("seed", 1, "fixed seed")
	output := flag.String("output", "", "output source file")
	flag.Parse()
	var src strings.Builder
	src.WriteString("package main\n")
	for i, b := range bodies {
		if *directive != "" {
			fmt.Fprintf(&src, "//garble:controlflow %s\n", *directive)
		}
		fmt.Fprintf(&src, "func compute%d(n uint32) uint32 {%s}\nfunc reference%d(n uint32) uint32 {%s}\n", i, b, i, b)
	}
	src.WriteString("func main(){ inputs:=[]uint32{0,1,2,3,4,5,6,7,8,15,16,17,23,31,63,64,255,65535,2147483648,4294967295}; for _,n:=range inputs {")
	for i := range bodies {
		fmt.Fprintf(&src, "if compute%d(n)!=reference%d(n){panic(%q)};", i, i, fmt.Sprintf("compute%d mismatch", i))
	}
	src.WriteString("}}\n")
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "main.go", src.String(), parser.ParseComments)
	must(err)
	if *directive != "" {
		p, _, err := ssautil.BuildPackage(&types.Config{}, fs, types.NewPackage("probe", "main"), []*ast.File{f}, 0)
		must(err)
		_, generated, _, err := ctrlflow.Obfuscate(fs, p, []*ast.File{f}, rand.New(rand.NewSource(*seed)))
		must(err)
		f.Decls = append(f.Decls, generated.Decls...)
	}
	f.Comments = nil
	var buf bytes.Buffer
	must(printer.Fprint(&buf, fs, f))
	rendered := buf.String()
	for i := range bodies {
		rendered = strings.ReplaceAll(rendered, fmt.Sprintf("func compute%d(", i), fmt.Sprintf("//go:noinline\nfunc compute%d(", i))
	}
	must(os.WriteFile(*output, []byte(rendered), 0600))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
