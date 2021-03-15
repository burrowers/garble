// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
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
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/tools/go/ast/astutil"

	"mvdan.cc/garble/internal/literals"
)

var (
	flagSet = flag.NewFlagSet("garble", flag.ContinueOnError)

	version = "(devel)" // to match the default from runtime/debug
)

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
	fset          = token.NewFileSet()
	sharedTempDir = os.Getenv("GARBLE_SHARED")

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

	// Basic information about the package being currently compiled or linked.
	curPkg *listedPackage

	// These are pulled from -importcfg in the current obfuscated build.
	// As such, they contain export data for the dependencies which might be
	// themselves obfuscated, depending on GOPRIVATE.
	importCfgEntries map[string]*importCfgEntry
	garbledImporter  = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(importCfgEntries[path].packagefile)
	}).(types.ImporterFrom)

	opts *flagOptions

	envGoPrivate = os.Getenv("GOPRIVATE") // complemented by 'go env' later
)

func obfuscatedTypesPackage(path string) *types.Package {
	entry, ok := importCfgEntries[path]
	if !ok {
		return nil
	}
	if entry.cachedPkg != nil {
		return entry.cachedPkg
	}
	pkg, err := garbledImporter.ImportFrom(path, opts.GarbleDir, 0)
	if err != nil {
		return nil
	}
	entry.cachedPkg = pkg // cache for later use
	return pkg
}

type importCfgEntry struct {
	packagefile string

	cachedPkg *types.Package
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

			// If the build failed and a random seed was used,
			// the failure might not reproduce with a different seed.
			// Print it before we exit.
			if flagSeed == "random" {
				fmt.Fprintf(os.Stderr, "random seed: %s\n", base64.RawStdEncoding.EncodeToString(opts.Seed))
			}
		}
		return 1
	}
	return 0
}

var errJustExit = errors.New("")

func goVersionOK() bool {
	const (
		minGoVersion       = "v1.16.0"
		suggestedGoVersion = "1.16.x"

		gitTimeFormat = "Mon Jan 2 15:04:05 2006 -0700"
	)
	// Go 1.16 was released on Febuary 16th, 2021.
	minGoVersionDate := time.Date(2021, 2, 16, 0, 0, 0, 0, time.UTC)

	out, err := exec.Command("go", "version").CombinedOutput()
	rawVersion := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(rawVersion, "go version ") {
		fmt.Fprintf(os.Stderr, `Can't get Go version: %v

This is likely due to go not being installed/setup correctly.

How to install Go: https://golang.org/doc/install
`, err)
		return false
	}

	rawVersion = strings.TrimPrefix(rawVersion, "go version ")

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

		fmt.Fprintf(os.Stderr, "Go version %q is too old; please upgrade to Go %s or a newer devel version\n", rawVersion, suggestedGoVersion)
		return false
	}

	version := "v" + strings.TrimPrefix(tag, "go")
	if semver.Compare(version, minGoVersion) < 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too old; please upgrade to Go %s\n", rawVersion, suggestedGoVersion)
		return false
	}

	return true
}

