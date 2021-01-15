// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/Binject/debug/goobj2"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/tools/go/ast/astutil"

	"mvdan.cc/garble/internal/literals"
)

var flagSet = flag.NewFlagSet("garble", flag.ContinueOnError)

var (
	flagGarbleLiterals bool
	flagGarbleTiny     bool
	flagDebugDir       string
	flagSeed           string
)

func init() {
	flagSet.Usage = usage
	flagSet.BoolVar(&flagGarbleLiterals, "literals", false, "Obfuscate literals such as strings")
	flagSet.BoolVar(&flagGarbleTiny, "tiny", false, "Optimize for binary size, losing the ability to reverse the process")
	flagSet.StringVar(&flagDebugDir, "debugdir", "", "Write the garbled source to a directory, e.g. -debugdir=out")
	flagSet.StringVar(&flagSeed, "seed", "", "Provide a base64-encoded seed, e.g. -seed=o9WDTZ4CN4w\nFor a random seed, provide -seed=random")
}

func usage() {
	fmt.Fprintf(os.Stderr, `
Garble obfuscates Go code by wrapping the Go toolchain.

Usage:

	garble [flags] build [build flags] [packages]

Aside from "build", the "test" command mirroring "go test" is also supported.

garble accepts the following flags:

`[1:])
	flagSet.PrintDefaults()
	fmt.Fprintf(os.Stderr, `

For more information, see https://github.com/burrowers/garble.
`[1:])
}

func main() { os.Exit(main1()) }

var (
	deferred []func() error
	fset     = token.NewFileSet()

	nameCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_z"
	b64         = base64.NewEncoding(nameCharset)
	printConfig = printer.Config{Mode: printer.RawFormat}

	// origImporter is a go/types importer which uses the original versions
	// of packages, without any obfuscation. This is helpful to make
	// decisions on how to obfuscate our input code.
	origImporter = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		pkg, err := listPackage(path)
		if err != nil {
			return nil, err
		}
		return os.Open(pkg.Export)
	})

	// Basic information about the package being currently compiled or
	// linked. These variables are filled in early, and reused later.
	curPkgPath   string // note that this isn't filled for the linker yet
	curActionID  []byte
	curImportCfg string

	buildInfo = struct {
		// TODO: replace part of this with goobj.ParseImportCfg, so that
		// we can also reuse it. For now, parsing ourselves is still
		// necessary so that we can set firstImport.
		imports map[string]importedPkg // parsed importCfg plus cached info

		firstImport string // first from -importcfg; the main package when linking
	}{imports: make(map[string]importedPkg)}

	garbledImporter = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(buildInfo.imports[path].packagefile)
	}).(types.ImporterFrom)

	opts *options

	envGoPrivate = os.Getenv("GOPRIVATE") // complemented by 'go env' later
)

const (
	garbleMapHeaderName = "garble/nameMap"
	garbleSrcHeaderName = "garble/src"
)

func garbledImport(path string) (*types.Package, error) {
	ipkg, ok := buildInfo.imports[path]
	if !ok {
		return nil, fmt.Errorf("could not find imported package %q", path)
	}
	if ipkg.pkg != nil {
		return ipkg.pkg, nil // cached
	}
	if opts.GarbleDir == "" {
		return nil, fmt.Errorf("$GARBLE_DIR unset; did you run via 'garble build'?")
	}
	pkg, err := garbledImporter.ImportFrom(path, opts.GarbleDir, 0)
	if err != nil {
		return nil, err
	}
	ipkg.pkg = pkg // cache for later use
	return pkg, nil
}

type importedPkg struct {
	packagefile string
	actionID    []byte

	pkg *types.Package
}

func main1() int {
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return 2
	}
	log.SetPrefix("[garble] ")
	args := flagSet.Args()
	if len(args) < 1 {
		usage()
		return 2
	}
	if err := mainErr(args); err != nil {
		switch err {
		case flag.ErrHelp:
			usage()
			return 2
		case errJustExit:
		default:
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	return 0
}

var errJustExit = errors.New("")

func goVersionOK() bool {
	const (
		minGoVersion        = "v1.15.0"
		supportedGoVersions = "1.15.x"

		gitTimeFormat = "Mon Jan 2 15:04:05 2006 -0700"
	)
	// Go 1.15 was released on August 11th, 2020.
	minGoVersionDate := time.Date(2020, 8, 11, 0, 0, 0, 0, time.UTC)

	out, err := exec.Command("go", "version").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, `Can't get Go version: %v

This is likely due to go not being installed/setup correctly.

How to install Go: https://golang.org/doc/install
`, err)
		return false
	}

	rawVersion := strings.TrimPrefix(strings.TrimSpace(string(out)), "go version ")

	tagIdx := strings.IndexByte(rawVersion, ' ')
	tag := rawVersion[:tagIdx]
	if tag == "devel" {
		commitAndDate := rawVersion[tagIdx+1:]
		// Remove commit hash and architecture from version
		startDateIdx := strings.IndexByte(commitAndDate, ' ') + 1
		endDateIdx := strings.LastIndexByte(commitAndDate, ' ')
		if endDateIdx <= 0 {
			fmt.Fprintf(os.Stderr, "Can't recognize devel build timestamp")
			return false
		}
		date := commitAndDate[startDateIdx:endDateIdx]

		versionDate, err := time.Parse(gitTimeFormat, date)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Can't recognize devel build timestamp: %v\n", err)
			return false
		}

		if versionDate.After(minGoVersionDate) {
			return true
		}

		fmt.Fprintf(os.Stderr, "You use the old unstable %q Go version, please upgrade Go to %s\n", rawVersion, supportedGoVersions)
		return false
	}

	version := "v" + strings.TrimPrefix(tag, "go")
	if semver.Compare(version, minGoVersion) < 0 {
		fmt.Fprintf(os.Stderr, "Outdated Go version %q is used, please upgrade Go to %s\n", version, supportedGoVersions)
		return false
	}

	return true
}

