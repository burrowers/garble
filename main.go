// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"bytes"
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
	"runtime"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"mvdan.cc/garble/internal/literals"
)

var flagSet = flag.NewFlagSet("garble", flag.ContinueOnError)

var (
	flagGarbleLiterals bool
	flagDebugDir       string
	flagSeed           string
)

func init() {
	flagSet.Usage = usage
	flagSet.BoolVar(&flagGarbleLiterals, "literals", false, "Encrypt all literals with AES, currently only literal strings are supported")
	flagSet.StringVar(&flagDebugDir, "debugdir", "", "Write the garbled source to a given directory: '-debugdir=./debug'")
	flagSet.StringVar(&flagSeed, "seed", "", "Provide a custom base64-encoded seed: '-seed=o9WDTZ4CN4w=' \nFor a random seed provide: '-seed=random'")
}

func usage() {
	fmt.Fprintf(os.Stderr, `
Usage of garble:

garble [flags] build [build flags] [packages]

The tool supports wrapping the following Go commands - run "garble cmd [args]"
instead of "go cmd [args]" to add obfuscation:

	build
	test

garble accepts the following flags:
`[1:])
	flagSet.PrintDefaults()
	os.Exit(2)
}

func main() { os.Exit(main1()) }

var (
	deferred []func() error
	fset     = token.NewFileSet()

	b64         = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_z")
	printConfig = printer.Config{Mode: printer.RawFormat}

	// listPackage helps implement a types.Importer which finds the export
	// data for the original dependencies, not their garbled counterparts.
	// This is useful to typecheck a package before it's garbled, so we can
	// make decisions on how to garble it.
	origTypesConfig = types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		pkg, err := listPackage(path)
		if err != nil {
			return nil, err
		}
		return os.Open(pkg.Export)
	})}

	buildInfo       = packageInfo{imports: make(map[string]importedPkg)}
	garbledImporter = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(buildInfo.imports[path].packagefile)
	}).(types.ImporterFrom)

	envGarbleDir      = os.Getenv("GARBLE_DIR")
	envGarbleLiterals = os.Getenv("GARBLE_LITERALS") == "true"
	envGarbleDebugDir = os.Getenv("GARBLE_DEBUGDIR")
	envGarbleSeed     = os.Getenv("GARBLE_SEED")
	envGoPrivate      string // filled via 'go env' below to support 'go env -w'
	envGarbleListPkgs = os.Getenv("GARBLE_LISTPKGS")

	seed []byte
)

func saveListedPackages(w io.Writer, test bool, patterns ...string) error {
	args := []string{"list", "-json", "-deps", "-export"}
	if test {
		args = append(args, "-test")
	}
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
}

