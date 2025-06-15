// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"io/fs"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-quicktest/qt"
	"github.com/rogpeppe/go-internal/goproxytest"
	"github.com/rogpeppe/go-internal/gotooltest"
	"github.com/rogpeppe/go-internal/testscript"

	ah "mvdan.cc/garble/internal/asthelper"
)

var proxyURL string

func TestMain(m *testing.M) {
	// If GORACE is unset, lower the default of atexit_sleep_ms=1000,
	// since otherwise every execution of garble through the test binary
	// would sleep for one second before exiting.
	// Given how many times garble runs via toolexec, that is very slow!
	// If GORACE is set, we assume that the caller knows what they are doing,
	// and we don't try to replace or modify their flags.
	if os.Getenv("GORACE") == "" {
		os.Setenv("GORACE", "atexit_sleep_ms=10")
	}
	if os.Getenv("RUN_GARBLE_MAIN") == "true" {
		main()
		return
	}
	testscript.Main(garbleMain{m}, map[string]func(){
		"garble": main,
	})
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

func TestScript(t *testing.T) {
	t.Parallel()

	execPath, err := os.Executable()
	qt.Assert(t, qt.IsNil(err))

	tempCacheDir := t.TempDir()

	hostCacheDir, err := os.UserCacheDir()
	qt.Assert(t, qt.IsNil(err))

	p := testscript.Params{
		Dir: filepath.Join("testdata", "script"),
		Setup: func(env *testscript.Env) error {
			// Use testdata/mod as our module proxy.
			env.Setenv("GOPROXY", proxyURL)

			// gotoolchain.txtar is one test which wants to reuse GOMODCACHE.
			out, err := exec.Command("go", "env", "GOMODCACHE").Output()
			if err != nil {
				return err
			}
			env.Setenv("HOST_GOMODCACHE", strings.TrimSpace(string(out)))

			// We use our own GOPROXY above, so avoid using sum.golang.org,
			// as we would fail to update any go.sum file in the testscripts.
			env.Setenv("GONOSUMDB", "*")

			// "go build" starts many short-lived Go processes,
			// such as asm, buildid, compile, and link.
			// They don't allocate huge amounts of memory,
			// and they'll exit within seconds,
			// so using the GC is basically a waste of CPU.
			// Turn it off entirely, releasing memory on exit.
			//
			// We don't want this setting always on,
			// as it could result in memory problems for users.
			// But it helps for our test suite,
			// as the packages are relatively small.
			env.Setenv("GOGC", "off")

			env.Setenv("gofullversion", runtime.Version())
			env.Setenv("EXEC_PATH", execPath)

			if os.Getenv("GOCOVERDIR") != "" {
				// Don't share cache dirs with the host if we want to collect code
				// coverage. Otherwise, the coverage info might be incomplete.
				env.Setenv("GOCACHE", filepath.Join(tempCacheDir, "go-cache"))
				env.Setenv("GARBLE_CACHE", filepath.Join(tempCacheDir, "garble-cache"))
			} else {
				// GOCACHE is initialized by gotooltest to use the host's cache.
				env.Setenv("GARBLE_CACHE", filepath.Join(hostCacheDir, "garble"))
			}
			return nil
		},
		// TODO: this condition should probably be supported by gotooltest
		Condition: func(cond string) (bool, error) {
			switch cond {
			case "cgo":
				out, err := exec.Command("go", "env", "CGO_ENABLED").Output()
				if err != nil {
					return false, err
				}
				result := strings.TrimSpace(string(out))
				switch result {
				case "0", "1":
					return result == "1", nil
				default:
					return false, fmt.Errorf("unknown CGO_ENABLED: %q", result)
				}
			}
			return false, fmt.Errorf("unknown condition")
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"sleep":             sleep,
			"binsubstr":         binsubstr,
			"bincmp":            bincmp,
			"generate-literals": generateLiterals,
			"setenvfile":        setenvfile,
			"grepfiles":         grepfiles,
			"setup-go":          setupGo,
		},
		UpdateScripts:       *update,
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
	}
	if err := gotooltest.Setup(&p); err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, p)
}

func createFile(ts *testscript.TestScript, path string) *os.File {
	file, err := os.Create(ts.MkAbs(path))
	if err != nil {
		ts.Fatalf("%v", err)
	}
	return file
}

// sleep is akin to a shell's sleep builtin.
// Note that tests should almost never use this; it's currently only used to
// work around a low-level Go syscall race on Linux.
func sleep(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: sleep duration")
	}
	d, err := time.ParseDuration(args[0])
	if err != nil {
		ts.Fatalf("%v", err)
	}
	time.Sleep(d)
}

func binsubstr(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 2 {
		ts.Fatalf("usage: binsubstr file substr...")
	}
	data := ts.ReadFile(args[0])
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
	for _, arg := range args {
		switch arg {
		case "stdout", "stderr":
			// Note that the diffoscope call below would not deal with
			// stdout/stderr either.
			ts.Fatalf("bincmp is for binary files. did you mean cmp?")
		}
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
		if _, err := exec.LookPath("diffoscope"); err == nil {
			// We'll error below; ignore the exec error here.
			ts.Exec("diffoscope",
				"--diff-context", "2", // down from 7 by default
				"--max-text-report-size", "4096", // no limit (in bytes) by default; avoid huge output
				ts.MkAbs(args[0]), ts.MkAbs(args[1]))
		} else {
			ts.Logf("diffoscope not found; skipping")
		}
		outDir := "bincmp_output"
		err := os.MkdirAll(outDir, 0o777)
		ts.Check(err)

		file1, err := os.CreateTemp(outDir, "file1-*")
		ts.Check(err)
		_, err = file1.Write([]byte(data1))
		ts.Check(err)
		err = file1.Close()
		ts.Check(err)

		file2, err := os.CreateTemp(outDir, "file2-*")
		ts.Check(err)
		_, err = file2.Write([]byte(data2))
		ts.Check(err)
		err = file2.Close()
		ts.Check(err)

		ts.Logf("wrote files to %s and %s", file1.Name(), file2.Name())
		sizeDiff := len(data2) - len(data1)
		ts.Fatalf("%s and %s differ; diffoscope above, size diff: %+d",
			args[0], args[1], sizeDiff)
	}
}

