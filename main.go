// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/gob"
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
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
	"unicode"

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
	os.Exit(2)
}

func main() { os.Exit(main1()) }

var (
	deferred []func() error
	fset     = token.NewFileSet()

	nameCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_z"
	b64         = base64.NewEncoding(nameCharset)
	printConfig = printer.Config{Mode: printer.RawFormat}

	// origImporter configures a go/types importer which uses the original
	// versions of packages, without any obfuscation. This is helpful to
	// make decisions on how to obfuscate our input code.
	origImporter = func(fromPkg string) types.Importer {
		return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			pkg, err := listPackage(fromPkg, path)
			if err != nil {
				return nil, err
			}
			return os.Open(pkg.Export)
		})
	}

	buildInfo = struct {
		actionID  []byte // from -buildid
		importCfg string // from -importcfg

		// TODO: replace part of this with goobj.ParseImportCfg, so that
		// we can also reuse it. For now, parsing ourselves is still
		// necessary so that we can set firstImport.
		imports map[string]importedPkg // parsed importCfg plus cached info

		firstImport string // first from -importcfg; the main package when linking
	}{imports: make(map[string]importedPkg)}

	garbledImporter = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(buildInfo.imports[path].packagefile)
	}).(types.ImporterFrom)

	envGoPrivate = os.Getenv("GOPRIVATE") // complemented by 'go env' later

	envGarbleDir      = os.Getenv("GARBLE_DIR")
	envGarbleLiterals = os.Getenv("GARBLE_LITERALS") == "true"
	envGarbleTiny     = os.Getenv("GARBLE_TINY") == "true"
	envGarbleDebugDir = os.Getenv("GARBLE_DEBUGDIR")
	envGarbleSeed     = os.Getenv("GARBLE_SEED")
	envGarbleListPkgs = os.Getenv("GARBLE_LISTPKGS")

	seed []byte
)

const (
	garbleMapHeaderName = "garble/nameMap"
	garbleSrcHeaderName = "garble/src"
)

func saveListedPackages(w io.Writer, flags, patterns []string) error {
	args := []string{"list", "-json", "-deps", "-export"}
	args = append(args, flags...)
	args = append(args, patterns...)
	cmd := exec.Command("go", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("go list error: %v", err)
	}
	dec := json.NewDecoder(stdout)
	listedPackages = make(map[string]*listedPackage)
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return err
		}
		listedPackages[pkg.ImportPath] = &pkg
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go list error: %v: %s", err, stderr.Bytes())
	}
	if err := gob.NewEncoder(w).Encode(listedPackages); err != nil {
		return err
	}
	return nil
}

// listedPackages contains data obtained via 'go list -json -export -deps'. This
// allows us to obtain the non-garbled export data of all dependencies, useful
// for type checking of the packages as we obfuscate them.
//
// Note that we obtain this data once in saveListedPackages, store it into a
// temporary file via gob encoding, and then reuse that file in each of the
// garble processes that wrap a package compilation.
var listedPackages map[string]*listedPackage

type listedPackage struct {
	ImportPath string
	Export     string
	Deps       []string
	ImportMap  map[string]string

	// TODO(mvdan): reuse this field once TOOLEXEC_IMPORTPATH is used
	private bool
}

func listPackage(fromPath, path string) (*listedPackage, error) {
	if listedPackages == nil {
		f, err := os.Open(envGarbleListPkgs)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err := gob.NewDecoder(f).Decode(&listedPackages); err != nil {
			return nil, err
		}
	}
	pkg, ok := listedPackages[path]
	if !ok {
		if fromPkg, ok := listedPackages[fromPath]; ok {
			if path2 := fromPkg.ImportMap[path]; path2 != "" {
				return listPackage(fromPath, path2)
			}
		}
		return nil, fmt.Errorf("path not found in listed packages: %s", path)
	}
	return pkg, nil
}

