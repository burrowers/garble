// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rogpeppe/go-internal/goproxytest"
	"github.com/rogpeppe/go-internal/gotooltest"
	"github.com/rogpeppe/go-internal/testscript"

	ah "mvdan.cc/garble/internal/asthelper"
)

var proxyURL string

func TestMain(m *testing.M) {
	os.Exit(testscript.RunMain(garbleMain{m}, map[string]func() int{
		"garble": main1,
	}))
}

type garbleMain struct {
	m *testing.M
}

func (m garbleMain) Run() int {
	// Start the Go proxy server running for all tests.
	srv, err := goproxytest.NewServer("testdata/mod", "")
	if err != nil {
		panic(fmt.Sprintf("cannot start proxy: %v", err))
	}
	proxyURL = srv.URL

	return m.m.Run()
}

var update = flag.Bool("u", false, "update testscript output files")

func TestScripts(t *testing.T) {
	t.Parallel()

	p := testscript.Params{
		Dir: filepath.Join("testdata", "scripts"),
		Setup: func(env *testscript.Env) error {
			env.Vars = append(env.Vars,
				"GOPROXY="+proxyURL,
				"GONOSUMDB=*",
				"gofullversion="+runtime.Version(),
			)

			if os.Getenv("TESTSCRIPT_COVER_DIR") != "" {
				// Don't reuse the build cache if we want to collect
				// code coverage. Otherwise, many toolexec calls would
				// be avoided and the coverage would be incomplete.
				env.Vars = append(env.Vars, "GOCACHE="+filepath.Join(env.WorkDir, "go-cache-tmp"))
			}
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"binsubstr":         binsubstr,
			"bincmp":            bincmp,
			"generate-literals": generateLiterals,
		},
		UpdateScripts: *update,
	}
	if err := gotooltest.Setup(&p); err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, p)
}

type binaryCache struct {
	name    string
	modtime time.Time
	content string
}

var cachedBinary binaryCache

func readFile(ts *testscript.TestScript, file string) string {
	file = ts.MkAbs(file)
	info, err := os.Stat(file)
	if err != nil {
		ts.Fatalf("%v", err)
	}

	if cachedBinary.modtime == info.ModTime() && cachedBinary.name == file {
		return cachedBinary.content
	}

	cachedBinary.name = file
	cachedBinary.modtime = info.ModTime()
	cachedBinary.content = ts.ReadFile(file)
	return cachedBinary.content
}

func createFile(ts *testscript.TestScript, path string) *os.File {
	file, err := os.Create(ts.MkAbs(path))
	if err != nil {
		ts.Fatalf("%v", err)
	}
	return file
}

func binsubstr(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 2 {
		ts.Fatalf("usage: binsubstr file substr...")
	}
	data := readFile(ts, args[0])
	var failed []string
	for _, substr := range args[1:] {
		match := strings.Contains(data, substr)
		if match && neg {
			failed = append(failed, substr)
		} else if !match && !neg {
			failed = append(failed, substr)
		}
	}
	if len(failed) > 0 && neg {
		ts.Fatalf("unexpected match for %q in %s", failed, args[0])
	} else if len(failed) > 0 {
		ts.Fatalf("expected match for %q in %s", failed, args[0])
	}
}

func bincmp(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: bincmp file1 file2")
	}
	data1 := ts.ReadFile(args[0])
	data2 := ts.ReadFile(args[1])
	if neg {
		if data1 == data2 {
			ts.Fatalf("%s and %s don't differ", args[0], args[1])
		}
		return
	}
	if data1 != data2 {
		if _, err := exec.LookPath("diffoscope"); err != nil {
			ts.Logf("diffoscope is not installing; skipping binary diff")
		} else {
			// We'll error below; ignore the exec error here.
			ts.Exec("diffoscope", ts.MkAbs(args[0]), ts.MkAbs(args[1]))
		}
		sizeDiff := len(data2) - len(data1)
		ts.Fatalf("%s and %s differ; diffoscope above, size diff: %+d",
			args[0], args[1], sizeDiff)
	}
}