var testRand = mathrand.New(mathrand.NewSource(time.Now().UnixNano()))

func generateStringLit(minSize int) *ast.BasicLit {
	buffer := make([]byte, minSize)
	_, err := testRand.Read(buffer)
	if err != nil {
		panic(err)
	}

	return ah.StringLit(string(buffer) + "a_unique_string_that_is_part_of_all_extra_literals")
}

// generateLiterals creates a new source code file with a few random literals inside.
// All literals contain the string "a_unique_string_that_is_part_of_all_extra_literals"
// so we can later check if they are all obfuscated by looking for this substring.
// The code is designed such that the Go compiler does not optimize away the literals,
// which would destroy the test.
// This is achieved by defining a global variable `var x = ""` and an `init` function
// which appends all literals to `x`.
func generateLiterals(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! generate-literals")
	}
	if len(args) != 1 {
		ts.Fatalf("usage: generate-literals file")
	}

	codePath := args[0]

	// Global string variable to which which we append string literals: `var x = ""`
	globalVar := &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{
				Names: []*ast.Ident{ast.NewIdent("x")},
				Values: []ast.Expr{
					&ast.BasicLit{Kind: token.STRING, Value: `""`},
				},
			},
		},
	}

	var statements []ast.Stmt

	// Assignments which append 100 random small literals to x: `x += "the_small_random_literal"`
	for range 100 {
		statements = append(
			statements,
			&ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent("x")},
				Tok: token.ADD_ASSIGN,
				Rhs: []ast.Expr{generateStringLit(1 + testRand.Intn(255))},
			},
		)
	}

	// Assignments which append 5 random huge literals to x: `x += "the_huge_random_literal"`
	// We add huge literals to make sure we obfuscate them fast.
	// 5 * 128KiB is large enough that it would take a very, very long time
	// to obfuscate those literals if too complex obfuscators are used.
	for range 5 {
		statements = append(
			statements,
			&ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent("x")},
				Tok: token.ADD_ASSIGN,
				Rhs: []ast.Expr{generateStringLit(128 << 10)},
			},
		)
	}

	// An `init` function which includes all assignments from above
	initFunc := &ast.FuncDecl{
		Name: &ast.Ident{
			Name: "init",
		},
		Type: &ast.FuncType{},
		Body: ah.BlockStmt(statements...),
	}

	// A file with the global string variable and init function
	file := &ast.File{
		Name: ast.NewIdent("main"),
		Decls: []ast.Decl{
			globalVar,
			initFunc,
		},
	}

	codeFile := createFile(ts, codePath)
	defer codeFile.Close()

	if err := printer.Fprint(codeFile, token.NewFileSet(), file); err != nil {
		ts.Fatalf("%v", err)
	}
}

func setenvfile(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! setenvfile")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: setenvfile name file")
	}

	ts.Setenv(args[0], ts.ReadFile(args[1]))
}

func grepfiles(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: grepfiles path pattern")
	}
	anyFound := false
	path, pattern := ts.MkAbs(args[0]), args[1]
	rx := regexp.MustCompile(pattern)
	if err := filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rx.MatchString(path) {
			if neg {
				return fmt.Errorf("%q matches %q", path, pattern)
			} else {
				anyFound = true
				return fs.SkipAll
			}
		}
		return nil
	}); err != nil {
		ts.Fatalf("%s", err)
	}
	if !neg && !anyFound {
		ts.Fatalf("no matches for %q", pattern)
	}
}

func setupGo(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: setup-go version")
	}
	// Download the version of Go specified as an argument, cache it in GOMODCACHE,
	// and get its GOROOT directory inside the cache so we can use it.
	cmd := exec.Command("go", "env", "GOROOT")
	cmd.Env = append(cmd.Environ(), "GOTOOLCHAIN="+args[0])
	out, err := cmd.Output()
	ts.Check(err)

	goroot := strings.TrimSpace(string(out))

	ts.Setenv("PATH", filepath.Join(goroot, "bin")+string(os.PathListSeparator)+ts.Getenv("PATH"))
	// Remove GOROOT from the environment, as it is unnecessary and gets in the way
	// when we want to test GOTOOLCHAIN upgrades, which will need different GOROOTs.
	ts.Setenv("GOROOT", "")
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			flags, args := splitFlagsFromArgs(test.args)
			got := [2][]string{flags, args}

			qt.Assert(t, qt.DeepEquals(got, test.want))
		})
	}
}

func TestFilterForwardBuildFlags(t *testing.T) {
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, _ := filterForwardBuildFlags(test.flags)
			qt.Assert(t, qt.DeepEquals(got, test.want))
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := flagValue(test.flags, test.flagName)
			qt.Assert(t, qt.DeepEquals(got, test.want))
		})
	}
}