func garbledImport(path string) (*types.Package, error) {
	ipkg, ok := buildInfo.imports[path]
	if !ok {
		return nil, fmt.Errorf("could not find imported package %q", path)
	}
	if ipkg.pkg != nil {
		return ipkg.pkg, nil // cached
	}
	if envGarbleDir == "" {
		return nil, fmt.Errorf("$GARBLE_DIR unset; did you run via 'garble build'?")
	}
	pkg, err := garbledImporter.ImportFrom(path, envGarbleDir, 0)
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
		flagSet.Usage()
	}
	if err := mainErr(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

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
		flagSet.Usage()
	case "build", "test":
		if !goVersionOK() {
			os.Exit(1)
		}
		// Split the flags from the package arguments, since we'll need
		// to run 'go list' on the same set of packages.
		flags, args := splitFlagsFromArgs(args[1:])
		for _, flag := range flags {
			switch flag {
			case "-h", "-help", "--help":
				flagSet.Usage()
			}
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		os.Setenv("GARBLE_DIR", wd)
		os.Setenv("GARBLE_LITERALS", fmt.Sprint(flagGarbleLiterals))
		os.Setenv("GARBLE_TINY", fmt.Sprint(flagGarbleTiny))

		if flagSeed == "random" {
			seed = make([]byte, 16) // random 128 bit seed

			if _, err := rand.Read(seed); err != nil {
				return fmt.Errorf("error generating random seed: %v", err)
			}

			flagSeed = "random;" + base64.StdEncoding.EncodeToString(seed)
		} else {
			flagSeed = strings.TrimRight(flagSeed, "=")
			seed, err := base64.RawStdEncoding.DecodeString(flagSeed)
			if err != nil {
				return fmt.Errorf("error decoding seed: %v", err)
			}

			if len(seed) != 0 && len(seed) < 8 {
				return fmt.Errorf("the seed needs to be at least 8 bytes, but is only %v bytes", len(seed))
			}

			flagSeed = base64.StdEncoding.EncodeToString(seed)
		}

		os.Setenv("GARBLE_SEED", flagSeed)

		if flagDebugDir != "" {
			if !filepath.IsAbs(flagDebugDir) {
				flagDebugDir = filepath.Join(wd, flagDebugDir)
			}

			if err := os.RemoveAll(flagDebugDir); err == nil || os.IsNotExist(err) {
				err := os.MkdirAll(flagDebugDir, 0o755)
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("debugdir error: %v", err)
			}
		}

		os.Setenv("GARBLE_DEBUGDIR", flagDebugDir)

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

		f, err := ioutil.TempFile("", "garble-list-deps")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())

		// Note that we also need to pass build flags to 'go list', such
		// as -tags.
		listFlags := filterBuildFlags(flags)
		if cmd == "test" {
			listFlags = append(listFlags, "-test")
		}
		if err := saveListedPackages(f, listFlags, args); err != nil {
			return err
		}
		os.Setenv("GARBLE_LISTPKGS", f.Name())
		if err := f.Close(); err != nil {
			return err
		}
		anyPrivate := false
		for path, pkg := range listedPackages {
			if isPrivate(path) {
				pkg.private = true
				anyPrivate = true
			}
		}
		if !anyPrivate {
			return fmt.Errorf("GOPRIVATE=%q does not match any packages to be built", envGoPrivate)
		}
		for path, pkg := range listedPackages {
			if pkg.private {
				continue
			}
			for _, depPath := range pkg.Deps {
				if listedPackages[depPath].private {
					return fmt.Errorf("public package %q can't depend on obfuscated package %q (matched via GOPRIVATE=%q)",
						path, depPath, envGoPrivate)
				}
			}
		}

		execPath, err := os.Executable()
		if err != nil {
			return err
		}
		goArgs := []string{
			cmd,
			"-trimpath",
			"-toolexec=" + execPath,
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

	_, tool := filepath.Split(args[0])
	if runtime.GOOS == "windows" {
		tool = strings.TrimSuffix(tool, ".exe")
	}
	transform := transformFuncs[tool]
	transformed := args[1:]
	// log.Println(tool, transformed)
	if transform != nil {
		if len(args) == 2 && args[1] == "-V=full" {
			cmd := exec.Command(args[0], args[1:]...)
			out, err := cmd.Output()
			if err != nil {
				if err, _ := err.(*exec.ExitError); err != nil {
					return fmt.Errorf("%v: %s", err, err.Stderr)
				}
				return err
			}
			line := string(bytes.TrimSpace(out)) // no trailing newline
			f := strings.Fields(line)
			if len(f) < 3 || f[0] != tool || f[1] != "version" || f[2] == "devel" && !strings.HasPrefix(f[len(f)-1], "buildID=") {
				return fmt.Errorf("%s -V=full: unexpected output:\n\t%s", args[0], line)
			}
			var toolID []byte
			if f[2] == "devel" {
				// On the development branch, use the content ID part of the build ID.
				toolID = decodeHash(contentID(f[len(f)-1]))
			} else {
				// For a release, the output is like: "compile version go1.9.1 X:framepointer".
				// Use the whole line.
				toolID = []byte(line)
			}

			contentID, err := ownContentID(toolID)
			if err != nil {
				return fmt.Errorf("cannot obtain garble's own version: %v", err)
			}
			// The part of the build ID that matters is the last, since it's the
			// "content ID" which is used to work out whether there is a need to redo
			// the action (build) or not. Since cmd/go parses the last word in the
			// output as "buildID=...", we simply add "+garble buildID=_/_/_/${hash}".
			// The slashes let us imitate a full binary build ID, but we assume that
			// the other components such as the action ID are not necessary, since the
			// only reader here is cmd/go and it only consumes the content ID.
			fmt.Printf("%s +garble buildID=_/_/_/%s\n", line, contentID)
			return nil
		}
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

const buildIDSeparator = "/"

// actionID returns the action ID half of a build ID, the first element.
func actionID(buildID string) string {
	i := strings.Index(buildID, buildIDSeparator)
	if i < 0 {
		return buildID
	}
	return buildID[:i]
}

// contentID returns the content ID half of a build ID, the last element.
func contentID(buildID string) string {
	return buildID[strings.LastIndex(buildID, buildIDSeparator)+1:]
}

// decodeHash isthe opposite of hashToString, but with a panic for error
// handling since it should never happen.
func decodeHash(str string) []byte {
	h, err := base64.RawURLEncoding.DecodeString(str)
	if err != nil {
		panic(fmt.Sprintf("invalid hash %q: %v", str, err))
	}
	return h
}

// hashToString encodes the first 120 bits of a sha256 sum in base64, the same
// format used for elements in a build ID.
func hashToString(h []byte) string {
	return base64.RawURLEncoding.EncodeToString(h[:15])
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

	pkgPath := flagValue(flags, "-p")
	if (pkgPath == "runtime" && envGarbleTiny) || pkgPath == "runtime/internal/sys" {
		// Even though these packages aren't private, we will still process
		// them later to remove build information and strip code from the
		// runtime. However, we only want flags to work on private packages.
		envGarbleLiterals = false
		envGarbleDebugDir = ""
	} else if !isPrivate(pkgPath) {
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

	if envGarbleSeed != "" {
		seed, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(envGarbleSeed, "random;"))
		if err != nil {
			return nil, fmt.Errorf("error decoding base64 seed: %v", err)
		}

		mathrand.Seed(int64(binary.BigEndian.Uint64(seed)))
	} else {
		mathrand.Seed(int64(binary.BigEndian.Uint64([]byte(buildInfo.actionID))))
	}

	tf := &transformer{
		info: &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		},
	}

	origTypesConfig := types.Config{
		Importer: origImporter(pkgPath),
	}
	tf.pkg, err = origTypesConfig.Check(pkgPath, fset, files, tf.info)
	if err != nil {
		return nil, fmt.Errorf("typecheck error: %v", err)
	}

	tf.privateNameMap = make(map[string]string)
	tf.existingNames = collectNames(files)
	tf.buildBlacklist(files)

	if envGarbleLiterals {
		// TODO: use transformer here?
		files = literals.Obfuscate(files, tf.info, fset, tf.blacklist)
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
		fileDetachedComments, localNameBlacklist, file := transformLineInfo(file, cgoFile)
		for _, name := range localNameBlacklist {
			obj := tf.pkg.Scope().Lookup(name)
			if obj != nil {
				tf.blacklist[obj] = struct{}{}
			}
		}
		detachedComments[i] = fileDetachedComments
		files[i] = file
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
		case pkgPath == "runtime":
			// strip unneeded runtime code
			stripRuntime(origName, file)
		case pkgPath == "runtime/internal/sys":
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
			// fmt.Fprintf(os.Stderr, "\n-- %s/%s --\n", pkgPath, origName)
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

		fileDetachedComments := detachedComments[i]
		if len(fileDetachedComments) > 0 {
			for _, comment := range fileDetachedComments {
				if _, err = printWriter.Write([]byte(comment + "\n")); err != nil {
					return nil, err
				}
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

		pkg, err := goobj2.Parse(objPath, pkgPath, importMap)
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

// neverPrivate is a core set of std packages which we cannot obfuscate.
// At the moment, this is the runtime package and its (few) dependencies.
// This might change in the future.
const neverPrivate = "runtime,internal/cpu,internal/bytealg,unsafe"

// isPrivate checks if GOPRIVATE matches path.
//
// To allow using garble without GOPRIVATE for standalone main packages, it will
// default to not matching standard library packages.
func isPrivate(path string) bool {
	if module.MatchPrefixPatterns(neverPrivate, path) {
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
	buildInfo.actionID = decodeHash(actionID(buildID))
	buildInfo.importCfg = flagValue(flags, "-importcfg")
	if buildInfo.importCfg == "" {
		return fmt.Errorf("could not find -importcfg argument")
	}
	data, err := ioutil.ReadFile(buildInfo.importCfg)
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
				actionID:    decodeHash(actionID(buildID)),
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

func buildidOf(path string) (string, error) {
	cmd := exec.Command("go", "tool", "buildid", path)
	out, err := cmd.Output()
	if err != nil {
		if err, _ := err.(*exec.ExitError); err != nil {
			return "", fmt.Errorf("%v: %s", err, err.Stderr)
		}
		return "", err
	}
	return string(out), nil
}

func hashWith(salt []byte, name string) string {
	const length = 4

	d := sha256.New()
	d.Write(salt)
	d.Write(seed)
	io.WriteString(d, name)
	sum := b64.EncodeToString(d.Sum(nil))

	if token.IsExported(name) {
		return "Z" + sum[:length]
	}
	return "z" + sum[:length]
}

func buildNameCharset() []rune {
	var charset []rune

	for _, r := range unicode.Letter.R16 {
		for c := r.Lo; c <= r.Hi; c += r.Stride {
			charset = append(charset, rune(c))
		}
	}

	for _, r := range unicode.Digit.R16 {
		for c := r.Lo; c <= r.Hi; c += r.Stride {
			charset = append(charset, rune(c))
		}
	}

	return charset
}

var privateNameCharset = buildNameCharset()

func encodeIntToName(i int) string {
	builder := strings.Builder{}
	for i > 0 {
		charIdx := i % len(privateNameCharset)
		i -= charIdx + 1
		c := privateNameCharset[charIdx]
		if builder.Len() == 0 && !unicode.IsLetter(c) {
			builder.WriteByte('_')
		}
		builder.WriteRune(c)
	}
	return builder.String()
}

// buildBlacklist collects all the objects in a package which are known to be
// used with reflect.TypeOf or reflect.ValueOf. Since we obfuscate one package
// at a time, we only detect those if the type definition and the reflect usage
// are both in the same package.
//
// The blacklist mainly contains named types and their field declarations.
func (tf *transformer) buildBlacklist(files []*ast.File) {
	tf.blacklist = make(map[types.Object]struct{})

	reflectBlacklist := func(node ast.Node) bool {
		expr, _ := node.(ast.Expr) // info.TypeOf(nil) will just return nil
		named := namedType(tf.info.TypeOf(expr))
		if named == nil {
			return true
		}

		obj := named.Obj()
		if obj == nil || obj.Pkg() != tf.pkg {
			return true
		}
		blacklistStruct(named, tf.blacklist)

		return true
	}

	visit := func(node ast.Node) bool {
		if envGarbleLiterals {
			// TODO: use transformer here?
			literals.ConstBlacklist(node, tf.info, tf.blacklist)
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
				ast.Inspect(arg, reflectBlacklist)
			}
		}
		return true
	}
	for _, file := range files {
		ast.Inspect(file, visit)
	}
}

// collectNames collects all names, including the names of local variables,
// functions, global fields, etc.
func collectNames(files []*ast.File) map[string]struct{} {
	blacklist := make(map[string]struct{})
	visit := func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			blacklist[ident.Name] = struct{}{}
		}
		return true
	}
	for _, file := range files {
		ast.Inspect(file, visit)
	}
	return blacklist
}

// transformer holds all the information and state necessary to obfuscate a
// single Go package.
type transformer struct {
	// The type-checking results; the package itself, and the Info struct.
	pkg  *types.Package
	info *types.Info

	// Maps to keep track of how, or whether not, we should obfuscate
	// certain parts of the package.
	// TODO: document better and use better field names; see issue #169.
	blacklist      map[types.Object]struct{}
	privateNameMap map[string]string
	existingNames  map[string]struct{}

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

		// The object itself is blacklisted, e.g. a type definition.
		if _, ok := tf.blacklist[obj]; ok {
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
			if (parentScope != tf.pkg.Scope()) && (x.IsField() && !x.Embedded()) {
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
				if _, ok := buildInfo.imports[path]; ok {
					garbledPkg, err := garbledImport(path)
					if err != nil {
						panic(err) // shouldn't happen
					}
					if garbledPkg.Scope().Lookup(named.Obj().Name()) != nil {
						blacklistStruct(named, tf.blacklist)
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
				if _, ok := buildInfo.imports[path]; ok {
					garbledPkg, err := garbledImport(path)
					if err != nil {
						panic(err) // shouldn't happen
					}
					if garbledPkg.Scope().Lookup(x.Name()) != nil {
						blacklistStruct(named, tf.blacklist)
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
		actionID := buildInfo.actionID
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

		// The exported names cannot be shortened as counter synchronization between packages is not currently implemented
		if token.IsExported(node.Name) {
			node.Name = hashWith(actionID, node.Name)
			return true
		}

		fullName := tf.pkg.Path() + "." + node.Name
		if name, ok := tf.privateNameMap[fullName]; ok {
			node.Name = name
			return true
		}

		var name string
		for {
			tf.nameCounter++
			name = encodeIntToName(tf.nameCounter)
			if _, ok := tf.existingNames[name]; !ok {
				break
			}
		}

		// orig := node.Name
		tf.privateNameMap[fullName] = name
		node.Name = name
		// log.Printf("%q hashed with %q to %q", orig, actionID, node.Name)
		return true
	}
	return astutil.Apply(file, pre, nil).(*ast.File)
}

func blacklistStruct(named *types.Named, blacklist map[types.Object]struct{}) {
	blacklist[named.Obj()] = struct{}{}
	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i := 0; i < strct.NumFields(); i++ {
		blacklist[strct.Field(i)] = struct{}{}
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
	garbledObj, garbledImports, privateNameMap, err := obfuscateImports(paths[0], tempDir, buildInfo.importCfg, importMap)
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
			// If the name is not in the map file, it means that the name was not obfuscated or is public
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

func ownContentID(toolID []byte) (string, error) {
	// We can't rely on the module version to exist, because it's
	// missing in local builds without 'go get'.
	// For now, use 'go tool buildid' on the binary that's running. Just
	// like Go's own cache, we use hex-encoded sha256 sums.
	// Once https://github.com/golang/go/issues/37475 is fixed, we
	// can likely just use that.
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	buildID, err := buildidOf(path)
	if err != nil {
		return "", err
	}
	ownID := decodeHash(contentID(buildID))

	// Join the two content IDs together into a single base64-encoded sha256
	// sum. This includes the original tool's content ID, and garble's own
	// content ID.
	h := sha256.New()
	h.Write(toolID)
	h.Write(ownID)

	// We also need to add the selected options to the full version string,
	// because all of them result in different output. We use spaces to
	// separate the env vars and flags, to reduce the chances of collisions.
	if envGoPrivate != "" {
		fmt.Fprintf(h, " GOPRIVATE=%s", envGoPrivate)
	}
	if envGarbleLiterals {
		fmt.Fprintf(h, " -literals")
	}
	if envGarbleTiny {
		fmt.Fprintf(h, " -tiny")
	}
	if envGarbleSeed != "" {
		fmt.Fprintf(h, " -seed=%x", envGarbleSeed)
	}

	return hashToString(h.Sum(nil)), nil
}
