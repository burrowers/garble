// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"log"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mod/modfile"
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
	flagLiterals bool
	flagTiny     bool
	flagDebugDir string
	flagSeed     seedFlag
)

func init() {
	flagSet.Usage = usage
	flagSet.BoolVar(&flagLiterals, "literals", false, "Obfuscate literals such as strings")
	flagSet.BoolVar(&flagTiny, "tiny", false, "Optimize for binary size, losing some ability to reverse the process")
	flagSet.StringVar(&flagDebugDir, "debugdir", "", "Write the obfuscated source to a directory, e.g. -debugdir=out")
	flagSet.Var(&flagSeed, "seed", "Provide a base64-encoded seed, e.g. -seed=o9WDTZ4CN4w\nFor a random seed, provide -seed=random")
}

type seedFlag struct {
	random bool
	bytes  []byte
}

func (f seedFlag) String() string {
	return base64.RawStdEncoding.EncodeToString(f.bytes)
}

func (f *seedFlag) Set(s string) error {
	if s == "random" {
		f.bytes = make([]byte, 16) // random 128 bit seed
		if _, err := rand.Read(f.bytes); err != nil {
			return fmt.Errorf("error generating random seed: %v", err)
		}
	} else {
		// We expect unpadded base64, but to be nice, accept padded
		// strings too.
		s = strings.TrimRight(s, "=")
		seed, err := base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Errorf("error decoding seed: %v", err)
		}

		if len(seed) < 8 {
			return fmt.Errorf("-seed needs at least 8 bytes, have %d", len(seed))
		}
		f.bytes = seed
	}
	return nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `
Garble obfuscates Go code by wrapping the Go toolchain.

	garble [garble flags] command [go flags] [go arguments]

For example, to build an obfuscated program:

	garble build ./cmd/foo

Similarly, to combine garble flags and Go build flags:

	garble -literals build -tags=purego ./cmd/foo

The following commands are supported:

	build          replace "go build"
	test           replace "go test"
	version        print Garble version
	reverse        de-obfuscate output such as stack traces

To learn more about a command, run "garble help <command>".

garble accepts the following flags before a command:

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
	parentWorkDir = os.Getenv("GARBLE_PARENT_WORK")

	// origImporter is a go/types importer which uses the original versions
	// of packages, without any obfuscation. This is helpful to make
	// decisions on how to obfuscate our input code.
	origImporter = importerWithMap(importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		pkg, err := listPackage(path)
		if err != nil {
			return nil, err
		}
		return os.Open(pkg.Export)
	}).(types.ImporterFrom).ImportFrom)

	// Basic information about the package being currently compiled or linked.
	curPkg *listedPackage

	garbledImporter = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		pkgfile := cachedOutput.KnownObjectFiles[path]
		if pkgfile == "" {
			panic(fmt.Sprintf("missing in cachedOutput.KnownObjectFiles: %q", path))
		}
		return os.Open(pkgfile)
	}).(types.ImporterFrom)
)

type importerWithMap func(path, dir string, mode types.ImportMode) (*types.Package, error)

func (fn importerWithMap) Import(path string) (*types.Package, error) {
	panic("should never be called")
}

func (fn importerWithMap) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path2 := curPkg.ImportMap[path]; path2 != "" {
		path = path2
	}
	return fn(path, dir, mode)
}

func obfuscatedTypesPackage(path string) *types.Package {
	if path == curPkg.ImportPath {
		panic("called obfuscatedTypesPackage on the current package?")
	}
	if pkg := cachedTypesPackages[path]; pkg != nil {
		return pkg
	}
	pkg, err := garbledImporter.ImportFrom(path, parentWorkDir, 0)
	if err != nil {
		panic(err)
	}
	cachedTypesPackages[path] = pkg // cache for later use
	return pkg
}

var cachedTypesPackages = make(map[string]*types.Package)

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
		if code, ok := err.(errJustExit); ok {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, err)

		// If the build failed and a random seed was used,
		// the failure might not reproduce with a different seed.
		// Print it before we exit.
		if flagSeed.random {
			fmt.Fprintf(os.Stderr, "random seed: %s\n", base64.RawStdEncoding.EncodeToString(flagSeed.bytes))
		}
		return 1
	}
	return 0
}

type errJustExit int

func (e errJustExit) Error() string { return fmt.Sprintf("exit: %d", e) }

func goVersionOK() bool {
	const (
		minGoVersionSemver = "v1.17.0"
		suggestedGoVersion = "1.17.x"
	)

	rxVersion := regexp.MustCompile(`go\d+\.\d+(\.\d)?`)
	version := rxVersion.FindString(cache.GoEnv.GOVERSION)
	if version == "" {
		// Go 1.15.x and older do not have GOVERSION yet.
		// We could go the extra mile and fetch it via 'go version',
		// but we'd have to error anyway.
		fmt.Fprintf(os.Stderr, "Go version is too old; please upgrade to Go %s or a newer devel version\n", suggestedGoVersion)
		return false
	}

	versionSemver := "v" + strings.TrimPrefix(version, "go")
	if semver.Compare(versionSemver, minGoVersionSemver) < 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too old; please upgrade to Go %s\n", version, suggestedGoVersion)
		return false
	}

	return true
}

func mainErr(args []string) error {
	// If we recognize an argument, we're not running within -toolexec.
	switch command, args := args[0], args[1:]; command {
	case "help":
		if hasHelpFlag(args) || len(args) > 1 {
			fmt.Fprintf(os.Stderr, "usage: garble help [command]\n")
			return errJustExit(2)
		}
		if len(args) == 1 {
			return mainErr([]string{args[0], "-h"})
		}
		usage()
		return errJustExit(2)
	case "version":
		if hasHelpFlag(args) || len(args) > 0 {
			fmt.Fprintf(os.Stderr, "usage: garble version\n")
			return errJustExit(2)
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

	_, tool := filepath.Split(args[0])
	if runtime.GOOS == "windows" {
		tool = strings.TrimSuffix(tool, ".exe")
	}
	if len(args) == 2 && args[1] == "-V=full" {
		return alterToolVersion(tool, args)
	}

	toolexecImportPath := os.Getenv("TOOLEXEC_IMPORTPATH")

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
	// log.Println(tool, transformed)
	cmd := exec.Command(args[0], transformed...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func hasHelpFlag(flags []string) bool {
	for _, f := range flags {
		switch f {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

// toolexecCmd builds an *exec.Cmd which is set up for running "go <command>"
// with -toolexec=garble and the supplied arguments.
//
// Note that it uses and modifies global state; in general, it should only be
// called once from mainErr in the top-level garble process.
func toolexecCmd(command string, args []string) (*exec.Cmd, error) {
	// Split the flags from the package arguments, since we'll need
	// to run 'go list' on the same set of packages.
	flags, args := splitFlagsFromArgs(args)
	if hasHelpFlag(flags) {
		out, _ := exec.Command("go", command, "-h").CombinedOutput()
		fmt.Fprintf(os.Stderr, `
