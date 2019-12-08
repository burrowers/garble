// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var flagSet = flag.NewFlagSet("garble", flag.ContinueOnError)

func init() { flagSet.Usage = usage }

func usage() {
	fmt.Fprintf(os.Stderr, `
Usage of garble:

	go build -toolexec=garble [build flags] [packages]
`[1:])
	flagSet.PrintDefaults()
	os.Exit(2)
}

func main() { os.Exit(main1()) }

func main1() int {
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return 2
	}
	args := flagSet.Args()
	if len(args) < 1 {
		flagSet.Usage()
	}
	_, tool := filepath.Split(args[0])
	// TODO: trim ".exe" for windows?
	transformed := args[1:]
	// fmt.Fprintln(os.Stderr, tool, transformed)
	if transform := transformFuncs[tool]; transform != nil {
		var err error
		transformed, err = transform(transformed)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	cmd := exec.Command(args[0], transformed...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

var transformFuncs = map[string]func([]string) ([]string, error){
	"compile": transformCompile,
	"link": transformLink,
}

func transformCompile(args []string) ([]string, error) {
	flags, files := splitFlagsFromFiles(args, ".go")
	if len(files) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}

	// TODO: find a way to do this. -trimpath is always present for some reason.
	// trimpath := false
	// for _, flag := range flags {
	// 	if strings.HasPrefix(flag, "-trimpath") {
	// 		trimpath = true
	// 	}
	// }
	// if !trimpath {
	// 	return nil, fmt.Errorf("-toolexec=garble should be used alongside -trimpath")
	// }
	return append(flags, files...), nil
}

func transformLink(args []string) ([]string, error) {
	flags, files := splitFlagsFromFiles(args, ".a")
	if len(files) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}
	flags = append(flags, "-w", "-s")
	return append(flags, files...), nil
}

func splitFlagsFromFiles(args []string, ext string) (flags, files []string) {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") && strings.HasSuffix(arg, ext) {
			return args[:i:i], args[i:]
		}
	}
	return args, nil
}