func mainErr(args []string) error {
	// If we recognise an argument, we're not running within -toolexec.
	switch cmd := args[0]; cmd {
	case "help":
		return flag.ErrHelp
	case "build", "test":
		if !goVersionOK() {
			return errJustExit
		}
		// Split the flags from the package arguments, since we'll need
		// to run 'go list' on the same set of packages.
		flags, args := splitFlagsFromArgs(args[1:])
		for _, f := range flags {
			switch f {
			case "-h", "-help", "--help":
				return flag.ErrHelp
			}
		}

		err := setOptions()
		if err != nil {
			return err
		}

		// Note that we also need to pass build flags to 'go list', such
		// as -tags.
		cache.BuildFlags = filterBuildFlags(flags)
		if cmd == "test" {
			cache.BuildFlags = append(cache.BuildFlags, "-test")
		}

		if err := setGoPrivate(); err != nil {
			return err
		}

		if err := setListedPackages(args); err != nil {
			return err
		}
		cache.ExecPath, err = os.Executable()
		if err != nil {
			return err
		}

		sharedName, err := saveShared()
		if err != nil {
			return err
		}
		defer os.Remove(sharedName)

		goArgs := []string{
			cmd,
			"-trimpath",
			"-toolexec=" + cache.ExecPath,
		}
		if cmd == "test" {
			// vet is generally not useful on garbled code; keep it
			// disabled by default.
			goArgs = append(goArgs, "-vet=off")
		}
		goArgs = append(goArgs, flags...)
		goArgs = append(goArgs, args...)

		cmd := exec.Command("go", goArgs...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if !filepath.IsAbs(args[0]) {
		// -toolexec gives us an absolute path to the tool binary to
		// run, so this is most likely misuse of garble by a user.
		return fmt.Errorf("unknown command: %q", args[0])
	}

	if err := loadShared(); err != nil {
		return err
	}
	opts = &cache.Options

	_, tool := filepath.Split(args[0])
	if runtime.GOOS == "windows" {
		tool = strings.TrimSuffix(tool, ".exe")
	}
	if len(args) == 2 && args[1] == "-V=full" {
		return alterToolVersion(tool, args)
	}

	transform := transformFuncs[tool]
	transformed := args[1:]
	// log.Println(tool, transformed)
	if transform != nil {
		var err error
		if transformed, err = transform(transformed); err != nil {
			return err
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
		return err
	}
	return nil
}

var transformFuncs = map[string]func([]string) ([]string, error){
	"compile": transformCompile,
	"link":    transformLink,
}

func transformCompile(args []string) ([]string, error) {
	var err error
	flags, paths := splitFlagsFromFiles(args, ".go")

	// We will force the linker to drop DWARF via -w, so don't spend time
	// generating it.
	flags = append(flags, "-dwarf=false")

	curPkgPath = flagValue(flags, "-p")
	if (curPkgPath == "runtime" && opts.Tiny) || curPkgPath == "runtime/internal/sys" {
		// Even though these packages aren't private, we will still process
		// them later to remove build information and strip code from the
		// runtime. However, we only want flags to work on private packages.
		opts.GarbleLiterals = false
		opts.DebugDir = ""
	} else if !isPrivate(curPkgPath) {
		return append(flags, paths...), nil
	}
	for i, path := range paths {
		if filepath.Base(path) == "_gomod_.go" {
			// never include module info
			paths = append(paths[:i], paths[i+1:]...)
			break
		}
	}
	if len(paths) == 1 && filepath.Base(paths[0]) == "_testmain.go" {
		return append(flags, paths...), nil
	}

	// If the value of -trimpath doesn't contain the separator ';', the 'go
	// build' command is most likely not using '-trimpath'.
	trimpath := flagValue(flags, "-trimpath")
	if !strings.Contains(trimpath, ";") {
		return nil, fmt.Errorf("-toolexec=garble should be used alongside -trimpath")
	}
	if err := fillBuildInfo(flags); err != nil {
		return nil, err
	}

	var files []*ast.File
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}

		files = append(files, file)
	}

	if len(opts.Seed) > 0 {
		mathrand.Seed(int64(binary.BigEndian.Uint64(opts.Seed)))
	} else {
		mathrand.Seed(int64(binary.BigEndian.Uint64([]byte(curActionID))))
	}

	tf := &transformer{
		info: &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		},
	}

	origTypesConfig := types.Config{Importer: origImporter}
	tf.pkg, err = origTypesConfig.Check(curPkgPath, fset, files, tf.info)
	if err != nil {
		return nil, fmt.Errorf("typecheck error: %v", err)
	}

	tf.privateNameMap = make(map[string]string)
	tf.existingNames = collectExistingNames(files)
	tf.recordReflectArgs(files)

	if opts.GarbleLiterals {
		// TODO: use transformer here?
		files = literals.Obfuscate(files, tf.info, fset, tf.ignoreObjects)
	}

	tempDir, err := ioutil.TempDir("", "garble-build")
	if err != nil {
		return nil, err
	}
	deferred = append(deferred, func() error {
		return os.RemoveAll(tempDir)
	})

	// Add our temporary dir to the beginning of -trimpath, so that we don't
	// leak temporary dirs. Needs to be at the beginning, since there may be
	// shorter prefixes later in the list, such as $PWD if TMPDIR=$PWD/tmp.
	flags = flagSetValue(flags, "-trimpath", tempDir+"=>;"+trimpath)
	// log.Println(flags)

	detachedComments := make([][]string, len(files))

	for i, file := range files {
		name := filepath.Base(filepath.Clean(paths[i]))
		cgoFile := strings.HasPrefix(name, "_cgo_")

		comments, file := transformLineInfo(file, cgoFile)
		tf.handleDirectives(comments)

		detachedComments[i], files[i] = comments, file
	}

	obfSrcArchive := &bytes.Buffer{}
	obfSrcGzipWriter := gzip.NewWriter(obfSrcArchive)
	defer obfSrcGzipWriter.Close()

	obfSrcTarWriter := tar.NewWriter(obfSrcGzipWriter)
	defer obfSrcTarWriter.Close()

	// TODO: randomize the order and names of the files
	newPaths := make([]string, 0, len(files))
	for i, file := range files {
		origName := filepath.Base(filepath.Clean(paths[i]))
		name := origName
		switch {
		case curPkgPath == "runtime":
			// strip unneeded runtime code
			stripRuntime(origName, file)
		case curPkgPath == "runtime/internal/sys":
			// The first declaration in zversion.go contains the Go
			// version as follows. Replace it here, since the
			// linker's -X does not work with constants.
			//
			//     const TheVersion = `devel ...`
			//
			// Don't touch the source in any other way.
			if origName != "zversion.go" {
				break
			}
			spec := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.ValueSpec)
			lit := spec.Values[0].(*ast.BasicLit)
			lit.Value = "`unknown`"
		case strings.HasPrefix(origName, "_cgo_"):
			// Cgo generated code requires a prefix. Also, don't
			// garble it, since it's just generated code and it gets
			// messy.
			name = "_cgo_" + name
		default:
			file = tf.transformGo(file)

			// Uncomment for some quick debugging. Do not delete.
			// fmt.Fprintf(os.Stderr, "\n-- %s/%s --\n", curPkgPath, origName)
			// if err := printConfig.Fprint(os.Stderr, fset, file); err != nil {
			// 	return nil, err
			// }
		}
		tempFile, err := os.Create(filepath.Join(tempDir, name))
		if err != nil {
			return nil, err
		}
		defer tempFile.Close()

		obfSrc := &bytes.Buffer{}
		printWriter := io.MultiWriter(tempFile, obfSrc)

		for _, comment := range detachedComments[i] {
			if _, err := printWriter.Write([]byte(comment + "\n")); err != nil {
				return nil, err
			}
		}
		if err := printConfig.Fprint(printWriter, fset, file); err != nil {
			return nil, err
		}
		if err := tempFile.Close(); err != nil {
			return nil, err
		}

		if err := obfSrcTarWriter.WriteHeader(&tar.Header{
			Name:    name,
			Mode:    0o755,
			ModTime: time.Now(), // Need for restoring obfuscation time
			Size:    int64(obfSrc.Len()),
		}); err != nil {
			return nil, err
		}
		if _, err := obfSrcTarWriter.Write(obfSrc.Bytes()); err != nil {
			return nil, err
		}

		newPaths = append(newPaths, tempFile.Name())
	}

	objPath := flagValue(flags, "-o")
	deferred = append(deferred, func() error {
		importMap := func(importPath string) (objectPath string) {
			return buildInfo.imports[importPath].packagefile
		}

		pkg, err := goobj2.Parse(objPath, curPkgPath, importMap)
		if err != nil {
			return err
		}

		data, err := json.Marshal(tf.privateNameMap)
		if err != nil {
			return err
		}

		// Adding an extra archive header is safe,
		// and shouldn't break other tools like the linker since our header name is unique
		pkg.ArchiveMembers = append(pkg.ArchiveMembers, goobj2.ArchiveMember{
			ArchiveHeader: goobj2.ArchiveHeader{
				Name: garbleMapHeaderName,
				Size: int64(len(data)),
				Data: data,
			},
		}, goobj2.ArchiveMember{
			ArchiveHeader: goobj2.ArchiveHeader{
				Name: garbleSrcHeaderName,
				Size: int64(obfSrcArchive.Len()),
				Data: obfSrcArchive.Bytes(),
			},
		})

		return pkg.Write(objPath)
	})

	return append(flags, newPaths...), nil
}