usage: garble [garble flags] %s [arguments]

This command wraps "go %s". Below is its help:

%s`[1:], command, command, out)
		return nil, errJustExit(2)
	}

	// Here is the only place we initialize the cache.
	// The sub-processes will parse it from a shared gob file.
	cache = &sharedCache{}

	// Note that we also need to pass build flags to 'go list', such
	// as -tags.
	cache.ForwardBuildFlags, _ = filterForwardBuildFlags(flags)
	if command == "test" {
		cache.ForwardBuildFlags = append(cache.ForwardBuildFlags, "-test")
	}

	if err := fetchGoEnv(); err != nil {
		return nil, err
	}

	if !goVersionOK() {
		return nil, errJustExit(1)
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
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	os.Setenv("GARBLE_PARENT_WORK", wd)

	if flagDebugDir != "" {
		if !filepath.IsAbs(flagDebugDir) {
			flagDebugDir = filepath.Join(wd, flagDebugDir)
		}

		if err := os.RemoveAll(flagDebugDir); err == nil || errors.Is(err, fs.ErrExist) {
			err := os.MkdirAll(flagDebugDir, 0o755)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("debugdir error: %v", err)
		}
	}

	// Pass the garble flags down to each toolexec invocation.
	// This way, all garble processes see the same flag values.
	var toolexecFlag strings.Builder
	toolexecFlag.WriteString("-toolexec=")
	toolexecFlag.WriteString(cache.ExecPath)
	appendFlags(&toolexecFlag)

	goArgs := []string{
		command,
		"-trimpath",
		toolexecFlag.String(),
	}
	if flagDebugDir != "" {
		// In case the user deletes the debug directory,
		// and a previous build is cached,
		// rebuild all packages to re-fill the debug dir.
		goArgs = append(goArgs, "-a")
	}
	if command == "test" {
		// vet is generally not useful on obfuscated code; keep it
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
	if !curPkg.ToObfuscate {
		return args, nil // we're not obfuscating this package
	}

	flags, paths := splitFlagsFromFiles(args, ".s")

	// When assembling, the import path can make its way into the output
	// object file.
	if curPkg.Name != "main" && curPkg.ToObfuscate {
		flags = flagSetValue(flags, "-p", curPkg.obfuscatedImportPath())
	}

	flags = alterTrimpath(flags)

	// If the assembler is running just for -gensymabis,
	// don't obfuscate the source, as we are not assembling yet.
	// The assembler will run again later; obfuscating twice is just wasteful.
	symabis := false
	for _, arg := range args {
		if arg == "-gensymabis" {
			symabis = true
			break
		}
	}
	newPaths := make([]string, 0, len(paths))
	if !symabis {
		var newPaths []string
		for _, path := range paths {
			name := filepath.Base(path)
			pkgDir := filepath.Join(sharedTempDir, filepath.FromSlash(curPkg.ImportPath))
			newPath := filepath.Join(pkgDir, name)
			newPaths = append(newPaths, newPath)
		}
		return append(flags, newPaths...), nil
	}

	// We need to replace all function references with their obfuscated name
	// counterparts.
	// Luckily, all func names in Go assembly files are immediately followed
	// by the unicode "middle dot", like:
	//
	//     TEXT ·privateAdd(SB),$0-24
	const middleDot = '·'
	middleDotLen := utf8.RuneLen(middleDot)

	for _, path := range paths {

		// Read the entire file into memory.
		// If we find issues with large files, we can use bufio.
		content, err := os.ReadFile(path)
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

			// We want to replace "OP ·foo" and "OP $·foo",
			// but not "OP somepkg·foo" just yet.
			// "somepkg" is often runtime, syscall, etc.
			// We don't obfuscate any of those for now.
			//
			// TODO: we'll likely need to deal with this
			// when we start obfuscating the runtime.
			// When we do, note that we can't hash with curPkg.
			localName := false
			if i >= 0 {
				switch remaining[i-1] {
				case ' ', '\t', '$':
					localName = true
				}
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

			if !localName {
				buf.WriteString(name)
				continue
			}

			newName := hashWith(curPkg.GarbleActionID, name)
			// log.Printf("%q hashed with %x to %q", name, curPkg.GarbleActionID, newName)
			buf.WriteString(newName)
		}

		// Uncomment for some quick debugging. Do not delete.
		// if curPkg.ToObfuscate {
		// 	fmt.Fprintf(os.Stderr, "\n-- %s --\n%s", path, buf.Bytes())
		// }

		name := filepath.Base(path)
		if path, err := writeTemp(name, buf.Bytes()); err != nil {
			return nil, err
		} else {
			newPaths = append(newPaths, path)
		}
	}

	return append(flags, newPaths...), nil
}

// writeTemp is a mix between os.CreateTemp and os.WriteFile, as it writes a
// named source file in sharedTempDir given an input buffer.
//
// Note that the file is created under a directory tree following curPkg's
// import path, mimicking how files are laid out in modules and GOROOT.
func writeTemp(name string, content []byte) (string, error) {
	pkgDir := filepath.Join(sharedTempDir, filepath.FromSlash(curPkg.ImportPath))
	if err := os.MkdirAll(pkgDir, 0o777); err != nil {
		return "", err
	}
	dstPath := filepath.Join(pkgDir, name)
	if err := writeFileExclusive(dstPath, content); err != nil {
		return "", err
	}
	return dstPath, nil
}

func transformCompile(args []string) ([]string, error) {
	var err error
	flags, paths := splitFlagsFromFiles(args, ".go")

	// We will force the linker to drop DWARF via -w, so don't spend time
	// generating it.
	flags = append(flags, "-dwarf=false")

	for i, path := range paths {
		if filepath.Base(path) == "_gomod_.go" {
			// never include module info
			paths = append(paths[:i], paths[i+1:]...)
			break
		}
	}

	var files []*ast.File
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	tf := newTransformer()
	if err := tf.typecheck(files); err != nil {
		return nil, err
	}

	flags = alterTrimpath(flags)

	// Note that if the file already exists in the cache from another build,
	// we don't need to write to it again thanks to the hash.
	// TODO: as an optimization, just load that one gob file.
	if err := loadCachedOutputs(); err != nil {
		return nil, err
	}

	tf.findReflectFunctions(files)
	newImportCfg, err := processImportCfg(flags)
	if err != nil {
		return nil, err
	}

	if err := writeGobExclusive(
		garbleExportFile(curPkg),
		cachedOutput,
	); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}

	// We can't obfuscate literals in the runtime and its dependencies,
	// because obfuscated literals sometimes escape to heap,
	// and that's not allowed in the runtime itself.
	if runtimeAndDeps[curPkg.ImportPath] {
		flagLiterals = false
	}

	// Literal obfuscation uses math/rand, so seed it deterministically.
	randSeed := flagSeed.bytes
	if len(randSeed) == 0 {
		randSeed = curPkg.GarbleActionID
	}
	// log.Printf("seeding math/rand with %x\n", randSeed)
	mathrand.Seed(int64(binary.BigEndian.Uint64(randSeed)))

	tf.prefillIgnoreObjects(files)
	// log.Println(flags)

	// If this is a package to obfuscate, swap the -p flag with the new
	// package path.
	newPkgPath := ""
	if curPkg.Name != "main" && curPkg.ToObfuscate {
		newPkgPath = curPkg.obfuscatedImportPath()
		flags = flagSetValue(flags, "-p", newPkgPath)
	}

	newPaths := make([]string, 0, len(files))

	for i, file := range files {
		name := filepath.Base(paths[i])
		if curPkg.ImportPath == "runtime" && flagTiny {
			// strip unneeded runtime code
			stripRuntime(name, file)
		}
		if strings.HasPrefix(name, "_cgo_") {
			// Don't obfuscate cgo code, since it's generated and it gets messy.
		} else {
			tf.handleDirectives(file.Comments)
			file = tf.transformGo(file)
		}
		if newPkgPath != "" {
			file.Name.Name = newPkgPath
		}

		src, err := printFile(file)
		if err != nil {
			return nil, err
		}

		// Uncomment for some quick debugging. Do not delete.
		// if curPkg.ToObfuscate {
		// 	fmt.Fprintf(os.Stderr, "\n-- %s/%s --\n%s", curPkg.ImportPath, name, src)
		// }

		if path, err := writeTemp(name, src); err != nil {
			return nil, err
		} else {
			newPaths = append(newPaths, path)
		}
		if flagDebugDir != "" {
			osPkgPath := filepath.FromSlash(curPkg.ImportPath)
			pkgDebugDir := filepath.Join(flagDebugDir, osPkgPath)
			if err := os.MkdirAll(pkgDebugDir, 0o755); err != nil {
				return nil, err
			}

			debugFilePath := filepath.Join(pkgDebugDir, name)
			if err := os.WriteFile(debugFilePath, src, 0o666); err != nil {
				return nil, err
			}
		}
	}
	flags = flagSetValue(flags, "-importcfg", newImportCfg)

	return append(flags, newPaths...), nil
}

// handleDirectives looks at all the comments in a file containing build
// directives, and does the necessary for the obfuscation process to work.
//
// Right now, this means recording what local names are used with go:linkname,
// and rewriting those directives to use obfuscated name from other packages.
func (tf *transformer) handleDirectives(comments []*ast.CommentGroup) {
	for _, group := range comments {
		for _, comment := range group.List {
			if !strings.HasPrefix(comment.Text, "//go:linkname ") {
				continue
			}
			fields := strings.Fields(comment.Text)
			if len(fields) != 3 {
				// TODO: the 2nd argument is optional, handle when it's not present
				continue
			}
			// This directive has two arguments: "go:linkname localName newName"

			// obfuscate the local name, if the current package is obfuscated
			if curPkg.ToObfuscate {
				fields[1] = hashWith(curPkg.GarbleActionID, fields[1])
			}

			// If the new name is of the form "pkgpath.Name", and
			// we've obfuscated "Name" in that package, rewrite the
			// directive to use the obfuscated name.
			newName := fields[2]
			dotCnt := strings.Count(newName, ".")
			if dotCnt < 1 {
				// probably a malformed linkname directive
				continue
			}

			// If the package path has multiple dots, split on the
			// last one.
			lastDotIdx := strings.LastIndex(newName, ".")
			pkgPath, name := newName[:lastDotIdx], newName[lastDotIdx+1:]

			lpkg, err := listPackage(pkgPath)
			if err != nil {
				// probably a made up symbol name, replace the comment
				// in case the local name was obfuscated.
				comment.Text = strings.Join(fields, " ")
				continue
			}
			if lpkg.ToObfuscate {
				// The name exists and was obfuscated; obfuscate
				// the new name.
				newName := hashWith(lpkg.GarbleActionID, name)
				newPkgPath := pkgPath
				if pkgPath != "main" {
					newPkgPath = lpkg.obfuscatedImportPath()
				}
				fields[2] = newPkgPath + "." + newName
			}

			comment.Text = strings.Join(fields, " ")
		}
	}
}

// cannotObfuscate is a list of some packages the runtime depends on, or
// packages which the runtime points to via go:linkname.
//
// Once we support go:linkname well and once we can obfuscate the runtime
// package, this entire map can likely go away.
//
// TODO: investigate and resolve each one of these
var cannotObfuscate = map[string]bool{
	// not a "real" package
	"unsafe": true,

	// some linkname failure
	"time":          true,
	"runtime/pprof": true,

	// all kinds of stuff breaks when obfuscating the runtime
	"syscall":      true,
	"internal/abi": true,

	// rebuilds don't work
	"os/signal": true,

	// cgo breaks otherwise
	"runtime/cgo": true,

	// garble reverse breaks otherwise
	"runtime/debug": true,

	// cgo heavy net doesn't like to be obfuscated
	"net": true,

	// some linkname failure
	"crypto/x509/internal/macos": true,
}

// Obtained from "go list -deps runtime" on Go master (1.18) as of Nov 2021.
// Note that the same command on Go 1.17 results in a subset of this list.
var runtimeAndDeps = map[string]bool{
	"internal/goarch":         true,
	"unsafe":                  true,
	"internal/abi":            true,
	"internal/cpu":            true,
	"internal/bytealg":        true,
	"internal/goexperiment":   true,
	"internal/goos":           true,
	"runtime/internal/atomic": true,
	"runtime/internal/math":   true,
	"runtime/internal/sys":    true,
	"runtime":                 true,
}

// toObfuscate checks if a package should be obfuscated given its import path.
// If you are holding a listedPackage, reuse its ToObfuscate field instead.
func toObfuscate(path string) bool {
	// We don't support obfuscating these yet.
	if cannotObfuscate[path] || runtimeAndDeps[path] {
		return false
	}
	// These are main packages, so we must always obfuscate them.
	if path == "command-line-arguments" || strings.HasPrefix(path, "plugin/unnamed") {
		return true
	}
	return module.MatchPrefixPatterns(cache.GOGARBLE, path)
}

// processImportCfg parses the importcfg file passed to a compile or link step,
// adding missing entries to KnownObjectFiles to be stored in the build cache.
// It also builds a new importcfg file to account for obfuscated import paths.
func processImportCfg(flags []string) (newImportCfg string, _ error) {
	importCfg := flagValue(flags, "-importcfg")
	if importCfg == "" {
		return "", fmt.Errorf("could not find -importcfg argument")
	}
	data, err := os.ReadFile(importCfg)
	if err != nil {
		return "", err
	}

	var packagefiles, importmaps [][2]string

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
			importmaps = append(importmaps, [2]string{beforePath, afterPath})
		case "packagefile":
			args := strings.TrimSpace(line[i+1:])
			j := strings.Index(args, "=")
			if j < 0 {
				continue
			}
			importPath, objectPath := args[:j], args[j+1:]

			packagefiles = append(packagefiles, [2]string{importPath, objectPath})

			if prev := cachedOutput.KnownObjectFiles[importPath]; prev != "" {
				// Nothing to do; recorded by one of our dependencies.
			} else if strings.HasSuffix(objectPath, "_pkg_.a") {
				// The path is inside a temporary directory, to be deleted soon.
				// Record the final location within the build cache instead.
				finalObjectPath, err := gocachePathForFile(objectPath)
				if err != nil {
					return "", err
				}
				cachedOutput.KnownObjectFiles[importPath] = finalObjectPath
			} else {
				cachedOutput.KnownObjectFiles[importPath] = objectPath
			}
		}
	}
	// log.Printf("%#v", buildInfo)

	// Produce the modified importcfg file.
	// This is mainly replacing the obfuscated paths.
	// Note that we range over maps, so this is non-deterministic, but that
	// should not matter as the file is treated like a lookup table.
	newCfg, err := os.CreateTemp(sharedTempDir, "importcfg")
	if err != nil {
		return "", err
	}
	for _, pair := range importmaps {
		beforePath, afterPath := pair[0], pair[1]
		if toObfuscate(afterPath) {
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
	for _, pair := range packagefiles {
		impPath, pkgfile := pair[0], pair[1]
		if toObfuscate(impPath) {
			lpkg, err := listPackage(impPath)
			if err != nil {
				panic(err) // shouldn't happen
			}
			impPath = lpkg.obfuscatedImportPath()
		}
		fmt.Fprintf(newCfg, "packagefile %s=%s\n", impPath, pkgfile)
	}

	// Uncomment to debug the transformed importcfg. Do not delete.
	// newCfg.Seek(0, 0)
	// io.Copy(os.Stderr, newCfg)

	if err := newCfg.Close(); err != nil {
		return "", err
	}
	return newCfg.Name(), nil
}

type (
	funcFullName             = string
	reflectParameterPosition = int
)

// cachedOutput contains information that will be stored as per garbleExportFile.
var cachedOutput = struct {
	// KnownObjectFiles is filled from -importcfg in the current obfuscated build.
	// As such, it records export data for the dependencies which might be
	// themselves obfuscated, depending on GOGARBLE.
	//
	// TODO: We rely on obfuscated type information to know what names we didn't
	// obfuscate. Instead, directly record what names we chose not to obfuscate,
	// which should then avoid having to go through go/types.
	KnownObjectFiles map[string]string

	// KnownReflectAPIs is a static record of what std APIs use reflection on their
	// parameters, so we can avoid obfuscating types used with them.
	//
	// TODO: we're not including fmt.Printf, as it would have many false positives,
	// unless we were smart enough to detect which arguments get used as %#v or %T.
	KnownReflectAPIs map[funcFullName][]reflectParameterPosition
}{
	KnownObjectFiles: map[string]string{},
	KnownReflectAPIs: map[funcFullName][]reflectParameterPosition{
		"reflect.TypeOf":  {0},
		"reflect.ValueOf": {0},
	},
}

// garbleExportFile returns an absolute path to a build cache entry
// which belongs to garble and corresponds to the given Go package.
//
// Unlike pkg.Export, it is only read and written by garble itself.
// Also unlike pkg.Export, it includes GarbleActionID,
// so its path will change if the obfuscated build changes.
//
// The purpose of such a file is to store garble-specific information
// in the build cache, to be reused at a later time.
// The file should have the same lifetime as pkg.Export,
// as it lives under the same cache directory that gets trimmed automatically.
func garbleExportFile(pkg *listedPackage) string {
	trimmed := strings.TrimSuffix(pkg.Export, "-d")
	if trimmed == pkg.Export {
		panic(fmt.Sprintf("unexpected export path of %s: %q", pkg.ImportPath, pkg.Export))
	}
	return trimmed + "-garble-" + hashToString(pkg.GarbleActionID) + "-d"
}

func loadCachedOutputs() error {
	for _, path := range curPkg.Deps {
		pkg, err := listPackage(path)
		if err != nil {
			panic(err) // shouldn't happen
		}
		if pkg.Export == "" {
			continue // nothing to load
		}
		// this function literal is used for the deferred close
		err = func() error {
			filename := garbleExportFile(pkg)
			f, err := os.Open(filename)
			if err != nil {
				return err
			}
			defer f.Close()

			// Decode appends new entries to the existing maps
			if err := gob.NewDecoder(f).Decode(&cachedOutput); err != nil {
				return fmt.Errorf("gob decode: %w", err)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	return nil
}

func (tf *transformer) findReflectFunctions(files []*ast.File) {
	visitReflect := func(node ast.Node) {
		funcDecl, ok := node.(*ast.FuncDecl)
		if !ok {
			return
		}

		funcObj := tf.info.ObjectOf(funcDecl.Name).(*types.Func)

		var paramNames []string
		for _, param := range funcDecl.Type.Params.List {
			for _, name := range param.Names {
				paramNames = append(paramNames, name.Name)
			}
		}

		ast.Inspect(funcDecl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			fnType, _ := tf.info.ObjectOf(sel.Sel).(*types.Func)
			if fnType == nil || fnType.Pkg() == nil {
				return true
			}

			fullName := fnType.FullName()
			var identifiers []string
			for _, argPos := range cachedOutput.KnownReflectAPIs[fullName] {
				arg := call.Args[argPos]

				ident, ok := arg.(*ast.Ident)
				if !ok {
					continue
				}

				obj := tf.info.ObjectOf(ident)
				if obj.Parent() == funcObj.Scope() {
					identifiers = append(identifiers, ident.Name)
				}
			}

			if identifiers == nil {
				return true
			}

			var argumentPosReflect []int
			for _, ident := range identifiers {
				for paramPos, paramName := range paramNames {
					if ident == paramName {
						argumentPosReflect = append(argumentPosReflect, paramPos)
					}
				}
			}
			cachedOutput.KnownReflectAPIs[funcObj.FullName()] = argumentPosReflect

			return true
		})
	}

	lenPrevKnownReflectAPIs := len(cachedOutput.KnownReflectAPIs)
	for _, file := range files {
		for _, decl := range file.Decls {
			visitReflect(decl)
		}
	}

	// if a new reflectAPI is found we need to Re-evaluate all functions which might be using that API
	if len(cachedOutput.KnownReflectAPIs) > lenPrevKnownReflectAPIs {
		tf.findReflectFunctions(files)
	}
}

// prefillIgnoreObjects collects objects which should not be obfuscated,
// such as those used as arguments to reflect.TypeOf or reflect.ValueOf.
// Since we obfuscate one package at a time, we only detect those if the type
// definition and the reflect usage are both in the same package.
func (tf *transformer) prefillIgnoreObjects(files []*ast.File) {
	tf.ignoreObjects = make(map[types.Object]bool)

	visit := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			ident = sel.Sel
		}

		fnType, _ := tf.info.ObjectOf(ident).(*types.Func)
		if fnType == nil || fnType.Pkg() == nil {
			return true
		}

		fullName := fnType.FullName()
		// log.Printf("%s: %s", fset.Position(node.Pos()), fullName)
		for _, argPos := range cachedOutput.KnownReflectAPIs[fullName] {
			arg := call.Args[argPos]
			argType := tf.info.TypeOf(arg)
			tf.recordIgnore(argType, tf.pkg.Path())
		}

		return true
	}
	for _, file := range files {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				name := strings.TrimPrefix(comment.Text, "//export ")
				if name == comment.Text {
					continue
				}
				name = strings.TrimSpace(name)
				obj := tf.pkg.Scope().Lookup(name)
				if obj == nil {
					continue // not found; skip
				}
				tf.ignoreObjects[obj] = true
			}
		}
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
	// This map is initialized by prefillIgnoreObjects at the start,
	// and extra entries from dependencies are added by transformGo,
	// for the sake of caching type lookups.
	// So far, it records:
	//
	//  * Types which are used for reflection.
	//  * Identifiers used in constant expressions.
	//  * Declarations exported via "//export".
	//  * Types or variables from external packages which were not obfuscated.
	ignoreObjects map[types.Object]bool

	// recordTypeDone helps avoid cycles in recordType.
	recordTypeDone map[types.Type]bool

	// fieldToStruct helps locate struct types from any of their field
	// objects. Useful when obfuscating field names.
	fieldToStruct map[*types.Var]*types.Struct

	// fieldToAlias helps tell if an embedded struct field object is a type
	// alias. Useful when obfuscating field names.
	fieldToAlias map[*types.Var]*types.TypeName
}

// newTransformer helps initialize some maps.
func newTransformer() *transformer {
	return &transformer{
		info: &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		},
		recordTypeDone: make(map[types.Type]bool),
		fieldToStruct:  make(map[*types.Var]*types.Struct),
		fieldToAlias:   make(map[*types.Var]*types.TypeName),
	}
}

func (tf *transformer) typecheck(files []*ast.File) error {
	origTypesConfig := types.Config{Importer: origImporter}
	pkg, err := origTypesConfig.Check(curPkg.ImportPath, fset, files, tf.info)
	if err != nil {
		return fmt.Errorf("typecheck error: %v", err)
	}
	tf.pkg = pkg

	// Run recordType on all types reachable via types.Info.
	// A bit hacky, but I could not find an easier way to do this.
	for _, obj := range tf.info.Defs {
		if obj != nil {
			tf.recordType(obj.Type())
		}
	}
	for name, obj := range tf.info.Uses {
		if obj != nil {
			if obj, ok := obj.(*types.TypeName); ok && obj.IsAlias() {
				vr, _ := tf.info.Defs[name].(*types.Var)
				if vr != nil {
					tf.fieldToAlias[vr] = obj
				}
			}
			tf.recordType(obj.Type())
		}
	}
	for _, tv := range tf.info.Types {
		tf.recordType(tv.Type)
	}
	return nil
}

// recordType visits every reachable type after typechecking a package.
// Right now, all it does is fill the fieldToStruct field.
// Since types can be recursive, we need a map to avoid cycles.
func (tf *transformer) recordType(t types.Type) {
	if tf.recordTypeDone[t] {
		return
	}
	tf.recordTypeDone[t] = true
	switch t := t.(type) {
	case interface{ Elem() types.Type }:
		tf.recordType(t.Elem())
	case *types.Named:
		tf.recordType(t.Underlying())
	}
	strct, _ := t.(*types.Struct)
	if strct == nil {
		return
	}
	for i := 0; i < strct.NumFields(); i++ {
		field := strct.Field(i)
		tf.fieldToStruct[field] = strct

		if field.Embedded() {
			tf.recordType(field.Type())
		}
	}
}

// transformGo obfuscates the provided Go syntax file.
func (tf *transformer) transformGo(file *ast.File) *ast.File {
	// Only obfuscate the literals here if the flag is on
	// and if the package in question is to be obfuscated.
	if flagLiterals && curPkg.ToObfuscate {
		file = literals.Obfuscate(file, tf.info, fset, tf.ignoreObjects)
	}

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
			_, isImplicit := tf.info.Defs[node]
			_, parentIsFile := cursor.Parent().(*ast.File)
			if isImplicit && !parentIsFile {
				// In a type switch like "switch foo := bar.(type) {",
				// "foo" is being declared as a symbolic variable,
				// as it is only actually declared in each "case SomeType:".
				//
				// As such, the symbolic "foo" in the syntax tree has no object,
				// but it is still recorded under Defs with a nil value.
				// We still want to obfuscate that syntax tree identifier,
				// so if we detect the case, create a dummy types.Var for it.
				//
				// Note that "package mypkg" also denotes a nil object in Defs,
				// and we don't want to treat that "mypkg" as a variable,
				// so avoid that case by checking the type of cursor.Parent.
				obj = types.NewVar(node.Pos(), tf.pkg, node.Name, nil)
			} else {
				return true
			}
		}
		pkg := obj.Pkg()
		if vr, ok := obj.(*types.Var); ok && vr.Embedded() {
			// The docs for ObjectOf say:
			//
			//     If id is an embedded struct field, ObjectOf returns the
			//     field (*Var) it defines, not the type (*TypeName) it uses.
			//
			// If this embedded field is a type alias, we want to
			// handle the alias's TypeName instead of treating it as
			// the type the alias points to.
			//
			// Alternatively, if we don't have an alias, we want to
			// use the embedded type, not the field.
			if tname := tf.fieldToAlias[vr]; tname != nil {
				if !tname.IsAlias() {
					panic("fieldToAlias recorded a non-alias TypeName?")
				}
				obj = tname
			} else {
				named := namedType(obj.Type())
				if named == nil {
					return true // unnamed type (probably a basic type, e.g. int)
				}
				// If the field embeds an alias,
				// and the field is declared in a dependency,
				// fieldToAlias might not tell us about the alias.
				// We lack the *ast.Ident for the field declaration,
				// so we can't see it in types.Info.Uses.
				//
				// Instead, detect such a "foreign alias embed".
				// If we embed a final named type,
				// but the field name does not match its name,
				// then it must have been done via an alias.
				// We dig out the alias's TypeName via locateForeignAlias.
				if named.Obj().Name() != node.Name {
					tname := locateForeignAlias(vr.Pkg().Path(), node.Name)
					tf.fieldToAlias[vr] = tname // to reuse it later
					obj = tname
				} else {
					obj = named.Obj()
				}
			}
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

		if pkg.Path() == "embed" {
			// The Go compiler needs to detect types such as embed.FS.
			// That will fail if we change the import path or type name.
			// Leave it as is.
			// Luckily, the embed package just declares the FS type.
			return true
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
		if !lpkg.ToObfuscate {
			return true // we're not obfuscating this package
		}
		hashToUse := lpkg.GarbleActionID

		// log.Printf("%s: %#v %T", fset.Position(node.Pos()), node, obj)
		switch obj := obj.(type) {
		case *types.Var:
			if !obj.IsField() {
				// Identifiers denoting variables are always obfuscated.
				break
			}
			// From this point on, we deal with struct fields.

			// Fields don't get hashed with the package's action ID.
			// They get hashed with the type of their parent struct.
			// This is because one struct can be converted to another,
			// as long as the underlying types are identical,
			// even if the structs are defined in different packages.
			//
			// TODO: Consider only doing this for structs where all
			// fields are exported. We only need this special case
			// for cross-package conversions, which can't work if
			// any field is unexported. If that is done, add a test
			// that ensures unexported fields from different
			// packages result in different obfuscated names.
			strct := tf.fieldToStruct[obj]
			if strct == nil {
				panic("could not find for " + node.Name)
			}
			// TODO: We should probably strip field tags here.
			// Do we need to do anything else to make a
			// struct type "canonical"?
			fieldsHash := []byte(strct.String())
			hashToUse = addGarbleToHash(fieldsHash)

			// If the struct of this field was not obfuscated, do not obfuscate
			// any of that struct's fields.
			parent, ok := cursor.Parent().(*ast.SelectorExpr)
			if !ok {
				break
			}
			named := namedType(tf.info.TypeOf(parent.X))
			if named == nil {
				break // TODO(mvdan): add a test
			}
			if name := named.Obj().Name(); strings.HasPrefix(name, "_Ctype") {
				// A field accessor on a cgo type, such as a C struct.
				// We're not obfuscating cgo names.
				return true
			}
			if path != curPkg.ImportPath {
				obfPkg := obfuscatedTypesPackage(path)
				if obfPkg.Scope().Lookup(named.Obj().Name()) != nil {
					tf.recordIgnore(named, path)
					return true
				}
			}
		case *types.TypeName:
			// If the type was not obfuscated in the package were it was defined,
			// do not obfuscate it here.
			if path != curPkg.ImportPath {
				named := namedType(obj.Type())
				if named == nil {
					break // TODO(mvdan): add a test
				}
				obfPkg := obfuscatedTypesPackage(path)
				if obfPkg.Scope().Lookup(obj.Name()) != nil {
					tf.recordIgnore(named, path)
					return true
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

		origName := node.Name
		_ = origName // used for debug prints below

		node.Name = hashWith(hashToUse, node.Name)
		// log.Printf("%q (%T) hashed with %x to %q", origName, obj, hashToUse, node.Name)
		return true
	}
	post := func(cursor *astutil.Cursor) bool {
		imp, ok := cursor.Node().(*ast.ImportSpec)
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
		if !lpkg.ToObfuscate {
			return true
		}
		newPath := lpkg.obfuscatedImportPath()
		imp.Path.Value = strconv.Quote(newPath)
		if imp.Name == nil {
			imp.Name = &ast.Ident{Name: lpkg.Name}
		}
		return true
	}

	return astutil.Apply(file, pre, post).(*ast.File)
}

// locateForeignAlias finds the TypeName for an alias by the name aliasName,
// which must be declared in one of the dependencies of dependentImportPath.
func locateForeignAlias(dependentImportPath, aliasName string) *types.TypeName {
	var found *types.TypeName
	lpkg, err := listPackage(dependentImportPath)
	if err != nil {
		panic(err) // shouldn't happen
	}
	for _, importedPath := range lpkg.Imports {
		pkg2, err := origImporter.ImportFrom(importedPath, parentWorkDir, 0)
		if err != nil {
			panic(err)
		}
		tname, ok := pkg2.Scope().Lookup(aliasName).(*types.TypeName)
		if ok && tname.IsAlias() {
			if found != nil {
				// We assume that the alias is declared exactly
				// once in the set of direct imports.
				// This might not be the case, e.g. if two
				// imports declare the same alias name.
				//
				// TODO: Think how we could solve that
				// efficiently, if it happens in practice.
				panic(fmt.Sprintf("found multiple TypeNames for %s", aliasName))
			}
			found = tname
		}
	}
	if found == nil {
		// This should never happen.
		// If package A embeds an alias declared in a dependency,
		// it must show up in the form of "B.Alias",
		// so A must import B and B must declare "Alias".
		panic(fmt.Sprintf("could not find TypeName for %s", aliasName))
	}
	return found
}

// recordIgnore adds any named types (including fields) under typ to
// ignoreObjects.
//
// Only the names declared in package pkgPath are recorded. This is to ensure
// that reflection detection only happens within the package declaring a type.
// Detecting it in downstream packages could result in inconsistencies.
func (tf *transformer) recordIgnore(t types.Type, pkgPath string) {
	switch t := t.(type) {
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() == nil || obj.Pkg().Path() != pkgPath {
			return // not from the specified package
		}
		if tf.ignoreObjects[obj] {
			return // prevent endless recursion
		}
		tf.ignoreObjects[obj] = true

		// Record the underlying type, too.
		tf.recordIgnore(t.Underlying(), pkgPath)

	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			field := t.Field(i)

			// This check is similar to the one in *types.Named.
			// It's necessary for unnamed struct types,
			// as they aren't named but still have named fields.
			if field.Pkg() == nil || field.Pkg().Path() != pkgPath {
				return // not from the specified package
			}

			// Record the field itself, too.
			tf.ignoreObjects[field] = true

			tf.recordIgnore(field.Type(), pkgPath)
		}

	case interface{ Elem() types.Type }:
		// Get past pointers, slices, etc.
		tf.recordIgnore(t.Elem(), pkgPath)
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

	// Make sure -X works with obfuscated identifiers.
	// To cover both obfuscated and non-obfuscated names,
	// duplicate each flag with a obfuscated version.
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

	// Starting in Go 1.17, Go's version is implicitly injected by the linker.
	// It's the same method as -X, so we can override it with an extra flag.
	flags = append(flags, "-X=runtime.buildVersion=unknown")

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

func alterTrimpath(flags []string) []string {
	// If the value of -trimpath doesn't contain the separator ';', the 'go
	// build' command is most likely not using '-trimpath'.
	trimpath := flagValue(flags, "-trimpath")

	// Add our temporary dir to the beginning of -trimpath, so that we don't
	// leak temporary dirs. Needs to be at the beginning, since there may be
	// shorter prefixes later in the list, such as $PWD if TMPDIR=$PWD/tmp.
	return flagSetValue(flags, "-trimpath", sharedTempDir+"=>;"+trimpath)
}

// forwardBuildFlags is obtained from 'go help build' as of Go 1.17.
var forwardBuildFlags = map[string]bool{
	// These shouldn't be used in nested cmd/go calls.
	"-a": false,
	"-n": false,
	"-x": false,
	"-v": false,

	// These are always set by garble.
	"-trimpath": false,
	"-toolexec": false,

	"-p":             true,
	"-race":          true,
	"-msan":          true,
	"-work":          true,
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
	"-overlay":       true,
}

// booleanFlags is obtained from 'go help build' and 'go help testflag' as of Go 1.17.
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

func filterForwardBuildFlags(flags []string) (filtered []string, firstUnknown string) {
	for i := 0; i < len(flags); i++ {
		arg := flags[i]
		name := arg
		if i := strings.IndexByte(arg, '='); i > 0 {
			name = arg[:i]
		}

		buildFlag := forwardBuildFlags[name]
		if buildFlag {
			filtered = append(filtered, arg)
		} else {
			firstUnknown = name
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
	return filtered, firstUnknown
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

func fetchGoEnv() error {
	out, err := exec.Command("go", "env", "-json",
		"GOPRIVATE", "GOMOD", "GOVERSION", "GOCACHE",
	).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, `Can't find Go toolchain: %v

This is likely due to go not being installed/setup correctly.

How to install Go: https://golang.org/doc/install
`, err)
		return errJustExit(1)
	}
	if err := json.Unmarshal(out, &cache.GoEnv); err != nil {
		return err
	}
	cache.GOGARBLE = os.Getenv("GOGARBLE")
	if cache.GOGARBLE != "" {
		// GOGARBLE is non-empty; nothing to do.
	} else if cache.GoEnv.GOPRIVATE != "" {
		// GOGARBLE is empty and GOPRIVATE is non-empty.
		// Set GOGARBLE to GOPRIVATE's value.
		cache.GOGARBLE = cache.GoEnv.GOPRIVATE
	} else {
		// If GOPRIVATE isn't set and we're in a module, use its module
		// path as a GOPRIVATE default. Include a _test variant too.
		// TODO(mvdan): we shouldn't need the _test variant here,
		// as the import path should not include it; only the package name.
		if mod, err := os.ReadFile(cache.GoEnv.GOMOD); err == nil {
			modpath := modfile.ModulePath(mod)
			if modpath != "" {
				cache.GOGARBLE = modpath + "," + modpath + "_test"
			}
		}
	}
	return nil
}
