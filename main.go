// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/ioutil"
	"log"
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

	garble build [build flags] [packages]

which is equivalent to the longer:

	go build -a -trimpath -toolexec=garble [build flags] [packages]
`[1:])
	flagSet.PrintDefaults()
	os.Exit(2)
}

func main() { os.Exit(main1()) }

var (
	workingDir string
	deferred   []func() error
	fset       = token.NewFileSet()

	b64           = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_z")
	printerConfig = printer.Config{Mode: printer.RawFormat}
)

func main1() int {
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return 2
	}
	log.SetPrefix("[garble] ")
	args := flagSet.Args()
	if len(args) < 1 {
		flagSet.Usage()
	}

	// If we recognise an argument, we're not running within -toolexec.
	switch args[0] {
	case "build":
		goArgs := []string{
			"build",
			"-a",
			"-trimpath",
			"-toolexec="+os.Args[0],
		}
		goArgs = append(goArgs, args[1:]...)

		cmd := exec.Command("go", goArgs...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}

	var err error
	workingDir, err = os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	_, tool := filepath.Split(args[0])
	// TODO: trim ".exe" for windows?
	transformed := args[1:]
	// log.Println(tool, transformed)
	if transform := transformFuncs[tool]; transform != nil {
		if transformed, err = transform(transformed); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	defer func() {
		for _, fn := range deferred {
			if err := fn(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}()
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
	"link":    transformLink,
}

func transformCompile(args []string) ([]string, error) {
	flags, files := splitFlagsFromFiles(args, ".go")
	if len(files) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}

	trimpath := flagValue(flags, "-trimpath")
	if !strings.Contains(trimpath, workingDir) {
		return nil, fmt.Errorf("-toolexec=garble should be used alongside -trimpath")
	}
	std := flagValue(flags, "-std") == "true"
	buildid := flagValue(flags, "-buildid")
	for i, file := range files {
		if std {
			continue
		}
		var err error
		files[i], err = transformGoFile(buildid, file)
		if err != nil {
			return nil, err
		}
	}
	return append(flags, files...), nil
}

func hashWith(salt, value string) string {
	const length = 8

	d := sha256.New()
	io.WriteString(d, salt)
	io.WriteString(d, value)
	sum := b64.EncodeToString(d.Sum(nil))

	for i := 0; i < len(sum)-length; i++ {
		if '0' <= sum[i] && sum[i] <= '9' {
			continue
		}
		return sum[i : i+length]
	}
	return "_" + sum[:length-1]
}

// transformGoFile creates a garbled copy of the Go file at path, and returns
// the path to the copy.
func transformGoFile(buildid, path string) (string, error) {
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return "", err
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			switch {
			case node.Obj == nil:
				// a builtin name, or the package name
			case node.Name == "main":
				// possibly the main func
			default:
				node.Name = hashWith(buildid, node.Name)
			}
		}
		return true
	})
	f, err := ioutil.TempFile("", "garble")
	if err != nil {
		return "", err
	}
	defer f.Close()
	printerConfig.Fprint(os.Stderr, fset, file)
	if err := printerConfig.Fprint(f, fset, file); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
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

// splitFlagsFromFiles splits args into a list of flag and file arguments. Since
// we can't rely on "--" being present, and we don't parse all flags upfront, we
// rely on finding the first argument that doesn't begin with "-" and that has
// the extension we expect for the list of files.
func splitFlagsFromFiles(args []string, ext string) (flags, files []string) {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") && strings.HasSuffix(arg, ext) {
			return args[:i:i], args[i:]
		}
	}
	return args, nil
}

// flagValue retrieves the value of a flag such as "-foo", from strings in the
// list of arguments like "-foo=bar" or "-foo" "bar".
func flagValue(flags []string, name string) string {
	for i, arg := range flags {
		if val := strings.TrimPrefix(arg, name+"="); val != arg {
			// -name=value
			return val
		}
		if arg == name {
			if i+1 < len(flags) {
				if val := flags[i+1]; !strings.HasPrefix(val, "-") {
					// -name value
					return flags[i+1]
				}
			}
			// -name, equivalent to -name=true
			return "true"
		}
	}
	return ""
}