// handleDirectives looks at all the comments in a file containing build
// directives, and does the necessary for the obfuscation process to work.
//
// Right now, this means recording what local names are used with go:linkname,
// and rewriting those directives to use obfuscated name from other packages.
func (tf *transformer) handleDirectives(comments []string) {
	for i, comment := range comments {
		if !strings.HasPrefix(comment, "//go:linkname ") {
			continue
		}
		fields := strings.Fields(comment)
		if len(fields) != 3 {
			continue
		}
		// This directive has two arguments: "go:linkname localName newName"
		localName := fields[1]

		// The local name must not be obfuscated.
		obj := tf.pkg.Scope().Lookup(localName)
		if obj != nil {
			tf.ignoreObjects[obj] = true
		}

		// If the new name is of the form "pkgpath.Name", and
		// we've obfuscated "Name" in that package, rewrite the
		// directive to use the obfuscated name.
		newName := strings.Split(fields[2], ".")
		if len(newName) != 2 {
			continue
		}
		pkg, name := newName[0], newName[1]
		if pkg == "runtime" && strings.HasPrefix(name, "cgo") {
			continue // ignore cgo-generated linknames
		}
		listedPkg, ok := buildInfo.imports[pkg]
		if !ok {
			continue // probably a made up symbol name
		}
		garbledPkg, _ := garbledImport(pkg)
		if garbledPkg != nil && garbledPkg.Scope().Lookup(name) != nil {
			continue // the name exists and was not garbled
		}

		// The name exists and was obfuscated; replace the
		// comment with the obfuscated name.
		obfName := hashWith(listedPkg.actionID, name)
		fields[2] = pkg + "." + obfName
		comments[i] = strings.Join(fields, " ")
	}
}