func listPackage(path string) (*listedPackage, error) {
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

type packageInfo struct {
	buildID string // from -buildid

	imports     map[string]importedPkg // from -importcfg
	firstImport string                 // first from -importcfg; the main package when linking
}

type importedPkg struct {
	packagefile string
	buildID     string

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

func mainErr(args []string) error {
	out, err := exec.Command("go", "env", "GOPRIVATE").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	envGoPrivate = string(bytes.TrimSpace(out))

	// If we recognise an argument, we're not running within -toolexec.
	switch cmd := args[0]; cmd {
	case "help":
		flagSet.Usage()
	case "build", "test":
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

		if flagSeed == "random" {
			seed = make([]byte, 16) // random 128 bit seed

			if _, err := rand.Read(seed); err != nil {
				return fmt.Errorf("Error generating random seed: %v", err)
			}

			flagSeed = "random;" + base64.StdEncoding.EncodeToString(seed)
		}
		os.Setenv("GARBLE_SEED", flagSeed)

		if flagDebugDir != "" {
			if !filepath.IsAbs(flagDebugDir) {
				flagDebugDir = filepath.Join(wd, flagDebugDir)
			}

			if info, err := os.Stat(flagDebugDir); os.IsNotExist(err) {
				err := os.MkdirAll(flagDebugDir, 0o755)
				if err != nil {
					return err
				}
			} else if err != nil {
				return fmt.Errorf("debugdir error: %v", err)
			} else if !info.IsDir() {
				return fmt.Errorf("debugdir exists, but is a file not a directory")
			}
		}

		os.Setenv("GARBLE_DEBUGDIR", flagDebugDir)

		// If GOPRIVATE isn't set and we're in a module, use its module
		// path as a GOPRIVATE default. Include a _test variant too.
		if envGoPrivate == "" {
			modpath, err := exec.Command("go", "list", "-m").Output()
			if err == nil {
				path := string(bytes.TrimSpace(modpath))
				os.Setenv("GOPRIVATE", path+","+path+"_test")
			}
		}

		f, err := ioutil.TempFile("", "garble-list-deps")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		// TODO: Pass along flags that 'go list' understands too, such
		// as -mod or -modfile.
		if err := saveListedPackages(f, cmd == "test", args...); err != nil {
			return err
		}
		os.Setenv("GARBLE_LISTPKGS", f.Name())
		if err := f.Close(); err != nil {
			return err
		}

		execPath, err := os.Executable()
		if err != nil {
			return err
		}
		goArgs := []string{
			cmd,
			"-a",
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
	if len(paths) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}
	pkgPath := flagValue(flags, "-p")
	if pkgPath == "runtime" || pkgPath == "runtime/internal/sys" {
		// Even though these packages aren't private, we will still process
		// them later to remove build information and add additional
		// functions to the runtime. However, we only want flags to work on
		// private packages.
		envGarbleLiterals = false
		envGarbleDebugDir = ""
	} else if !isPrivate(pkgPath) {
		return args, nil
	}
	for i, path := range paths {
		if filepath.Base(path) == "_gomod_.go" {
			// never include module info
			paths = append(paths[:i], paths[i+1:]...)
			break
		}
	}
	if len(paths) == 1 && filepath.Base(paths[0]) == "_testmain.go" {
		return args, nil
	}

	// If the value of -trimpath doesn't contain the separator ';', the 'go
	// build' command is most likely not using '-trimpath'.
	trimpath := flagValue(flags, "-trimpath")
	if !strings.Contains(trimpath, ";") {
		return nil, fmt.Errorf("-toolexec=garble should be used alongside -trimpath")
	}
	if err := readBuildIDs(flags); err != nil {
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
			return nil, fmt.Errorf("Error decoding base64 encoded seed: %v", err)
		}

		mathrand.Seed(int64(binary.BigEndian.Uint64(seed)))
	} else {
		mathrand.Seed(int64(binary.BigEndian.Uint64([]byte(buildInfo.buildID))))
	}

	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	pkg, err := origTypesConfig.Check(pkgPath, fset, files, info)
	if err != nil {
		return nil, fmt.Errorf("typecheck error: %v", err)
	}

	blacklist := buildBlacklist(files, info, pkg)

	if envGarbleLiterals {
		files = literals.Obfuscate(files, info, fset, blacklist)
		// ast changed so we need to typecheck again
		pkg, err = origTypesConfig.Check(pkgPath, fset, files, info)
		if err != nil {
			return nil, fmt.Errorf("typecheck error: %v", err)
		}
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
	args = flags

	pkgDebugDir := ""
	if envGarbleDebugDir != "" {
		osPkgPath := filepath.FromSlash(pkgPath)
		pkgDebugDir = filepath.Join(envGarbleDebugDir, osPkgPath)
		if err := os.MkdirAll(pkgDebugDir, 0o755); err != nil {
			return nil, err
		}
	}

	// TODO: randomize the order and names of the files
	for i, file := range files {
		origName := filepath.Base(filepath.Clean(paths[i]))
		name := origName
		switch {
		case pkgPath == "runtime":
			// Add additional runtime API
			addRuntimeAPI(origName, file)
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
			file = transformGo(file, info, blacklist)
			name = fmt.Sprintf("z%d.go", i)

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

		var printWriter io.Writer = tempFile
		var debugFile *os.File
		if pkgDebugDir != "" {
			debugFile, err = os.Create(filepath.Join(pkgDebugDir, name))
			if err != nil {
				return nil, err
			}
			defer debugFile.Close()

			printWriter = io.MultiWriter(tempFile, debugFile)
		}

		if err := printConfig.Fprint(printWriter, fset, file); err != nil {
			return nil, err
		}
		if err := tempFile.Close(); err != nil {
			return nil, err
		}
		debugFile.Close() // this is ok to error if no file is supplied

		args = append(args, tempFile.Name())
	}

	// We will force the linker to drop DWARF via -w, so don't spend time
	// generating it.
	flags = append(flags, "-dwarf=false")
	return args, nil
}

// isPrivate checks if GOPRIVATE matches pkgPath.
//
// To allow using garble without GOPRIVATE for standalone main packages, it will
// default to not matching standard library packages.
func isPrivate(pkgPath string) bool {
	if pkgPath == "main" || strings.HasPrefix(pkgPath, "plugin/unnamed") {
		// TODO: why don't we see the full package path for main
		// packages? The linker has it at the top of -importcfg, but not
		// the compiler.
		return true
	}
	return GlobsMatchPath(envGoPrivate, pkgPath)
}

func readBuildIDs(flags []string) error {
	buildInfo.buildID = flagValue(flags, "-buildid")
	switch buildInfo.buildID {
	case "", "true":
		return fmt.Errorf("could not find -buildid argument")
	}
	buildInfo.buildID = trimBuildID(buildInfo.buildID)
	importcfg := flagValue(flags, "-importcfg")
	if importcfg == "" {
		return fmt.Errorf("could not find -importcfg argument")
	}
	f, err := os.OpenFile(importcfg, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := ioutil.ReadAll(f)
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
		// log.Println("buildid:", fileID)

		if len(buildInfo.imports) == 0 {
			buildInfo.firstImport = importPath
		}
		buildInfo.imports[importPath] = importedPkg{
			packagefile: objectPath,
			buildID:     fileID,
		}
	}
	// log.Printf("%#v", buildInfo)

	if err := f.Close(); err != nil {
		return err
	}
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
	out, err := cmd.Output()
	if err != nil {
		if err, _ := err.(*exec.ExitError); err != nil {
			return "", fmt.Errorf("%v: %s", err, err.Stderr)
		}
		return "", err
	}
	return trimBuildID(string(bytes.TrimSpace(out))), nil
}

func hashWith(salt, value string) string {
	const length = 8

	d := sha256.New()
	io.WriteString(d, salt)
	d.Write(seed)
	io.WriteString(d, value)
	sum := b64.EncodeToString(d.Sum(nil))

	if token.IsExported(value) {
		return "Z" + sum[:length]
	}
	return "z" + sum[:length]
}

// buildBlacklist collects all the objects in a package which are known to be
// used with reflect.TypeOf or reflect.ValueOf. Since we obfuscate one package
// at a time, we only detect those if the type definition and the reflect usage
// are both in the same package.
//
// The blacklist mainly contains named types and their field declarations.
func buildBlacklist(files []*ast.File, info *types.Info, pkg *types.Package) map[types.Object]struct{} {
	blacklist := make(map[types.Object]struct{})

	reflectBlacklist := func(node ast.Node) bool {
		expr, _ := node.(ast.Expr)
		named := namedType(info.TypeOf(expr))
		if named == nil {
			return true
		}

		obj := named.Obj()
		if obj == nil || obj.Pkg() != pkg {
			return true
		}
		blacklist[obj] = struct{}{}

		strct, _ := named.Underlying().(*types.Struct)
		if strct != nil {
			for i := 0; i < strct.NumFields(); i++ {
				blacklist[strct.Field(i)] = struct{}{}
			}
		}

		return true
	}

	visit := func(node ast.Node) bool {
		if envGarbleLiterals {
			literals.ConstBlacklist(node, info, blacklist)
		}

		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fnType := info.ObjectOf(sel.Sel)

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
	return blacklist
}

// transformGo garbles the provided Go syntax node.
func transformGo(file *ast.File, info *types.Info, blacklist map[types.Object]struct{}) *ast.File {
	// Remove all comments, minus the "//go:" compiler directives.
	// The final binary should still not contain comment text, but removing
	// it helps ensure that (and makes position info less predictable).
	origComments := file.Comments
	file.Comments = nil
	for _, commentGroup := range origComments {
		for _, comment := range commentGroup.List {
			if strings.HasPrefix(comment.Text, "//go:") {
				file.Comments = append(file.Comments, &ast.CommentGroup{
					List: []*ast.Comment{comment},
				})
			}
		}
	}

	// Shuffle top level declarations if there are no remaining compiler
	// directives.
	if len(file.Comments) == 0 {
		// TODO: Also allow files with compiler directives.
		mathrand.Shuffle(len(file.Decls), func(i, j int) {
			decl1 := file.Decls[i]
			decl2 := file.Decls[j]

			// Import declarations must remain at the top of the file.
			gd1, ok1 := decl1.(*ast.GenDecl)
			gd2, ok2 := decl2.(*ast.GenDecl)
			if (ok1 && gd1.Tok == token.IMPORT) || (ok2 && gd2.Tok == token.IMPORT) {
				return
			}
			file.Decls[i], file.Decls[j] = decl2, decl1
		})
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
		obj := info.ObjectOf(node)
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
		if _, ok := blacklist[obj]; ok {
			return true
		}

		// log.Printf("%#v %T", node, obj)
		switch x := obj.(type) {
		case *types.Var:
			if x.IsField() && x.Exported() {
				// might be used for reflection, e.g.
				// encoding/json without struct tags
				return true
			}

			if obj.Parent() != pkg.Scope() {
				// identifiers of non-global variables never show up in the binary
				return true
			}

		case *types.TypeName:
			if obj.Parent() != pkg.Scope() {
				// identifiers of non-global types never show up in the binary
				return true
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
		buildID := buildInfo.buildID
		path := pkg.Path()
		if !isPrivate(path) {
			return true // only private packages are transformed
		}
		if id := buildInfo.imports[path].buildID; id != "" {
			garbledPkg, err := garbledImport(path)
			if err != nil {
				panic(err) // shouldn't happen
			}
			// Check if the imported name wasn't
			// garbled, e.g. if it's assembly.
			if garbledPkg.Scope().Lookup(obj.Name()) != nil {
				return true
			}
			buildID = id
		}
		// orig := node.Name
		node.Name = hashWith(buildID, node.Name)
		// log.Printf("%q hashed with %q to %q", orig, buildID, node.Name)
		return true
	}
	return astutil.Apply(file, pre, nil).(*ast.File)
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
	flags, paths := splitFlagsFromFiles(args, ".a")
	if len(paths) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}

	// Make sure -X works with garbled identifiers. To cover both garbled
	// and non-garbled names, duplicate each flag with a garbled version.
	if err := readBuildIDs(flags); err != nil {
		return nil, err
	}
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
		if id := buildInfo.imports[pkgPath].buildID; id != "" {
			name = hashWith(id, name)
			flags = append(flags, fmt.Sprintf("-X=%s.%s=%s", pkg, name, str))
		}
	})

	// Ensure we strip the -buildid flag, to not leak any build IDs for the
	// link operation or the main package's compilation.
	flags = flagSetValue(flags, "-buildid", "")

	// Strip debug information and symbol tables.
	flags = append(flags, "-w", "-s")
	return append(flags, paths...), nil
}

func splitFlagsFromArgs(all []string) (flags, args []string) {
	for i := 0; i < len(all); i++ {
		arg := all[i]
		if !strings.HasPrefix(arg, "-") {
			return all[:i], all[i:]
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
