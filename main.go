// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
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
	deferred []func() error
	fset     = token.NewFileSet()

	b64           = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_z")
	printerConfig = printer.Config{Mode: printer.RawFormat}
	typesConfig   = types.Config{Importer: importer.ForCompiler(fset, "gc", objLookup)}

	buildInfo = packageInfo{imports: make(map[string]importedPkg)}
)

type jsonExport struct {
	Export string
}

func objLookup(path string) (io.ReadCloser, error) {
	// objPath := buildInfo.imports[path].packagefile
	cmd := exec.Command("go", "list", "-json", "-export", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go list error: %v: %s", err, out)
	}
	var res struct {
		Export string
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return os.Open(res.Export)
}

type packageInfo struct {
	buildID string
	imports map[string]importedPkg
}

type importedPkg struct {
	packagefile string
	buildID     string
}

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
			"-toolexec=" + os.Args[0],
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

	_, tool := filepath.Split(args[0])
	// TODO: trim ".exe" for windows?
	transformed := args[1:]
	// log.Println(tool, transformed)
	if transform := transformFuncs[tool]; transform != nil {
		var err error
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
	flags, paths := splitFlagsFromFiles(args, ".go")
	if len(paths) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}

	// If the value of -trimpath doesn't contain the separator ';', the 'go
	// build' command is most likely not using '-trimpath'.
	trimpath := flagValue(flags, "-trimpath")
	if !strings.Contains(trimpath, ";") {
		return nil, fmt.Errorf("-toolexec=garble should be used alongside -trimpath")
	}
	if flagValue(flags, "-std") == "true" {
		return args, nil
	}
	if err := readBuildIDs(flags); err != nil {
		return nil, err
	}
	// log.Printf("%#v", ids)
	var files []*ast.File
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
	}
	if _, err := typesConfig.Check("current.pkg/path", fset, files, info); err != nil {
		return nil, fmt.Errorf("typecheck error: %v", err)
	}

	args = flags
	for _, file := range files {
		file := transformGo(file, info)
		f, err := ioutil.TempFile("", "garble")
		if err != nil {
			return nil, err
		}
		defer f.Close()
		// printerConfig.Fprint(os.Stderr, fset, file)
		if err := printerConfig.Fprint(f, fset, file); err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		deferred = append(deferred, func() error {
			return os.Remove(f.Name())
		})
		args = append(args, f.Name())
	}
	return args, nil
}

func readBuildIDs(flags []string) error {
	buildInfo.buildID = flagValue(flags, "-buildid")
	if buildInfo.buildID == "" {
		return fmt.Errorf("could not find -buildid argument")
	}
	buildInfo.buildID = trimBuildID(buildInfo.buildID)
	importcfg := flagValue(flags, "-importcfg")
	if importcfg == "" {
		return fmt.Errorf("could not find -importcfg argument")
	}
	data, err := ioutil.ReadFile(importcfg)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, " ")
		if i < 0 {
			continue
		}
		if verb := line[:i]; verb != "packagefile" {
			continue
		}
		args := strings.TrimSpace(line[i+1:])
		j := strings.Index(args, "=")
		if j < 0 {
			continue
		}
		importPath, objectPath := args[:j], args[j+1:]
		fileID, err := buildidOf(objectPath)
		if err != nil {
			return err
		}
		buildInfo.imports[importPath] = importedPkg{
			packagefile: objectPath,
			buildID:     fileID,
		}
	}
	// log.Printf("%#v", buildInfo)
	return nil
}

func trimBuildID(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.IndexByte(id, '/'); i > 0 {
		id = id[:i]
	}
	return id
}

func buildidOf(path string) (string, error) {
	cmd := exec.Command("go", "tool", "buildid", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	return trimBuildID(string(out)), nil
}

func hashWith(salt, value string) string {
	const length = 8

	d := sha256.New()
	io.WriteString(d, salt)
	io.WriteString(d, value)
	sum := b64.EncodeToString(d.Sum(nil))

	if token.IsExported(value) {
		return "Z" + sum[:length]
	}
	return "z" + sum[:length]
}

// transformGo garbles the provided Go syntax node.
func transformGo(node ast.Node, info *types.Info) ast.Node {
	pre := func(cursor *astutil.Cursor) bool {
		switch node := cursor.Node().(type) {
		case *ast.Ident:
			if node.Name == "_" {
				return true // unnamed remains unnamed
			}
			obj := info.ObjectOf(node)
			switch obj.(type) {
			case *types.Var:
			case *types.Const:
			case *types.TypeName:
			case *types.Func:
				switch node.Name {
				case "main", "init":
					return true // don't break them
				}
			case nil:
				switch cursor.Parent().(type) {
				case *ast.AssignStmt:
					// symbolic var v in v := expr.(type)
				default:
					return true
				}
			default:
				return true // we only want to rename the above
			}
			buildID := buildInfo.buildID
			if obj != nil {
				pkg := obj.Pkg()
				if pkg == nil {
					return true // universe scope
				}
				path := pkg.Path()
				if !strings.Contains(path, ".") {
					return true // std isn't transformed
				}
				if id := buildInfo.imports[path].buildID; id != "" {
					buildID = id
				}
			}
			// log.Printf("%#v\n", node.Obj)
			node.Name = hashWith(buildID, node.Name)
		}
		return true
	}
	return astutil.Apply(node, pre, nil)
}

func transformLink(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".a")
	if len(paths) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}
	flags = append(flags, "-w", "-s")
	return append(flags, paths...), nil
}

// splitFlagsFromFiles splits args into a list of flag and file arguments. Since
// we can't rely on "--" being present, and we don't parse all flags upfront, we
// rely on finding the first argument that doesn't begin with "-" and that has
// the extension we expect for the list of paths.
func splitFlagsFromFiles(args []string, ext string) (flags, paths []string) {
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