func generateStringLit(size int) *ast.BasicLit {
	buffer := make([]byte, size)
	_, err := mathrand.Read(buffer)
	if err != nil {
		panic(err)
	}

	return ah.StringLit(string(buffer))
}

func generateLiterals(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! generate-literals")
	}
	if len(args) != 1 {
		ts.Fatalf("usage: generate-literals file")
	}

	codePath := args[0]

	// Add 100 randomly small literals.
	var statements []ast.Stmt
	for i := 0; i < 100; i++ {
		literal := generateStringLit(1 + mathrand.Intn(255))
		statements = append(statements, &ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("_")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{literal},
		})
	}
	// Add 5 huge literals, to make sure we don't try to obfuscate them.
	// 5 * 128KiB is large enough that it would take a very, very long time
	// to obfuscate those literals with our simple code.
	for i := 0; i < 5; i++ {
		literal := generateStringLit(128 << 10)
		statements = append(statements, &ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("_")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{literal},
		})
	}

	file := &ast.File{
		Name: ast.NewIdent("main"),
		Decls: []ast.Decl{&ast.FuncDecl{
			Name: ast.NewIdent("extraLiterals"),
			Type: &ast.FuncType{Params: &ast.FieldList{}},
			Body: ah.BlockStmt(statements...),
		}},
	}

	codeFile := createFile(ts, codePath)
	defer codeFile.Close()

	if err := printer.Fprint(codeFile, token.NewFileSet(), file); err != nil {
		ts.Fatalf("%v", err)
	}
}

func TestSplitFlagsFromArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want [2][]string
	}{
		{"Empty", []string{}, [2][]string{{}, nil}},
		{
			"JustFlags",
			[]string{"-foo", "bar", "-baz"},
			[2][]string{{"-foo", "bar", "-baz"}, nil},
		},
		{
			"JustArgs",
			[]string{"some", "pkgs"},
			[2][]string{{}, {"some", "pkgs"}},
		},
		{
			"FlagsAndArgs",
			[]string{"-foo=bar", "baz"},
			[2][]string{{"-foo=bar"}, {"baz"}},
		},
		{
			"BoolFlagsAndArgs",
			[]string{"-race", "pkg"},
			[2][]string{{"-race"}, {"pkg"}},
		},
		{
			"ExplicitBoolFlag",
			[]string{"-race=true", "pkg"},
			[2][]string{{"-race=true"}, {"pkg"}},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			flags, args := splitFlagsFromArgs(test.args)
			got := [2][]string{flags, args}

			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Fatalf("splitFlagsFromArgs(%q) mismatch (-want +got):\n%s", test.args, diff)
			}
		})
	}
}

func TestFilterBuildFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		flags []string
		want  []string
	}{
		{"Empty", []string{}, nil},
		{
			"NoBuild",
			[]string{"-short", "-json"},
			nil,
		},
		{
			"Mixed",
			[]string{"-short", "-tags", "foo", "-mod=readonly", "-json"},
			[]string{"-tags", "foo", "-mod=readonly"},
		},
		{
			"NonBinarySkipped",
			[]string{"-o", "binary", "-tags", "foo"},
			[]string{"-tags", "foo"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, _ := filterBuildFlags(test.flags)

			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Fatalf("filterBuildFlags(%q) mismatch (-want +got):\n%s", test.flags, diff)
			}
		})
	}
}

func TestFlagValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		flags    []string
		flagName string
		want     string
	}{
		{"StrSpace", []string{"-buildid", "bar"}, "-buildid", "bar"},
		{"StrSpaceDash", []string{"-buildid", "-bar"}, "-buildid", "-bar"},
		{"StrEqual", []string{"-buildid=bar"}, "-buildid", "bar"},
		{"StrEqualDash", []string{"-buildid=-bar"}, "-buildid", "-bar"},
		{"StrMissing", []string{"-foo"}, "-buildid", ""},
		{"StrNotFollowed", []string{"-buildid"}, "-buildid", ""},
		{"StrEmpty", []string{"-buildid="}, "-buildid", ""},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := flagValue(test.flags, test.flagName)
			if got != test.want {
				t.Fatalf("flagValue(%q, %q) got %q, want %q",
					test.flags, test.flagName, got, test.want)
			}
		})
	}
}