// runtimeRelated is a snapshot of all the packages runtime depends on, or
// packages which the runtime points to via go:linkname.
//
// Once we support go:linkname well and once we can obfuscate the runtime
// package, this entire map can likely go away.
//
// The list was obtained via scripts/runtime-related.sh on Go 1.15.5.
var runtimeRelated = map[string]bool{
	"bufio":                             true,
	"bytes":                             true,
	"compress/flate":                    true,
	"compress/gzip":                     true,
	"context":                           true,
	"encoding/binary":                   true,
	"errors":                            true,
	"fmt":                               true,
	"hash":                              true,
	"hash/crc32":                        true,
	"internal/bytealg":                  true,
	"internal/cpu":                      true,
	"internal/fmtsort":                  true,
	"internal/oserror":                  true,
	"internal/poll":                     true,
	"internal/race":                     true,
	"internal/reflectlite":              true,
	"internal/syscall/execenv":          true,
	"internal/syscall/unix":             true,
	"internal/syscall/windows":          true,
	"internal/syscall/windows/registry": true,
	"internal/syscall/windows/sysdll":   true,
	"internal/testlog":                  true,
	"internal/unsafeheader":             true,
	"io":                                true,
	"io/ioutil":                         true,
	"math":                              true,
	"math/bits":                         true,
	"os":                                true,
	"os/signal":                         true,
	"path/filepath":                     true,
	"plugin":                            true,
	"reflect":                           true,
	"runtime":                           true,
	"runtime/cgo":                       true,
	"runtime/debug":                     true,
	"runtime/internal/atomic":           true,
	"runtime/internal/math":             true,
	"runtime/internal/sys":              true,
	"runtime/pprof":                     true,
	"runtime/trace":                     true,
	"sort":                              true,
	"strconv":                           true,
	"strings":                           true,
	"sync":                              true,
	"sync/atomic":                       true,
	"syscall":                           true,
	"text/tabwriter":                    true,
	"time":                              true,
	"unicode":                           true,
	"unicode/utf16":                     true,
	"unicode/utf8":                      true,
	"unsafe":                            true,
}

// isPrivate checks if GOPRIVATE matches path.
//
// To allow using garble without GOPRIVATE for standalone main packages, it will
// default to not matching standard library packages.
func isPrivate(path string) bool {
	if runtimeRelated[path] {
		return false
	}
	if path == "main" || path == "command-line-arguments" || strings.HasPrefix(path, "plugin/unnamed") {
		// TODO: why don't we see the full package path for main
		// packages? The linker has it at the top of -importcfg, but not
		// the compiler.
		return true
	}
	return module.MatchPrefixPatterns(envGoPrivate, path)
}

// fillBuildInfo initializes the global buildInfo struct via the supplied flags.
func fillBuildInfo(flags []string) error {
	buildID := flagValue(flags, "-buildid")
	switch buildID {
	case "", "true":
		return fmt.Errorf("could not find -buildid argument")
	}
	curActionID = decodeHash(splitActionID(buildID))
	curImportCfg = flagValue(flags, "-importcfg")
	if curImportCfg == "" {
		return fmt.Errorf("could not find -importcfg argument")
	}
	data, err := ioutil.ReadFile(curImportCfg)
	if err != nil {
		return err
	}

	importMap := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, " ")
		if i < 0 {
			continue
		}
		verb := line[:i]
		switch verb {
		case "importmap":
			args := strings.TrimSpace(line[i+1:])
			j := strings.Index(args, "=")
			if j < 0 {
				continue
			}
			beforePath, afterPath := args[:j], args[j+1:]
			importMap[afterPath] = beforePath
		case "packagefile":
			args := strings.TrimSpace(line[i+1:])
			j := strings.Index(args, "=")
			if j < 0 {
				continue
			}
			importPath, objectPath := args[:j], args[j+1:]
			buildID, err := buildidOf(objectPath)
			if err != nil {
				return err
			}
			// log.Println("buildid:", buildID)

			if len(buildInfo.imports) == 0 {
				buildInfo.firstImport = importPath
			}
			impPkg := importedPkg{
				packagefile: objectPath,
				actionID:    decodeHash(splitActionID(buildID)),
			}
			buildInfo.imports[importPath] = impPkg

			if otherPath, ok := importMap[importPath]; ok {
				buildInfo.imports[otherPath] = impPkg
			}
		}
	}
	// log.Printf("%#v", buildInfo)
	return nil
}

