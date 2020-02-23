// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/gotooltest"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	os.Exit(testscript.RunMain(m, map[string]func() int{
		"garble": main1,
	}))
}

var update = flag.Bool("u", false, "update testscript output files")

func TestScripts(t *testing.T) {
	t.Parallel()

	p := testscript.Params{
		Dir: filepath.Join("testdata", "scripts"),
		Setup: func(env *testscript.Env) error {
			bindir := filepath.Join(env.WorkDir, ".bin")
			if err := os.Mkdir(bindir, 0777); err != nil {
				return err
			}
			binfile := filepath.Join(bindir, "garble")
			if runtime.GOOS == "windows" {
				binfile += ".exe"
			}
			if err := os.Symlink(os.Args[0], binfile); err != nil {
				return err
			}
			env.Vars = append(env.Vars, fmt.Sprintf("PATH=%s%c%s", bindir, filepath.ListSeparator, os.Getenv("PATH")))
			env.Vars = append(env.Vars, "TESTSCRIPT_COMMAND=garble")
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"binsubstr": binsubstr,
			"bincmp":    bincmp,
		},
		UpdateScripts: *update,
	}
	if err := gotooltest.Setup(&p); err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, p)
}

func binsubstr(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 2 {
		ts.Fatalf("usage: binsubstr file substr...")
	}
	data := ts.ReadFile(args[0])
	for _, substr := range args[1:] {
		match := strings.Contains(data, substr)
		if match && neg {
			ts.Fatalf("unexpected match for %q in %s", substr, args[0])
		} else if !match && !neg {
			ts.Fatalf("expected match for %q in %s", substr, args[0])
		}
	}
}

func bincmp(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("unsupported: ! bincmp")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: bincmp file1 file2")
	}
	data1 := ts.ReadFile(args[0])
	data2 := ts.ReadFile(args[1])
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

func TestFlagValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		flags    []string
		flagName string
		want     string
	}{
		{"BoolAlone", []string{"-std"}, "-std", "true"},
		{"BoolFollowed", []string{"-std", "-foo"}, "-std", "true"},
		{"BoolFalse", []string{"-std=false"}, "-std", "false"},
		{"BoolMissing", []string{"-foo"}, "-std", ""},
		{"BoolEmpty", []string{"-std="}, "-std", ""},
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