func mainErr(args []string) error {
	// If we recognize an argument, we're not running within -toolexec.
	switch command, args := args[0], args[1:]; command {
	case "help":
		return flag.ErrHelp
	case "version":
		if len(args) > 0 {
			return fmt.Errorf("the version command does not take arguments")
		}
		// don't overwrite the version if it was set by -ldflags=-X
		if info, ok := debug.ReadBuildInfo(); ok && version == "(devel)" {
			mod := &info.Main
			if mod.Replace != nil {
				mod = mod.Replace
			}
			version = mod.Version
		}
		fmt.Println(version)
		return nil
	case "reverse":
		return commandReverse(args)
	case "build", "test":
		cmd, err := toolexecCmd(command, args)
		if err != nil {
			return err
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	if !filepath.IsAbs(args[0]) {
		// -toolexec gives us an absolute path to the tool binary to
		// run, so this is most likely misuse of garble by a user.
		return fmt.Errorf("unknown command: %q", args[0])
	}

	// We're in a toolexec sub-process, not directly called by the user.
	// Load the shared data and wrap the tool, like the compiler or linker.

	if err := loadSharedCache(); err != nil {
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

	toolexecImportPath := os.Getenv("TOOLEXEC_IMPORTPATH")

	// Unfortunately, TOOLEXEC_IMPORTPATH is just "foo/bar" for the package
	// whose ImportPath in "go list -json" is "foo/bar [foo/bar.test]".
	// The ImportPath "foo/bar" also exists in "go list -json", so we can't
	// possibly differentiate between the two versions of a package.
	// The same happens with "foo/bar_test", whose ImportPath is actually
	// "foo/bar_test [foo/bar.test]".
	// We'll likely file this as an upstream bug to fix in Go 1.17.
	//
	// Until then, here's our workaround: since this edge case only happens
	// for the compiler, check if any "_test.go" files are being compiled.
	// If so, we are compiling a test package, so we add the missing extra.
	if tool == "compile" {
		isTestPkg := false
		_, paths := splitFlagsFromFiles(args, ".go")
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				isTestPkg = true
				break
			}
		}
		if isTestPkg {
			forPkg := strings.TrimSuffix(toolexecImportPath, "_test")
			toolexecImportPath = fmt.Sprintf("%s [%s.test]", toolexecImportPath, forPkg)
		}
	}
	curPkg = cache.ListedPackages[toolexecImportPath]
	if curPkg == nil {
		return fmt.Errorf("TOOLEXEC_IMPORTPATH not found in listed packages: %s", toolexecImportPath)
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
	cmd := exec.Command(args[0], transformed...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// toolexecCmd builds an *exec.Cmd which is set up for running "go <command>"
// with -toolexec=garble and the supplied arguments.
//
// Note that it uses and modifies global state; in general, it should only be
// called once from mainErr in the top-level garble process.
func toolexecCmd(command string, args []string) (*exec.Cmd, error) {
	if !goVersionOK() {
		return nil, errJustExit
	}
	// Split the flags from the package arguments, since we'll need
	// to run 'go list' on the same set of packages.
	flags, args := splitFlagsFromArgs(args)
	for _, f := range flags {
		switch f {
		case "-h", "-help", "--help":
			return nil, flag.ErrHelp
		}
	}

	if err := setFlagOptions(); err != nil {
		return nil, err
	}

	// Here is the only place we initialize the cache.
	// The sub-processes will parse it from a shared gob file.
	cache = &sharedCache{Options: *opts}

	// Note that we also need to pass build flags to 'go list', such
	// as -tags.
	cache.BuildFlags = filterBuildFlags(flags)
	if command == "test" {
		cache.BuildFlags = append(cache.BuildFlags, "-test")
	}

	if err := setGoPrivate(); err != nil {
		return nil, err
	}

	var err error
	cache.ExecPath, err = os.Executable()
	if err != nil {
		return nil, err
	}

	if err := setListedPackages(args); err != nil {
		return nil, err
	}

	sharedTempDir, err = saveSharedCache()
	if err != nil {
		return nil, err
	}
	os.Setenv("GARBLE_SHARED", sharedTempDir)
	defer os.Remove(sharedTempDir)

	goArgs := []string{
		command,
		"-trimpath",
		"-toolexec=" + cache.ExecPath,
	}
	if flagDebugDir != "" {
		// In case the user deletes the debug directory,
		// and a previous build is cached,
		// rebuild all packages to re-fill the debug dir.
		goArgs = append(goArgs, "-a")
	}
	if command == "test" {
		// vet is generally not useful on garbled code; keep it
		// disabled by default.
		goArgs = append(goArgs, "-vet=off")
	}
	goArgs = append(goArgs, flags...)
	goArgs = append(goArgs, args...)

	return exec.Command("go", goArgs...), nil
}

var transformFuncs = map[string]func([]string) (args []string, _ error){
	"asm":     transformAsm,
	"compile": transformCompile,
	"link":    transformLink,
}

func transformAsm(args []string) ([]string, error) {
	// If the current package isn't private, we have nothing to do.
	if !curPkg.Private {
		return args, nil
	}

	flags, paths := splitFlagsFromFiles(args, ".s")

	// When assembling, the import path can make its way into the output
	// object file.
	if curPkg.Name != "main" && curPkg.Private {
		flags = flagSetValue(flags, "-p", curPkg.obfuscatedImportPath())
	}

	// We need to replace all function references with their obfuscated name
	// counterparts.
	// Luckily, all func names in Go assembly files are immediately followed
	// by the unicode "middle dot", like:
	//
	//     TEXT ·privateAdd(SB),$0-24
	const middleDot = '·'
	middleDotLen := utf8.RuneLen(middleDot)

	newPaths := make([]string, 0, len(paths))
	for _, path := range paths {

		// Read the entire file into memory.
		// If we find issues with large files, we can use bufio.
		content, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// Find all middle-dot names, and replace them.
		remaining := content
		var buf bytes.Buffer
		for {
			i := bytes.IndexRune(remaining, middleDot)
			if i < 0 {
				buf.Write(remaining)
				remaining = nil
				break
			}
			i += middleDotLen

			buf.Write(remaining[:i])
			remaining = remaining[i:]

			// The name ends at the first rune which cannot be part
			// of a Go identifier, such as a comma or space.
			nameEnd := 0
			for nameEnd < len(remaining) {
				c, size := utf8.DecodeRune(remaining[nameEnd:])
				if !unicode.IsLetter(c) && c != '_' && !unicode.IsDigit(c) {
					break
				}
				nameEnd += size
			}
			name := string(remaining[:nameEnd])
			remaining = remaining[nameEnd:]

			newName := hashWith(curPkg.GarbleActionID, name)
			// log.Printf("%q hashed with %x to %q", name, curPkg.GarbleActionID, newName)
			buf.WriteString(newName)
		}

		// TODO: do the original asm filenames ever matter?
		tempFile, err := ioutil.TempFile(sharedTempDir, "*.s")
		if err != nil {
			return nil, err
		}
		defer tempFile.Close()

		if _, err := tempFile.Write(buf.Bytes()); err != nil {
			return nil, err
		}
		if err := tempFile.Close(); err != nil {
			return nil, err
		}

		newPaths = append(newPaths, tempFile.Name())
	}

	return append(flags, newPaths...), nil
}

func transformCompile(args []string) ([]string, error) {
	var err error
	flags, paths := splitFlagsFromFiles(args, ".go")

	// We will force the linker to drop DWARF via -w, so don't spend time
	// generating it.
	flags = append(flags, "-dwarf=false")

	if (curPkg.ImportPath == "runtime" && opts.Tiny) || curPkg.ImportPath == "runtime/internal/sys" {
		// Even though these packages aren't private, we will still process
		// them later to remove build information and strip code from the
		// runtime. However, we only want flags to work on private packages.
		opts.GarbleLiterals = false
		opts.DebugDir = ""
	} else if !curPkg.Private {
		return append(flags, paths...), nil
	}

	for i, path := range paths {
		if filepath.Base(path) == "_gomod_.go" {
			// never include module info
			paths = append(paths[:i], paths[i+1:]...)
			break
		}
	}

	// If the value of -trimpath doesn't contain the separator ';', the 'go
	// build' command is most likely not using '-trimpath'.
	trimpath := flagValue(flags, "-trimpath")
	if !strings.Contains(trimpath, ";") {
		return nil, fmt.Errorf("-toolexec=garble should be used alongside -trimpath")
	}

	newImportCfg, err := processImportCfg(flags)
	if err != nil {
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

	randSeed := opts.Seed
	if len(randSeed) == 0 {
		randSeed = curPkg.GarbleActionID
	}
	// log.Printf("seeding math/rand with %x\n", randSeed)
	mathrand.Seed(int64(binary.BigEndian.Uint64(randSeed)))

	tf := &transformer{
		info: &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		},
	}

	// The standard library vendors external packages, which results in them
	// listing "golang.org/x/foo" in go list -json's Deps, plus an ImportMap
	// entry to remap them to "vendor/golang.org/x/foo".
	// We support that edge case in listPackage, presumably, though it seems
	// like importer.ForCompiler with a lookup function isn't capable of it.
	// It does work without an explicit lookup func though, which results in
	// extra calls to 'go list'.
	// Since this is a rare edge case and only occurs for a few std
	// packages, do the extra 'go list' calls for now.
	// TODO(mvdan): report this upstream and investigate further.
	if curPkg.Standard && len(curPkg.ImportMap) > 0 {
		origImporter = importer.Default()
	}

	origTypesConfig := types.Config{Importer: origImporter}
	tf.pkg, err = origTypesConfig.Check(curPkg.ImportPath, fset, files, tf.info)
	if err != nil {
		return nil, fmt.Errorf("typecheck error: %v", err)
	}

	tf.recordReflectArgs(files)

	if opts.GarbleLiterals {
		// TODO: use transformer here?
		files = literals.Obfuscate(files, tf.info, fset, tf.ignoreObjects)
	}

	// Add our temporary dir to the beginning of -trimpath, so that we don't
	// leak temporary dirs. Needs to be at the beginning, since there may be
	// shorter prefixes later in the list, such as $PWD if TMPDIR=$PWD/tmp.
	flags = flagSetValue(flags, "-trimpath", sharedTempDir+"=>;"+trimpath)
	// log.Println(flags)

	detachedComments := make([][]string, len(files))

	for i, file := range files {
		name := filepath.Base(filepath.Clean(paths[i]))

		comments, file := tf.transformLineInfo(file, name)
		tf.handleDirectives(comments)

		detachedComments[i], files[i] = comments, file
	}

	// If this is a package to obfuscate, swap the -p flag with the new
	// package path.
	newPkgPath := ""
	if curPkg.Name != "main" && curPkg.Private {
		newPkgPath = curPkg.obfuscatedImportPath()
		flags = flagSetValue(flags, "-p", newPkgPath)
	}

	newPaths := make([]string, 0, len(files))
	for i, file := range files {
		origName := filepath.Base(filepath.Clean(paths[i]))
		name := origName
		switch {
		case curPkg.ImportPath == "runtime":
			// strip unneeded runtime code
			stripRuntime(origName, file)
		case curPkg.ImportPath == "runtime/internal/sys":
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

			ast.Inspect(file, func(node ast.Node) bool {
				imp, ok := node.(*ast.ImportSpec)
				if !ok {
					return true
				}
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					panic(err) // should never happen
				}
				// We're importing an obfuscated package.
				// Replace the import path with its obfuscated version.
				// If the import was unnamed, give it the name of the
				// original package name, to keep references working.
				lpkg, err := listPackage(path)
				if err != nil {
					panic(err) // should never happen
				}
				if !lpkg.Private {
					return true
				}
				newPath := lpkg.obfuscatedImportPath()
				imp.Path.Value = strconv.Quote(newPath)
				if imp.Name == nil {
					imp.Name = &ast.Ident{Name: lpkg.Name}
				}
				return true
			})
		}
		if newPkgPath != "" {
			file.Name.Name = newPkgPath
		}

		// Uncomment for some quick debugging. Do not delete.
		// if curPkg.Private {
		// 	fmt.Fprintf(os.Stderr, "\n-- %s/%s --\n", curPkg.ImportPath, origName)
		// 	if err := printConfig.Fprint(os.Stderr, fset, file); err != nil {
		// 		return nil, err
		// 	}
		// }

		tempFile, err := ioutil.TempFile(sharedTempDir, name+".*.go")
		if err != nil {
			return nil, err
		}
		defer tempFile.Close()

		for _, comment := range detachedComments[i] {
			if _, err := tempFile.Write([]byte(comment + "\n")); err != nil {
				return nil, err
			}
		}
		if err := printConfig.Fprint(tempFile, fset, file); err != nil {
			return nil, err
		}
		if opts.DebugDir != "" {
			osPkgPath := filepath.FromSlash(curPkg.ImportPath)
			pkgDebugDir := filepath.Join(opts.DebugDir, osPkgPath)
			if err := os.MkdirAll(pkgDebugDir, 0o755); err != nil {
				return nil, err
			}

			debugFilePath := filepath.Join(pkgDebugDir, origName)
			debugFile, err := os.Create(debugFilePath)
			if err != nil {
				return nil, err
			}
			if err := printConfig.Fprint(debugFile, fset, file); err != nil {
				return nil, err
			}
			if err := debugFile.Close(); err != nil {
				return nil, err
			}
		}

		if err := tempFile.Close(); err != nil {
			return nil, err
		}

		newPaths = append(newPaths, tempFile.Name())
	}
	flags = flagSetValue(flags, "-importcfg", newImportCfg)

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
		target := strings.Split(fields[2], ".")
		if len(target) != 2 {
			continue
		}
		pkgPath, name := target[0], target[1]
		if pkgPath == "runtime" && strings.HasPrefix(name, "cgo") {
			continue // ignore cgo-generated linknames
		}
		lpkg, err := listPackage(pkgPath)
		if err != nil {
			continue // probably a made up symbol name
		}
		if !lpkg.Private {
			continue // ignore non-private symbols
		}
		obfPkg := obfuscatedTypesPackage(pkgPath)
		if obfPkg != nil && obfPkg.Scope().Lookup(name) != nil {
			continue // the name exists and was not garbled
		}

		// The name exists and was obfuscated; replace the
		// comment with the obfuscated name.
		newName := hashWith(lpkg.GarbleActionID, name)
		newPkgPath := pkgPath
		if pkgPath != "main" {
			newPkgPath = lpkg.obfuscatedImportPath()
		}
		fields[2] = newPkgPath + "." + newName
		comments[i] = strings.Join(fields, " ")
	}
}

// runtimeRelated is a snapshot of all the packages runtime depends on, or
// packages which the runtime points to via go:linkname.
//
// Once we support go:linkname well and once we can obfuscate the runtime
// package, this entire map can likely go away.
//
// The list was obtained via scripts/runtime-related.sh on Go 1.16.
var runtimeRelated = map[string]bool{
	"bufio":                                  true,
	"bytes":                                  true,
	"compress/flate":                         true,
	"compress/gzip":                          true,
	"context":                                true,
	"crypto/x509/internal/macos":             true,
	"encoding/binary":                        true,
	"errors":                                 true,
	"fmt":                                    true,
	"hash":                                   true,
	"hash/crc32":                             true,
	"internal/bytealg":                       true,
	"internal/cpu":                           true,
	"internal/fmtsort":                       true,
	"internal/nettrace":                      true,
	"internal/oserror":                       true,
	"internal/poll":                          true,
	"internal/race":                          true,
	"internal/reflectlite":                   true,
	"internal/singleflight":                  true,
	"internal/syscall/execenv":               true,
	"internal/syscall/unix":                  true,
	"internal/syscall/windows":               true,
	"internal/syscall/windows/registry":      true,
	"internal/syscall/windows/sysdll":        true,
	"internal/testlog":                       true,
	"internal/unsafeheader":                  true,
	"io":                                     true,
	"io/fs":                                  true,
	"math":                                   true,
	"math/bits":                              true,
	"net":                                    true,
	"os":                                     true,
	"os/signal":                              true,
	"path":                                   true,
	"plugin":                                 true,
	"reflect":                                true,
	"runtime":                                true,
	"runtime/cgo":                            true,
	"runtime/debug":                          true,
	"runtime/internal/atomic":                true,
	"runtime/internal/math":                  true,
	"runtime/internal/sys":                   true,
	"runtime/metrics":                        true,
	"runtime/pprof":                          true,
	"runtime/trace":                          true,
	"sort":                                   true,
	"strconv":                                true,
	"strings":                                true,
	"sync":                                   true,
	"sync/atomic":                            true,
	"syscall":                                true,
	"text/tabwriter":                         true,
	"time":                                   true,
	"unicode":                                true,
	"unicode/utf16":                          true,
	"unicode/utf8":                           true,
	"unsafe":                                 true,
	"vendor/golang.org/x/net/dns/dnsmessage": true,
	"vendor/golang.org/x/net/route":          true,
}

// isPrivate checks if a package import path should be considered private,
// meaning that it should be obfuscated.
func isPrivate(path string) bool {
	// We don't support obfuscating these yet.
	if runtimeRelated[path] {
		return false
	}
	// These are main packages, so we must always obfuscate them.
	if path == "command-line-arguments" || strings.HasPrefix(path, "plugin/unnamed") {
		return true
	}
	return module.MatchPrefixPatterns(envGoPrivate, path)
}

// processImportCfg initializes importCfgEntries via the supplied flags, and
// constructs a new importcfg with the obfuscated import paths changed as
// necessary.
func processImportCfg(flags []string) (newImportCfg string, _ error) {
	importCfg := flagValue(flags, "-importcfg")
	if importCfg == "" {
		return "", fmt.Errorf("could not find -importcfg argument")
	}
	data, err := ioutil.ReadFile(importCfg)
	if err != nil {
		return "", err
	}

	importCfgEntries = make(map[string]*importCfgEntry)
	importMap := make(map[string]string)

	for _, line := range strings.SplitAfter(string(data), "\n") {
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

			impPkg := &importCfgEntry{packagefile: objectPath}
			importCfgEntries[importPath] = impPkg

			if otherPath, ok := importMap[importPath]; ok {
				importCfgEntries[otherPath] = impPkg
			}
		}
	}
	// log.Printf("%#v", buildInfo)

	// Produce the modified importcfg file.
	// This is mainly replacing the obfuscated paths.
	// Note that we range over maps, so this is non-deterministic, but that
	// should not matter as the file is treated like a lookup table.
	newCfg, err := ioutil.TempFile(sharedTempDir, "importcfg")
	if err != nil {
		return "", err
	}
	for beforePath, afterPath := range importMap {
		if isPrivate(afterPath) {
			lpkg, err := listPackage(beforePath)
			if err != nil {
				panic(err) // shouldn't happen
			}

			// Note that beforePath is not the canonical path.
			// For beforePath="vendor/foo", afterPath and
			// lpkg.ImportPath can be just "foo".
			// Don't use obfuscatedImportPath here.
			beforePath = hashWith(lpkg.GarbleActionID, beforePath)

			afterPath = lpkg.obfuscatedImportPath()
		}
		fmt.Fprintf(newCfg, "importmap %s=%s\n", beforePath, afterPath)
	}
	for impPath, pkg := range importCfgEntries {
		if isPrivate(impPath) {
			lpkg, err := listPackage(impPath)
			if err != nil {
				panic(err) // shouldn't happen
			}
			impPath = lpkg.obfuscatedImportPath()
		}
		fmt.Fprintf(newCfg, "packagefile %s=%s\n", impPath, pkg.packagefile)
	}

	// Uncomment to debug the transformed importcfg. Do not delete.
	// newCfg.Seek(0, 0)
	// io.Copy(os.Stderr, newCfg)

	if err := newCfg.Close(); err != nil {
		return "", err
	}
	return newCfg.Name(), nil
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

// transformer holds all the information and state necessary to obfuscate a
// single Go package.
type transformer struct {
	// The type-checking results; the package itself, and the Info struct.
	pkg  *types.Package
	info *types.Info

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
}

// transformGo garbles the provided Go syntax node.
func (tf *transformer) transformGo(file *ast.File) *ast.File {
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
		lpkg, err := listPackage(path)
		if err != nil {
			panic(err) // shouldn't happen
		}
		if !lpkg.Private {
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
				if obfPkg := obfuscatedTypesPackage(path); obfPkg != nil {
					if obfPkg.Scope().Lookup(named.Obj().Name()) != nil {
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
				if obfPkg := obfuscatedTypesPackage(path); obfPkg != nil {
					if obfPkg.Scope().Lookup(x.Name()) != nil {
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

		obfPkg := obfuscatedTypesPackage(path)
		// Check if the imported name wasn't garbled.
		// If the object returned from the garbled package's scope has a
		// different type as the object we're searching for, they are
		// most likely two separate objects with the same name, so ok to
		// garble
		if obfPkg == nil {
			// TODO(mvdan): This is probably a bug.
			// Add a test case where an indirect package has a name
			// that we did not obfuscate.
		} else if o := obfPkg.Scope().Lookup(obj.Name()); o != nil && reflect.TypeOf(o) == reflect.TypeOf(obj) {
			return true
		}

		origName := node.Name
		_ = origName // used for debug prints below

		node.Name = hashWith(lpkg.GarbleActionID, node.Name)
		// log.Printf("%q hashed with %x to %q", origName, lpkg.GarbleActionID, node.Name)
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
		return false // test funcs don't have receivers
	}
	params := sign.Params()
	if params.Len() != 1 {
		return false // too many parameters for a test func
	}
	named := namedType(params.At(0).Type())
	if named == nil {
		return false // the only parameter isn't named, like "string"
	}
	obj := named.Obj()
	return obj != nil && obj.Pkg().Path() == "testing" && obj.Name() == "T"
}

func transformLink(args []string) ([]string, error) {
	// We can't split by the ".a" extension, because cached object files
	// lack any extension.
	flags, args := splitFlagsFromArgs(args)

	newImportCfg, err := processImportCfg(flags)
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

		// If the package path is "main", it's the current top-level
		// package we are linking.
		// Otherwise, find it in the cache.
		lpkg := curPkg
		if pkg != "main" {
			lpkg = cache.ListedPackages[pkg]
		}
		if lpkg == nil {
			// We couldn't find the package.
			// Perhaps a typo, perhaps not part of the build.
			// cmd/link ignores those, so we should too.
			return
		}
		// As before, the main package must remain as "main".
		newPkg := pkg
		if pkg != "main" {
			newPkg = lpkg.obfuscatedImportPath()
		}
		newName := hashWith(lpkg.GarbleActionID, name)
		flags = append(flags, fmt.Sprintf("-X=%s.%s=%s", newPkg, newName, str))
	})

	// Ensure we strip the -buildid flag, to not leak any build IDs for the
	// link operation or the main package's compilation.
	flags = flagSetValue(flags, "-buildid", "")

	// Strip debug information and symbol tables.
	flags = append(flags, "-w", "-s")

	flags = flagSetValue(flags, "-importcfg", newImportCfg)
	return append(flags, args...), nil
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