// recordReflectArgs collects all the objects in a package which are known to be
// used as arguments to reflect.TypeOf or reflect.ValueOf. Since we obfuscate
// one package at a time, we only detect those if the type definition and the
// reflect usage are both in the same package.
//
// The resulting map mainly contains named types and their field declarations.
func (tf *transformer) recordReflectArgs(files []*ast.File) {
	tf.ignoreObjects = make(map[types.Object]bool)

	visitReflectArg := func(node ast.Node) bool {
		expr, _ := node.(ast.Expr) // info.TypeOf(nil) will just return nil
		named := namedType(tf.info.TypeOf(expr))
		if named == nil {
			return true
		}

		obj := named.Obj()
		if obj == nil || obj.Pkg() != tf.pkg {
			return true
		}
		recordStruct(named, tf.ignoreObjects)

		return true
	}

	visit := func(node ast.Node) bool {
		if opts.GarbleLiterals {
			// TODO: use transformer here?
			literals.RecordUsedAsConstants(node, tf.info, tf.ignoreObjects)
		}

		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fnType := tf.info.ObjectOf(sel.Sel)

		if fnType.Pkg() == nil {
			return true
		}

		if fnType.Pkg().Path() == "reflect" && (fnType.Name() == "TypeOf" || fnType.Name() == "ValueOf") {
			for _, arg := range call.Args {
				ast.Inspect(arg, visitReflectArg)
			}
		}
		return true
	}
	for _, file := range files {
		ast.Inspect(file, visit)
	}
}

// collectExistingNames collects all names, including the names of local
// variables, functions, global fields, etc.
func collectExistingNames(files []*ast.File) map[string]bool {
	names := make(map[string]bool)
	visit := func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			names[ident.Name] = true
		}
		return true
	}
	for _, file := range files {
		ast.Inspect(file, visit)
	}
	return names
}

// transformer holds all the information and state necessary to obfuscate a
// single Go package.
type transformer struct {
	// The type-checking results; the package itself, and the Info struct.
	pkg  *types.Package
	info *types.Info

	// existingNames contains all the existing names in the current package,
	// to avoid name collisions.
	existingNames map[string]bool

	// privateNameMap records how unexported names were obfuscated. For
	// example, "some/pkg.foo" could be mapped to "C" if it was one of the
	// first private names to be obfuscated.
	// TODO: why include the "some/pkg." prefix if it's always going to be
	// the current package?
	privateNameMap map[string]string

	// ignoreObjects records all the objects we cannot obfuscate. An object
	// is any named entity, such as a declared variable or type.
	//
	// So far, this map records:
	//
	//  * Types which are used for reflection; see recordReflectArgs.
	//  * Identifiers used in constant expressions; see RecordUsedAsConstants.
	//  * Identifiers used in go:linkname directives; see handleDirectives.
	//  * Types or variables from external packages which were not
	//    obfuscated, for caching reasons; see transformGo.
	ignoreObjects map[types.Object]bool

	// nameCounter keeps track of how many unique identifier names we've
	// obfuscated, so that the obfuscated names get assigned incrementing
	// short names like "a", "b", "c", etc.
	nameCounter int
}

