// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
			if err := os.Symlink(os.Args[0], filepath.Join(bindir, "garble")); err != nil {
				return err
			}
			env.Vars = append(env.Vars, fmt.Sprintf("PATH=%s%c%s", bindir, filepath.ListSeparator, os.Getenv("PATH")))
			env.Vars = append(env.Vars, "TESTSCRIPT_COMMAND=garble")

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
		UpdateScripts: *update,
	})
}
