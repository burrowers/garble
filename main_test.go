// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"flag"
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
			env.Vars = append(env.Vars, "TESTBIN="+os.Args[0])

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
