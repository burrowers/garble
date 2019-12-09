// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

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

	testscript.Run(t, testscript.Params{
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
			if err := os.Symlink(os.Args[0], filepath.Join(bindir, "garble")); err != nil {
				return err
			}
			env.Vars = append(env.Vars, fmt.Sprintf("PATH=%s%c%s", bindir, filepath.ListSeparator, os.Getenv("PATH")))
			env.Vars = append(env.Vars, "TESTSCRIPT_COMMAND=garble")

			// GitHub Actions doesn't define %LocalAppData% on
			// Windows, which breaks $GOCACHE. Set it ourselves.
			if runtime.GOOS == "windows" {
				env.Vars = append(env.Vars, fmt.Sprintf(`LOCALAPPDATA=%s\appdata`, env.WorkDir))
			}

			for _, name := range [...]string{
				"HOME",
				"USERPROFILE", // $HOME for windows
				"GOCACHE",
			} {
				if value := os.Getenv(name); value != "" {
					env.Vars = append(env.Vars, name+"="+value)
				}
			}
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"bingrep": bingrep,
			"bincmp":  bincmp,
		},
		UpdateScripts: *update,
	})
}

func bingrep(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 2 {
		ts.Fatalf("usage: bingrep file pattern...")
	}
	data := ts.ReadFile(args[0])
	for _, pattern := range args[1:] {
		rx, err := regexp.Compile(pattern)
		ts.Check(err)
		match := rx.MatchString(data)
		if match && neg {
			ts.Fatalf("unexpected match for %q in %s", pattern, args[0])
		} else if !match && !neg {
			ts.Fatalf("expected match for %q in %s", pattern, args[0])
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
		sizeDiff := len(data2) - len(data1)
		ts.Fatalf("%s and %s differ; size diff: %+d", args[0], args[1], sizeDiff)
	}
}