// transformGo garbles the provided Go syntax node.
func (tf *transformer) transformGo(file *ast.File) *ast.File {
	// Shuffle top level declarations
	mathrand.Shuffle(len(file.Decls), func(i, j int) {
		decl1 := file.Decls[i]
		decl2 := file.Decls[j]

		// Import declarations must remain at the top of the file.
		gd1, iok1 := decl1.(*ast.GenDecl)
		gd2, iok2 := decl2.(*ast.GenDecl)
		if (iok1 && gd1.Tok == token.IMPORT) || (iok2 && gd2.Tok == token.IMPORT) {
			return
		}

		// init function declarations must remain in order.
		fd1, fok1 := decl1.(*ast.FuncDecl)
		fd2, fok2 := decl2.(*ast.FuncDecl)
		if (fok1 && fd1.Name.Name == "init") || (fok2 && fd2.Name.Name == "init") {
			return
		}

		file.Decls[i], file.Decls[j] = decl2, decl1
	})

	pre := func(cursor *astutil.Cursor) bool {
		node, ok := cursor.Node().(*ast.Ident)
		if !ok {
			return true
		}
		if node.Name == "_" {
			return true // unnamed remains unnamed
		}
		if strings.HasPrefix(node.Name, "_C") || strings.Contains(node.Name, "_cgo") {
			return true // don't mess with cgo-generated code
		}
		obj := tf.info.ObjectOf(node)
		if obj == nil {
			return true
		}
		pkg := obj.Pkg()
		if vr, ok := obj.(*types.Var); ok && vr.Embedded() {
			// ObjectOf returns the field for embedded struct
			// fields, not the type it uses. Use the type.
			named := namedType(obj.Type())
			if named == nil {
				return true // unnamed type (probably a basic type, e.g. int)
			}
			obj = named.Obj()
			pkg = obj.Pkg()
		}
		if pkg == nil {
			return true // universe scope
		}

		if pkg.Name() == "main" && obj.Exported() && obj.Parent() == pkg.Scope() {
			// TODO: only do this when -buildmode is plugin? what
			// about other -buildmode options?
			return true // could be a Go plugin API
		}

		// We don't want to obfuscate this object.
		if tf.ignoreObjects[obj] {
			return true
		}

		path := pkg.Path()
		if !isPrivate(path) {
			return true // only private packages are transformed
		}

		// log.Printf("%#v %T", node, obj)
		parentScope := obj.Parent()
		switch x := obj.(type) {
		case *types.Var:
			if parentScope != nil && parentScope != pkg.Scope() {
				// identifiers of non-global variables never show up in the binary
				return true
			}

			// if the struct of this field was not garbled, do not garble
			// any of that struct's fields
			if parentScope != tf.pkg.Scope() && x.IsField() && !x.Embedded() {
				parent, ok := cursor.Parent().(*ast.SelectorExpr)
				if !ok {
					break
				}
				parentType := tf.info.TypeOf(parent.X)
				if parentType == nil {
					break
				}
				named := namedType(parentType)
				if named == nil {
					break
				}
				if name := named.Obj().Name(); strings.HasPrefix(name, "_Ctype") {
					// A field accessor on a cgo type, such as a C struct.
					// We're not obfuscating cgo names.
					return true
				}
				if garbledPkg, _ := garbledImport(path); garbledPkg != nil {
					if garbledPkg.Scope().Lookup(named.Obj().Name()) != nil {
						recordStruct(named, tf.ignoreObjects)
						return true
					}
				}
			}
		case *types.TypeName:
			if parentScope != pkg.Scope() {
				// identifiers of non-global types never show up in the binary
				return true
			}

			// if the type was not garbled in the package were it was defined,
			// do not garble it here
			if parentScope != tf.pkg.Scope() {
				named := namedType(x.Type())
				if named == nil {
					break
				}
				if garbledPkg, _ := garbledImport(path); garbledPkg != nil {
					if garbledPkg.Scope().Lookup(x.Name()) != nil {
						recordStruct(named, tf.ignoreObjects)
						return true
					}
				}
			}
		case *types.Func:
			sign := obj.Type().(*types.Signature)
			if obj.Exported() && sign.Recv() != nil {
				return true // might implement an interface
			}
			if implementedOutsideGo(x) {
				return true // give up in this case
			}
			switch node.Name {
			case "main", "init", "TestMain":
				return true // don't break them
			}
			if strings.HasPrefix(node.Name, "Test") && isTestSignature(sign) {
				return true // don't break tests
			}
		default:
			return true // we only want to rename the above
		}

		// Handle the case where the name is defined in an indirectly
		// imported package. Since only direct imports show up in our
		// importcfg, buildInfo.imports will not initially contain the
		// package path we want.
		//
		// This edge case can happen, for example, if package A imports
		// package B and calls its API, and B's API returns C's struct.
		// Suddenly, A can use struct field names defined in C, even
		// though A never directly imports C.
		//
		// For this rare case, for now, do an extra "go list -toolexec"
		// call to retrieve its export path.
		// TODO: Think about ways to avoid this extra exec call. Perhaps
		// add an extra archive header to record all direct and indirect
		// importcfg data, like we do with private name maps.
		if _, e := buildInfo.imports[path]; !e && path != curPkgPath {
			goArgs := []string{
				"list",
				"-json",
				"-export",
				"-trimpath",
				"-toolexec=" + cache.ExecPath,
			}
			goArgs = append(goArgs, cache.BuildFlags...)
			goArgs = append(goArgs, path)

			cmd := exec.Command("go", goArgs...)
			cmd.Dir = opts.GarbleDir
			out, err := cmd.Output()
			if err != nil {
				if err := err.(*exec.ExitError); err != nil {
					panic(fmt.Sprintf("%v: %s", err, err.Stderr))
				}
				panic(err)
			}
			var pkg listedPackage
			if err := json.Unmarshal(out, &pkg); err != nil {
				panic(err) // shouldn't happen
			}
			buildID, err := buildidOf(pkg.Export)
			if err != nil {
				panic(err) // shouldn't happen
			}
			// Adding it to buildInfo.imports allows us to reuse the
			// "if" branch below. Plus, if this edge case triggers
			// multiple times in a single package compile, we can
			// call "go list" once and cache its result.
			buildInfo.imports[path] = importedPkg{
				packagefile: pkg.Export,
				actionID:    decodeHash(splitActionID(buildID)),
			}
			// log.Printf("fetched indirect dependency %q from: %s", path, pkg.Export)
		}

		actionID := curActionID
		// TODO: Make this check less prone to bugs, like the one we had
		// with indirect dependencies. If "path" is not our current
		// package, then it must exist in buildInfo.imports. Otherwise
		// we should panic.
		if id := buildInfo.imports[path].actionID; len(id) > 0 {
			garbledPkg, err := garbledImport(path)
			if err != nil {
				panic(err) // shouldn't happen
			}
			// Check if the imported name wasn't garbled, e.g. if it's assembly.
			// If the object returned from the garbled package's scope has a different type as the object
			// we're searching for, they are most likely two separate objects with the same name, so ok to garble
			if o := garbledPkg.Scope().Lookup(obj.Name()); o != nil && reflect.TypeOf(o) == reflect.TypeOf(obj) {
				return true
			}
			actionID = id
		}

		origName := node.Name
		_ = origName // used for debug prints below

		// The exported names cannot be shortened as counter synchronization
		// between packages is not currently implemented
		if token.IsExported(node.Name) {
			node.Name = hashWith(actionID, node.Name)
			// log.Printf("%q hashed with %x to %q", origName, actionID, node.Name)
			return true
		}

		fullName := tf.pkg.Path() + "." + node.Name
		if name, ok := tf.privateNameMap[fullName]; ok {
			node.Name = name
			// log.Printf("%q retrieved private name %q for %q", origName, name, path)
			return true
		}

		var name string
		for {
			tf.nameCounter++
			name = encodeIntToName(tf.nameCounter)
			if !tf.existingNames[name] {
				break
			}
		}

		tf.privateNameMap[fullName] = name
		node.Name = name
		// log.Printf("%q assigned private name %q for %q", origName, name, path)
		return true
	}
	return astutil.Apply(file, pre, nil).(*ast.File)
}

