// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	switch tool {
	case "compile":
		var err error
		transformed, err = transformCompile(args[1:])
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

func transformCompile(args []string) ([]string, error) {
	return args, nil
}
