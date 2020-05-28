// Copyright (c) 2019, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"bytes"
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
	"runtime"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

var flagSet = flag.NewFlagSet("garble", flag.ContinueOnError)

func init() { flagSet.Usage = usage }

func usage() {
	fmt.Fprintf(os.Stderr, `
Usage of garble:

	garble build [build flags] [packages]

The tool supports wrapping the following Go commands - run "garble cmd [args]"
instead of "go cmd [args]" to add obfuscation:

	build
	test

garble does not have flags of its own at this moment.
`[1:])
	flagSet.PrintDefaults()
	os.Exit(2)
}

func main() { os.Exit(main1()) }

var (
	deferred []func() error
	fset     = token.NewFileSet()

	b64             = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_z")
	printerConfig   = printer.Config{Mode: printer.RawFormat}
	origTypesConfig = types.Config{Importer: importer.ForCompiler(fset, "gc", origLookup)}

	buildInfo       = packageInfo{imports: make(map[string]importedPkg)}
	garbledImporter = importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(buildInfo.imports[path].packagefile)
	}).(types.ImporterFrom)

	envGarbleDir = os.Getenv("GARBLE_DIR")
	envGoPrivate string // filled via 'go env' below to support 'go env -w'
)

// origLookup helps implement a types.Importer which finds the export data for
// the original dependencies, not their garbled counterparts. This is useful to
// typecheck a package before it's garbled, so we can make decisions on how to
// garble it.
func origLookup(path string) (io.ReadCloser, error) {
	cmd := exec.Command("go", "list", "-json", "-export", path)
	if envGarbleDir == "" {
		return nil, fmt.Errorf("$GARBLE_DIR unset; did you run via 'garble build'?")
	}
	cmd.Dir = envGarbleDir
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
		if len(args) > 1 {
			switch args[1] {
			case "-h", "-help", "--help":
				flagSet.Usage()
			}
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		os.Setenv("GARBLE_DIR", wd)

		// If GOPRIVATE isn't set and we're in a module, use its module
		// path as a GOPRIVATE default. Include a _test variant too.
		if envGoPrivate == "" {
			modpath, err := exec.Command("go", "list", "-m").CombinedOutput()
			if err == nil {
				path := string(bytes.TrimSpace(modpath))
				os.Setenv("GOPRIVATE", path+","+path+"_test")
			}
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
		goArgs = append(goArgs, args[1:]...)

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
	transform, ok := transformFuncs[tool]
	if !ok {
		return fmt.Errorf("unknown tool: %q", tool)
	}
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

	"addr2line": nil,
	"api":       nil,
	"asm":       nil,
	"buildid":   nil,
	"cgo":       nil,
	"cover":     nil,
	"dist":      nil,
	"doc":       nil,
	"fix":       nil,
	"nm":        nil,
	"objdump":   nil,
	"pack":      nil,
	"pprof":     nil,
	"test2json": nil,
	"trace":     nil,
	"vet":       nil,
}

func transformCompile(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".go")
	if len(paths) == 0 {
		// Nothing to transform; probably just ["-V=full"].
		return args, nil
	}
	pkgPath := flagValue(flags, "-p")
	if !isPrivate(pkgPath) {
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
	// log.Printf("%#v", ids)
	var files []*ast.File
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
	}
	if _, err := origTypesConfig.Check(pkgPath, fset, files, info); err != nil {
		return nil, fmt.Errorf("typecheck error: %v", err)
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

	blacklist := buildBlacklist(files, info)

	// TODO: randomize the order and names of the files
	for i, file := range files {
		origName := filepath.Base(filepath.Clean(paths[i]))
		name := fmt.Sprintf("z%d.go", i)
		switch {
		case strings.HasPrefix(origName, "_cgo_"):
			// Cgo generated code requires a prefix. Also, don't
			// garble it, since it's just generated code and it gets
			// messy.
			name = "_cgo_" + name
		default:
			file = transformGo(file, info, blacklist)
		}
		tempFile := filepath.Join(tempDir, name)
		f, err := os.Create(tempFile)
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
		args = append(args, f.Name())
	}
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
		if len(buildInfo.imports) == 0 {
			buildInfo.firstImport = importPath
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
	return trimBuildID(string(bytes.TrimSpace(out))), nil
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

func buildBlacklist(files []*ast.File, info *types.Info) (blacklist []types.Object) {
	pre := func(node ast.Node) bool {
		nodeExpr, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		exprFunc, ok := nodeExpr.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		exprFuncType := info.ObjectOf(exprFunc.Sel)

		if exprFuncType.Pkg().Path() == "reflect" && (exprFuncType.Name() == "TypeOf" || exprFuncType.Name() == "ValueOf") {
			for _, arg := range nodeExpr.Args {
				if expr, ok := arg.(*ast.CallExpr); ok {
					if f, ok := expr.Fun.(*ast.Ident); ok {
						blacklistItem := info.ObjectOf(f)
						blacklist = append(blacklist, blacklistItem)
					}
				}
			}
		}

		return true
	}

	for _, file := range files {
		ast.Inspect(file, pre)
	}

	return blacklist
}

// transformGo garbles the provided Go syntax node.
func transformGo(file *ast.File, info *types.Info, blacklist []types.Object) *ast.File {
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
			switch cursor.Parent().(type) {
			case *ast.AssignStmt:
				// symbolic var v in v := expr.(type)
				node.Name = hashWith(buildInfo.buildID, node.Name)
			}
			return true
		}
		pkg := obj.Pkg()
		if pkg == nil {
			return true // universe scope
		}
		if vr, ok := obj.(*types.Var); ok && vr.Embedded() {
			// ObjectOf returns the field for embedded struct
			// fields, not the type it uses. Use the type.
			obj = objOf(obj.Type())
			pkg = obj.Pkg()
		}

		if pkg.Name() == "main" && obj.Exported() && obj.Parent() == pkg.Scope() {
			// TODO: only do this when -buildmode is plugin? what
			// about other -buildmode options?
			return true // could be a Go plugin API
		}

		for _, item := range blacklist {
			if item.Pkg().Path() == obj.Pkg().Path() && item.Name() == obj.Name() {
				return true
			}
		}

		// log.Printf("%#v %T", node, obj)
		switch x := obj.(type) {
		case *types.Var:
			if x.IsField() && x.Exported() {
				// might be used for reflection, e.g.
				// encoding/json without struct tags
				return true
			}
		case *types.Const:
		case *types.TypeName:
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
		(obj.Scope() != nil && obj.Scope().Pos() == token.NoPos)
}

// objOf tries to obtain the object behind a *types.Named, even if it's behind a
// pointer type. This is useful to obtain "testing.T" from "*testing.T", or to
// obtain the type declaration object from an embedded field.
func objOf(t types.Type) types.Object {
	switch t := t.(type) {
	case *types.Named:
		return t.Obj()
	case interface{ Elem() types.Type }:
		return objOf(t.Elem())
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
	obj := objOf(params.At(0).Type())
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
		j := strings.IndexByte(name, '.')
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