// recordStruct adds the given named type to the map, plus all of its fields if
// it is a struct. This function is mainly used for types used via reflection,
// so we want to record their members too.
func recordStruct(named *types.Named, m map[types.Object]bool) {
	m[named.Obj()] = true
	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i := 0; i < strct.NumFields(); i++ {
		m[strct.Field(i)] = true
	}
}

// implementedOutsideGo returns whether a *types.Func does not have a body, for
// example when it's implemented in assembly, or when one uses go:linkname.
//
// Note that this function can only return true if the obj parameter was
// type-checked from source - that is, if it's the top-level package we're
// building. Dependency packages, whose type information comes from export data,
// do not differentiate these "external funcs" in any way.
func implementedOutsideGo(obj *types.Func) bool {
	return obj.Type().(*types.Signature).Recv() == nil &&
		(obj.Scope() != nil && obj.Scope().End() == token.NoPos)
}

// named tries to obtain the *types.Named behind a type, if there is one.
// This is useful to obtain "testing.T" from "*testing.T", or to obtain the type
// declaration object from an embedded field.
func namedType(t types.Type) *types.Named {
	switch t := t.(type) {
	case *types.Named:
		return t
	case interface{ Elem() types.Type }:
		return namedType(t.Elem())
	default:
		return nil
	}
}

// isTestSignature returns true if the signature matches "func _(*testing.T)".
func isTestSignature(sign *types.Signature) bool {
	if sign.Recv() != nil {
		return false
	}
	params := sign.Params()
	if params.Len() != 1 {
		return false
	}
	obj := namedType(params.At(0).Type()).Obj()
	return obj != nil && obj.Pkg().Path() == "testing" && obj.Name() == "T"
}

func transformLink(args []string) ([]string, error) {
	// We can't split by the ".a" extension, because cached object files
	// lack any extension.
	flags, paths := splitFlagsFromArgs(args)

	if err := fillBuildInfo(flags); err != nil {
		return nil, err
	}

	tempDir, err := ioutil.TempDir("", "garble-build")
	if err != nil {
		return nil, err
	}
	deferred = append(deferred, func() error {
		return os.RemoveAll(tempDir)
	})

	// there should only ever be one archive/object file passed to the linker,
	// the file for the main package or entrypoint
	if len(paths) != 1 {
		return nil, fmt.Errorf("expected exactly one link argument")
	}
	importMap := func(importPath string) (objectPath string) {
		return buildInfo.imports[importPath].packagefile
	}
	garbledObj, garbledImports, privateNameMap, err := obfuscateImports(paths[0], tempDir, importMap)
	if err != nil {
		return nil, err
	}

	// Make sure -X works with garbled identifiers. To cover both garbled
	// and non-garbled names, duplicate each flag with a garbled version.
	flagValueIter(flags, "-X", func(val string) {
		// val is in the form of "pkg.name=str"
		i := strings.IndexByte(val, '=')
		if i <= 0 {
			return
		}
		name := val[:i]
		str := val[i+1:]
		j := strings.LastIndexByte(name, '.')
		if j <= 0 {
			return
		}
		pkg := name[:j]
		name = name[j+1:]

		pkgPath := pkg
		if pkgPath == "main" {
			// The main package is known under its import path in
			// the import config map.
			pkgPath = buildInfo.firstImport
		}
		if id := buildInfo.imports[pkgPath].actionID; len(id) > 0 {
			// We use privateNameMap because unexported names are obfuscated
			// to short names like "A", "B", "C" etc, which is not reproducible
			// here. If the name isn't in the map, a hash will do.
			newName, ok := privateNameMap[pkg+"."+name]
			if !ok {
				newName = hashWith(id, name)
			}
			garbledPkg := garbledImports[pkg]
			flags = append(flags, fmt.Sprintf("-X=%s.%s=%s", garbledPkg, newName, str))
		}
	})

	// Ensure we strip the -buildid flag, to not leak any build IDs for the
	// link operation or the main package's compilation.
	flags = flagSetValue(flags, "-buildid", "")

	// Strip debug information and symbol tables.
	flags = append(flags, "-w", "-s")
	return append(flags, garbledObj), nil
}

