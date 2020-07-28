// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/goproxytest"
	"github.com/rogpeppe/go-internal/gotooltest"
	"github.com/rogpeppe/go-internal/testscript"
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
			bindir := filepath.Join(env.WorkDir, ".bin")
			if err := os.Mkdir(bindir, 0o777); err != nil {
				return err
			}
			binfile := filepath.Join(bindir, "garble")
			if runtime.GOOS == "windows" {
				binfile += ".exe"
			}
			if err := os.Symlink(os.Args[0], binfile); err != nil {
				if err := copyFile(os.Args[0], binfile); err != nil { // Fallback to copy if symlink failed. Useful for Windows not elevated processes
					return err
				}
			}
			env.Vars = append(env.Vars, fmt.Sprintf("PATH=%s%c%s", bindir, filepath.ListSeparator, os.Getenv("PATH")))
			env.Vars = append(env.Vars, "TESTSCRIPT_COMMAND=garble")
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"binsubstr":   binsubstr,
			"bincmp":      bincmp,
			"binsubint":   binsubint,
			"binsubfloat": binsubfloat,
		},
		UpdateScripts: *update,
	}
	if err := gotooltest.Setup(&p); err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, p)
}

func copyFile(from, to string) error {
	writer, err := os.Create(to)
	if err != nil {
		return err
	}
	defer writer.Close()

	reader, err := os.Open(from)
	if err != nil {
		return err
	}
	defer reader.Close()

	_, err = io.Copy(writer, reader)
	return err
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

func binsubint(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 2 {
		ts.Fatalf("usage: binsubint file subint...")
	}

	data := readFile(ts, args[0])
	var failed []string
	for _, subIntStr := range args[1:] {
		subInt, err := strconv.Atoi(subIntStr)
		if err != nil {
			ts.Fatalf("%v", err)
		}

		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, uint64(subInt))

		match := strings.Contains(data, string(b))
		if !match {
			binary.BigEndian.PutUint64(b, uint64(subInt))
			match = strings.Contains(data, string(b))
		}
		if match && neg {
			failed = append(failed, subIntStr)
		} else if !match && !neg {
			failed = append(failed, subIntStr)
		}
	}
	if len(failed) > 0 && neg {
		ts.Fatalf("unexpected match for %s in %s", failed, args[0])
	} else if len(failed) > 0 {
		ts.Fatalf("expected match for %s in %s", failed, args[0])
	}
}

func binsubfloat(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 2 {
		ts.Fatalf("usage: binsubint file binsubfloat...")
	}
	data := readFile(ts, args[0])
	var failed []string
	for _, subFloatStr := range args[1:] {
		subFloat, err := strconv.ParseFloat(subFloatStr, 64)
		if err != nil {
			ts.Fatalf("%v", err)
		}

		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, math.Float64bits(subFloat))

		match := strings.Contains(data, string(b))
		if !match {
			binary.BigEndian.PutUint64(b, math.Float64bits(subFloat))
			match = strings.Contains(data, string(b))
		}
		if match && neg {
			failed = append(failed, subFloatStr)
		} else if !match && !neg {
			failed = append(failed, subFloatStr)
		}
	}
	if len(failed) > 0 && neg {
		ts.Fatalf("unexpected match for %s in %s", failed, args[0])
	} else if len(failed) > 0 {
		ts.Fatalf("expected match for %s in %s", failed, args[0])
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