func splitFlagsFromArgs(all []string) (flags, args []string) {
	for i := 0; i < len(all); i++ {
		arg := all[i]
		if !strings.HasPrefix(arg, "-") {
			return all[:i:i], all[i:]
		}
		if booleanFlags[arg] || strings.Contains(arg, "=") {
			// Either "-bool" or "-name=value".
			continue
		}
		// "-name value", so the next arg is part of this flag.
		i++
	}
	return all, nil
}

// buildFlags is obtained from 'go help build' as of Go 1.15.
var buildFlags = map[string]bool{
	"-a":             true,
	"-n":             true,
	"-p":             true,
	"-race":          true,
	"-msan":          true,
	"-v":             true,
	"-work":          true,
	"-x":             true,
	"-asmflags":      true,
	"-buildmode":     true,
	"-compiler":      true,
	"-gccgoflags":    true,
	"-gcflags":       true,
	"-installsuffix": true,
	"-ldflags":       true,
	"-linkshared":    true,
	"-mod":           true,
	"-modcacherw":    true,
	"-modfile":       true,
	"-pkgdir":        true,
	"-tags":          true,
	"-trimpath":      true,
	"-toolexec":      true,
}

// booleanFlags is obtained from 'go help build' and 'go help testflag' as of Go
// 1.15.
var booleanFlags = map[string]bool{
	// Shared build flags.
	"-a":          true,
	"-i":          true,
	"-n":          true,
	"-v":          true,
	"-x":          true,
	"-race":       true,
	"-msan":       true,
	"-linkshared": true,
	"-modcacherw": true,
	"-trimpath":   true,

	// Test flags (TODO: support its special -args flag)
	"-c":        true,
	"-json":     true,
	"-cover":    true,
	"-failfast": true,
	"-short":    true,
	"-benchmem": true,
}

func filterBuildFlags(flags []string) (filtered []string) {
	for i := 0; i < len(flags); i++ {
		arg := flags[i]
		name := arg
		if i := strings.IndexByte(arg, '='); i > 0 {
			name = arg[:i]
		}

		buildFlag := buildFlags[name]
		if buildFlag {
			filtered = append(filtered, arg)
		}
		if booleanFlags[arg] || strings.Contains(arg, "=") {
			// Either "-bool" or "-name=value".
			continue
		}
		// "-name value", so the next arg is part of this flag.
		if i++; buildFlag && i < len(flags) {
			filtered = append(filtered, flags[i])
		}
	}
	return filtered
}

// splitFlagsFromFiles splits args into a list of flag and file arguments. Since
// we can't rely on "--" being present, and we don't parse all flags upfront, we
// rely on finding the first argument that doesn't begin with "-" and that has
// the extension we expect for the list of paths.
//
// This function only makes sense for lower-level tool commands, such as
// "compile" or "link", since their arguments are predictable.
func splitFlagsFromFiles(all []string, ext string) (flags, paths []string) {
	for i, arg := range all {
		if !strings.HasPrefix(arg, "-") && strings.HasSuffix(arg, ext) {
			return all[:i:i], all[i:]
		}
	}
	return all, nil
}

// flagValue retrieves the value of a flag such as "-foo", from strings in the
// list of arguments like "-foo=bar" or "-foo" "bar". If the flag is repeated,
// the last value is returned.
func flagValue(flags []string, name string) string {
	lastVal := ""
	flagValueIter(flags, name, func(val string) {
		lastVal = val
	})
	return lastVal
}

// flagValueIter retrieves all the values for a flag such as "-foo", like
// flagValue. The difference is that it allows handling complex flags, such as
// those whose values compose a list.
func flagValueIter(flags []string, name string, fn func(string)) {
	for i, arg := range flags {
		if val := strings.TrimPrefix(arg, name+"="); val != arg {
			// -name=value
			fn(val)
		}
		if arg == name { // -name ...
			if i+1 < len(flags) {
				// -name value
				fn(flags[i+1])
			}
		}
	}
}

func flagSetValue(flags []string, name, value string) []string {
	for i, arg := range flags {
		if strings.HasPrefix(arg, name+"=") {
			// -name=value
			flags[i] = name + "=" + value
			return flags
		}
		if arg == name { // -name ...
			if i+1 < len(flags) {
				// -name value
				flags[i+1] = value
				return flags
			}
			return flags
		}
	}
	return append(flags, name+"="+value)
}

func setGoPrivate() error {
	if envGoPrivate == "" {
		// Try 'go env' too, to query ${CONFIG}/go/env as well.
		out, err := exec.Command("go", "env", "GOPRIVATE").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		envGoPrivate = string(bytes.TrimSpace(out))
	}
	// If GOPRIVATE isn't set and we're in a module, use its module
	// path as a GOPRIVATE default. Include a _test variant too.
	if envGoPrivate == "" {
		modpath, err := exec.Command("go", "list", "-m").Output()
		if err == nil {
			path := string(bytes.TrimSpace(modpath))
			envGoPrivate = path + "," + path + "_test"
		}
	}
	// Explicitly set GOPRIVATE, since future garble processes won't
	// query 'go env' again.
	os.Setenv("GOPRIVATE", envGoPrivate)
	return nil
}
