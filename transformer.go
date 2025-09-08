package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"log"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ssa"
	"mvdan.cc/garble/internal/ctrlflow"
	"mvdan.cc/garble/internal/literals"
)

// cmd/bundle will include a go:generate directive in its output by default.
// Ours specifies a version and doesn't assume bundle is in $PATH, so drop it.

//go:generate go tool bundle -o cmdgo_quoted.go -prefix cmdgoQuoted cmd/internal/quoted
//go:generate sed -i /go:generate/d cmdgo_quoted.go

// computeLinkerVariableStrings iterates over the -ldflags arguments,
// filling a map with all the string values set via the linker's -X flag.
// TODO: can we put this in sharedCache, using objectString as a key?
func computeLinkerVariableStrings(pkg *types.Package) (map[*types.Var]string, error) {
	linkerVariableStrings := make(map[*types.Var]string)

	// TODO: this is a linker flag that affects how we obfuscate a package at
	// compile time. Note that, if the user changes ldflags, then Go may only
	// re-link the final binary, without re-compiling any packages at all.
	// It's possible that this could result in:
	//
	//    garble -literals build -ldflags=-X=pkg.name=before # name="before"
	//    garble -literals build -ldflags=-X=pkg.name=after  # name="before" as cached
	//
	// We haven't been able to reproduce this problem for now,
	// but it's worth noting it and keeping an eye out for it in the future.
	// If we do confirm this theoretical bug,
	// the solution will be to either find a different solution for -literals,
	// or to force including -ldflags into the build cache key.
	ldflags, err := cmdgoQuotedSplit(flagValue(sharedCache.ForwardBuildFlags, "-ldflags"))
	if err != nil {
		return nil, err
	}
	flagValueIter(ldflags, "-X", func(val string) {
		// val is in the form of "foo.com/bar.name=value".
		fullName, stringValue, found := strings.Cut(val, "=")
		if !found {
			return // invalid
		}

		// fullName is "foo.com/bar.name"
		i := strings.LastIndexByte(fullName, '.')
		path, name := fullName[:i], fullName[i+1:]

		// Note that package main always has import path "main" as part of a build.
		if path != pkg.Path() && (path != "main" || pkg.Name() != "main") {
			return // not the current package
		}

		obj, _ := pkg.Scope().Lookup(name).(*types.Var)
		if obj == nil {
			return // no such variable; skip
		}
		linkerVariableStrings[obj] = stringValue
	})
	return linkerVariableStrings, nil
}

func typecheck(pkgPath string, files []*ast.File, origImporter importerWithMap) (*types.Package, *types.Info, error) {
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Scopes:     make(map[ast.Node]*types.Scope),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Instances:  make(map[*ast.Ident]types.Instance),
	}
	origTypesConfig := types.Config{
		// Note that we don't set GoVersion here. Any Go language version checks
		// are performed by the upfront `go list -json -compiled` call.
		Importer: origImporter,
		Sizes:    types.SizesFor("gc", sharedCache.GoEnv.GOARCH),
	}
	pkg, err := origTypesConfig.Check(pkgPath, fset, files, info)
	if err != nil {
		return nil, nil, fmt.Errorf("typecheck error: %v", err)
	}
	return pkg, info, err
}

func computeFieldToStruct(info *types.Info) map[*types.Var]*types.Struct {
	done := make(map[*types.Named]bool)
	fieldToStruct := make(map[*types.Var]*types.Struct)

	// Run recordType on all types reachable via types.Info.
	// A bit hacky, but I could not find an easier way to do this.
	for _, obj := range info.Uses {
		if obj != nil {
			recordType(obj.Type(), nil, done, fieldToStruct)
		}
	}
	for _, obj := range info.Defs {
		if obj != nil {
			recordType(obj.Type(), nil, done, fieldToStruct)
		}
	}
	for _, tv := range info.Types {
		recordType(tv.Type, nil, done, fieldToStruct)
	}
	return fieldToStruct
}

// recordType visits every reachable type after typechecking a package.
// Right now, all it does is fill the fieldToStruct map.
// Since types can be recursive, we need a map to avoid cycles.
// We only need to track named types as done, as all cycles must use them.
func recordType(used, origin types.Type, done map[*types.Named]bool, fieldToStruct map[*types.Var]*types.Struct) {
	used = types.Unalias(used)
	if origin == nil {
		origin = used
	} else {
		origin = types.Unalias(origin)
		// origin may be a [*types.TypeParam].
		// For now, we haven't found a need to recurse in that case.
		// We can edit this code in the future if we find an example,
		// because we panic if a field is not in fieldToStruct.
		if _, ok := origin.(*types.TypeParam); ok {
			return
		}
	}
	type Container interface{ Elem() types.Type }
	switch used := used.(type) {
	case Container:
		recordType(used.Elem(), origin.(Container).Elem(), done, fieldToStruct)
	case *types.Named:
		if done[used] {
			return
		}
		done[used] = true
		// If we have a generic struct like
		//
		//	type Foo[T any] struct { Bar T }
		//
		// then we want the hashing to use the original "Bar T",
		// because otherwise different instances like "Bar int" and "Bar bool"
		// will result in different hashes and the field names will break.
		// Ensure we record the original generic struct, if there is one.
		recordType(used.Underlying(), used.Origin().Underlying(), done, fieldToStruct)
	case *types.Struct:
		origin := origin.(*types.Struct)
		for i := range used.NumFields() {
			field := used.Field(i)
			fieldToStruct[field] = origin

			if field.Embedded() {
				recordType(field.Type(), origin.Field(i).Type(), done, fieldToStruct)
			}
		}
	}
}

// isSafeForInstanceType returns true if the passed type is safe for var declaration.
// Unsafe types: generic types and non-method interfaces.
func isSafeForInstanceType(t types.Type) bool {
	switch t := types.Unalias(t).(type) {
	case *types.Basic:
		return t.Kind() != types.Invalid
	case *types.Named:
		if t.TypeParams().Len() > 0 {
			return false
		}
		return isSafeForInstanceType(t.Underlying())
	case *types.Signature:
		return t.TypeParams().Len() == 0
	case *types.Interface:
		return t.IsMethodSet()
	}
	return true
}

// namedType tries to obtain the *types.TypeName behind a type, if there is one.
// This is useful to obtain "testing.T" from "*testing.T", or to obtain the type
// declaration object from an embedded field.
// Note that, for a type alias, this gives the alias name.
func namedType(t types.Type) *types.TypeName {
	switch t := t.(type) {
	case *types.Alias:
		return t.Obj()
	case *types.Named:
		return t.Obj()
	case *types.Pointer:
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
	tname := namedType(params.At(0).Type())
	if tname == nil {
		return false // the only parameter isn't named, like "string"
	}
	return tname.Pkg().Path() == "testing" && tname.Name() == "T"
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
	trimpath := flagValue(flags, "-trimpath")

	// Add our temporary dir to the beginning of -trimpath, so that we don't
	// leak temporary dirs. Needs to be at the beginning, since there may be
	// shorter prefixes later in the list, such as $PWD if TMPDIR=$PWD/tmp.
	return flagSetValue(flags, "-trimpath", sharedTempDir+"=>;"+trimpath)
}

// transformer holds all the information and state necessary to obfuscate a
// single Go package.
type transformer struct {
	// curPkg holds basic information about the package being currently compiled or linked.
	curPkg *listedPackage

	// curPkgCache is the pkgCache for curPkg.
	curPkgCache pkgCache

	// The type-checking results; the package itself, and the Info struct.
	pkg  *types.Package
	info *types.Info

	// linkerVariableStrings records objects for variables used in -ldflags=-X flags,
	// as well as the strings the user wants to inject them with.
	// Used when obfuscating literals, so that we obfuscate the injected value.
	linkerVariableStrings map[*types.Var]string

	// fieldToStruct helps locate struct types from any of their field
	// objects. Useful when obfuscating field names.
	fieldToStruct map[*types.Var]*types.Struct

	// obfRand is initialized by transformCompile and used during obfuscation.
	// It is left nil at init time, so that we only use it after it has been
	// properly initialized with a deterministic seed.
	// It must only be used for deterministic obfuscation;
	// if it is used for any other purpose, we may lose determinism.
	obfRand *mathrand.Rand

	// origImporter is a go/types importer which uses the original versions
	// of packages, without any obfuscation. This is helpful to make
	// decisions on how to obfuscate our input code.
	origImporter importerWithMap

	// usedAllImportsFiles is used to prevent multiple calls of tf.useAllImports function on one file
	// in case of simultaneously applied control flow and literals obfuscation
	usedAllImportsFiles map[*ast.File]bool
}

var transformMethods = map[string]func(*transformer, []string) ([]string, error){
	"asm":     (*transformer).transformAsm,
	"compile": (*transformer).transformCompile,
	"link":    (*transformer).transformLink,
}

func (tf *transformer) transformAsm(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".s")

	// When assembling, the import path can make its way into the output object file.
	flags = flagSetValue(flags, "-p", tf.curPkg.obfuscatedImportPath())

	flags = alterTrimpath(flags)

	// The assembler runs twice; the first with -gensymabis,
	// where we continue below and we obfuscate all the source.
	// The second time, without -gensymabis, we reconstruct the paths to the
	// obfuscated source files and reuse them to avoid work.
	newPaths := make([]string, 0, len(paths))
	if !slices.Contains(args, "-gensymabis") {
		for _, path := range paths {
			name := hashWithPackage(tf.curPkg, filepath.Base(path)) + ".s"
			pkgDir := filepath.Join(sharedTempDir, tf.curPkg.obfuscatedSourceDir())
			newPath := filepath.Join(pkgDir, name)
			newPaths = append(newPaths, newPath)
		}
		return append(flags, newPaths...), nil
	}

	const missingHeader = "missing header path"
	newHeaderPaths := make(map[string]string)
	var buf, includeBuf bytes.Buffer
	for _, path := range paths {
		buf.Reset()
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close() // in case of error
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()

			// Whole-line comments might be directives, leave them in place.
			// For example: //go:build race
			// Any other comment, including inline ones, can be discarded entirely.
			line, comment, hasComment := strings.Cut(line, "//")
			if hasComment && line == "" {
				buf.WriteString("//")
				buf.WriteString(comment)
				buf.WriteByte('\n')
				continue
			}

			// Preprocessor lines to include another file.
			// For example: #include "foo.h"
			if quoted, ok := strings.CutPrefix(line, "#include"); ok {
				quoted = strings.TrimSpace(quoted)
				path, err := strconv.Unquote(quoted)
				if err != nil { // note that strconv.Unquote errors do not include the input string
					return nil, fmt.Errorf("cannot unquote %q: %v", quoted, err)
				}
				newPath := newHeaderPaths[path]
				switch newPath {
				case missingHeader: // no need to try again
					buf.WriteString(line)
					buf.WriteByte('\n')
					continue
				case "": // first time we see this header
					includeBuf.Reset()
					content, err := os.ReadFile(path)
					if errors.Is(err, fs.ErrNotExist) {
						newHeaderPaths[path] = missingHeader
						buf.WriteString(line)
						buf.WriteByte('\n')
						continue // a header file provided by Go or the system
					} else if err != nil {
						return nil, err
					}
					tf.replaceAsmNames(&includeBuf, content)

					// For now, we replace `foo.h` or `dir/foo.h` with `garbled_foo.h`.
					// The different name ensures we don't use the unobfuscated file.
					// This is far from perfect, but does the job for the time being.
					// In the future, use a randomized name.
					basename := filepath.Base(path)
					newPath = "garbled_" + basename

					if _, err := tf.writeSourceFile(basename, newPath, includeBuf.Bytes()); err != nil {
						return nil, err
					}
					newHeaderPaths[path] = newPath
				}
				buf.WriteString("#include ")
				buf.WriteString(strconv.Quote(newPath))
				buf.WriteByte('\n')
				continue
			}

			// Anything else is regular assembly; replace the names.
			tf.replaceAsmNames(&buf, []byte(line))
			buf.WriteByte('\n')
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}

		// With assembly files, we obfuscate the filename in the temporary
		// directory, as assembly files do not support `/*line` directives.
		// TODO(mvdan): per cmd/asm/internal/lex, they do support `#line`.
		basename := filepath.Base(path)
		newName := hashWithPackage(tf.curPkg, basename) + ".s"
		if path, err := tf.writeSourceFile(basename, newName, buf.Bytes()); err != nil {
			return nil, err
		} else {
			newPaths = append(newPaths, path)
		}
		f.Close() // do not keep len(paths) files open
	}

	return append(flags, newPaths...), nil
}

func (tf *transformer) replaceAsmNames(buf *bytes.Buffer, remaining []byte) {
	// We need to replace all function references with their obfuscated name
	// counterparts.
	// Luckily, all func names in Go assembly files are immediately followed
	// by the unicode "middle dot", like:
	//
	//	TEXT ·privateAdd(SB),$0-24
	//	TEXT runtime∕internal∕sys·Ctz64(SB), NOSPLIT, $0-12
	//
	// Note that import paths in assembly, like `runtime∕internal∕sys` above,
	// use Unicode periods and slashes rather than the ASCII ones used by `go list`.
	// We need to convert to ASCII to find the right package information.
	const (
		asmPeriod = '·'
		goPeriod  = '.'
		asmSlash  = '∕'
		goSlash   = '/'
	)
	asmPeriodLen := utf8.RuneLen(asmPeriod)

	for {
		periodIdx := bytes.IndexRune(remaining, asmPeriod)
		if periodIdx < 0 {
			buf.Write(remaining)
			remaining = nil
			break
		}

		// The package name ends at the first rune which cannot be part of a Go
		// import path, such as a comma or space.
		pkgStart := periodIdx
		for pkgStart >= 0 {
			c, size := utf8.DecodeLastRune(remaining[:pkgStart])
			if !unicode.IsLetter(c) && c != '_' && c != asmSlash && !unicode.IsDigit(c) {
				break
			}
			pkgStart -= size
		}
		// The package name might actually be longer, e.g:
		//
		//	JMP test∕with·many·dots∕main∕imported·PublicAdd(SB)
		//
		// We have `test∕with` so far; grab `·many·dots∕main∕imported` as well.
		pkgEnd := periodIdx
		lastAsmPeriod := -1
		for i := pkgEnd + asmPeriodLen; i <= len(remaining); {
			c, size := utf8.DecodeRune(remaining[i:])
			if c == asmPeriod {
				lastAsmPeriod = i
			} else if !unicode.IsLetter(c) && c != '_' && c != asmSlash && !unicode.IsDigit(c) {
				if lastAsmPeriod > 0 {
					pkgEnd = lastAsmPeriod
				}
				break
			}
			i += size
		}
		asmPkgPath := string(remaining[pkgStart:pkgEnd])

		// Write the bytes before our unqualified `·foo` or qualified `pkg·foo`.
		buf.Write(remaining[:pkgStart])

		// If the name was qualified, fetch the package, and write the
		// obfuscated import path if needed.
		lpkg := tf.curPkg
		if asmPkgPath != "" {
			if asmPkgPath != tf.curPkg.Name {
				goPkgPath := asmPkgPath
				goPkgPath = strings.ReplaceAll(goPkgPath, string(asmPeriod), string(goPeriod))
				goPkgPath = strings.ReplaceAll(goPkgPath, string(asmSlash), string(goSlash))
				var err error
				lpkg, err = listPackage(tf.curPkg, goPkgPath)
				if err != nil {
					panic(err) // shouldn't happen
				}
			}
			if lpkg.ToObfuscate {
				// Note that we don't need to worry about asmSlash here,
				// because our obfuscated import paths contain no slashes right now.
				buf.WriteString(lpkg.obfuscatedImportPath())
			} else {
				buf.WriteString(asmPkgPath)
			}
		}

		// Write the middle dot and advance the remaining slice.
		buf.WriteRune(asmPeriod)
		remaining = remaining[pkgEnd+asmPeriodLen:]

		// The declared name ends at the first rune which cannot be part of a Go
		// identifier, such as a comma or space.
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

		if lpkg.ToObfuscate && !compilerIntrinsics[lpkg.ImportPath][name] {
			newName := hashWithPackage(lpkg, name)
			if flagDebug { // TODO(mvdan): remove once https://go.dev/issue/53465 if fixed
				log.Printf("asm name %q hashed with %x to %q", name, tf.curPkg.GarbleActionID, newName)
			}
			buf.WriteString(newName)
		} else {
			buf.WriteString(name)
		}
	}
}

// writeSourceFile is a mix between os.CreateTemp and os.WriteFile, as it writes a
// named source file in sharedTempDir given an input buffer.
//
// Note that the file is created under a directory tree following curPkg's
// import path, mimicking how files are laid out in modules and GOROOT.
func (tf *transformer) writeSourceFile(basename, obfuscated string, content []byte) (string, error) {
	// Uncomment for some quick debugging. Do not delete.
	// fmt.Fprintf(os.Stderr, "\n-- %s/%s --\n%s", curPkg.ImportPath, basename, content)

	if flagDebugDir != "" {
		pkgDir := filepath.Join(flagDebugDir, filepath.FromSlash(tf.curPkg.ImportPath))
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			return "", err
		}
		dstPath := filepath.Join(pkgDir, basename)
		if err := os.WriteFile(dstPath, content, 0o666); err != nil {
			return "", err
		}
	}
	// We use the obfuscated import path to hold the temporary files.
	// Assembly files do not support line directives to set positions,
	// so the only way to not leak the import path is to replace it.
	pkgDir := filepath.Join(sharedTempDir, tf.curPkg.obfuscatedSourceDir())
	if err := os.MkdirAll(pkgDir, 0o777); err != nil {
		return "", err
	}
	dstPath := filepath.Join(pkgDir, obfuscated)
	if err := writeFileExclusive(dstPath, content); err != nil {
		return "", err
	}
	return dstPath, nil
}

func (tf *transformer) transformCompile(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".go")

	// We will force the linker to drop DWARF via -w, so don't spend time
	// generating it.
	flags = append(flags, "-dwarf=false")

	// The Go file paths given to the compiler are always absolute paths.
	files, err := parseFiles(tf.curPkg, "", paths)
	if err != nil {
		return nil, err
	}

	// Literal and control flow obfuscation uses math/rand, so seed it deterministically.
	randSeed := tf.curPkg.GarbleActionID[:]
	if flagSeed.present() {
		randSeed = flagSeed.bytes
	}
	// log.Printf("seeding math/rand with %x\n", randSeed)
	tf.obfRand = mathrand.New(mathrand.NewSource(int64(binary.BigEndian.Uint64(randSeed))))

	// Even if loadPkgCache below finds a direct cache hit,
	// other parts of garble still need type information to obfuscate.
	// We could potentially avoid this by saving the type info we need in the cache,
	// although in general that wouldn't help much, since it's rare for Go's cache
	// to miss on a package and for our cache to hit.
	if tf.pkg, tf.info, err = typecheck(tf.curPkg.ImportPath, files, tf.origImporter); err != nil {
		return nil, err
	}

	var (
		ssaPkg       *ssa.Package
		requiredPkgs []string
	)
	if flagControlFlow {
		ssaPkg = ssaBuildPkg(tf.pkg, files, tf.info)

		newFileName, newFile, affectedFiles, err := ctrlflow.Obfuscate(fset, ssaPkg, files, tf.obfRand)
		if err != nil {
			return nil, err
		}

		if newFile != nil {
			files = append(files, newFile)
			paths = append(paths, newFileName)
			for _, file := range affectedFiles {
				tf.useAllImports(file)
			}
			if tf.pkg, tf.info, err = typecheck(tf.curPkg.ImportPath, files, tf.origImporter); err != nil {
				return nil, err
			}

			for _, imp := range newFile.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					panic(err) // should never happen
				}
				requiredPkgs = append(requiredPkgs, path)
			}
		}
	}

	if tf.curPkgCache, err = loadPkgCache(tf.curPkg, tf.pkg, files, tf.info, ssaPkg); err != nil {
		return nil, err
	}

	// These maps are not kept in pkgCache, since they are only needed to obfuscate curPkg.
	tf.fieldToStruct = computeFieldToStruct(tf.info)
	if flagLiterals {
		if tf.linkerVariableStrings, err = computeLinkerVariableStrings(tf.pkg); err != nil {
			return nil, err
		}
	}

	flags = alterTrimpath(flags)
	newImportCfg, err := tf.processImportCfg(flags, requiredPkgs)
	if err != nil {
		return nil, err
	}

	// Note that the main package always uses `-p main`, even though it's not an import path.
	flags = flagSetValue(flags, "-p", tf.curPkg.obfuscatedImportPath())

	newPaths := make([]string, 0, len(files))

	for i, file := range files {
		basename := filepath.Base(paths[i])
		log.Printf("obfuscating %s", basename)
		if tf.curPkg.ImportPath == "runtime" {
			if flagTiny {
				// strip unneeded runtime code
				stripRuntime(basename, file)
				tf.useAllImports(file)
			}
			if basename == "symtab.go" {
				updateMagicValue(file, magicValue())
				updateEntryOffset(file, entryOffKey())
			}
		}
		if err := tf.transformDirectives(file.Comments); err != nil {
			return nil, err
		}
		file = tf.transformGoFile(file)
		file.Name.Name = tf.curPkg.obfuscatedPackageName()

		src, err := printFile(tf.curPkg, file)
		if err != nil {
			return nil, err
		}

		if tf.curPkg.Name == "main" && strings.HasSuffix(reflectPatchFile, basename) {
			src = reflectMainPostPatch(src, tf.curPkg, tf.curPkgCache)
		}

		// We hide Go source filenames via "//line" directives,
		// so there is no need to use obfuscated filenames here.
		if path, err := tf.writeSourceFile(basename, basename, src); err != nil {
			return nil, err
		} else {
			newPaths = append(newPaths, path)
		}
	}
	flags = flagSetValue(flags, "-importcfg", newImportCfg)

	return append(flags, newPaths...), nil
}

// transformDirectives rewrites //go:linkname toolchain directives in comments
// to replace names with their obfuscated versions.
func (tf *transformer) transformDirectives(comments []*ast.CommentGroup) error {
	for _, group := range comments {
		for _, comment := range group.List {
			if !strings.HasPrefix(comment.Text, "//go:linkname ") {
				continue
			}

			// We can have either just one argument:
			//
			//	//go:linkname localName
			//
			// Or two arguments, where the second may refer to a name in a
			// different package:
			//
			//	//go:linkname localName newName
			//	//go:linkname localName pkg.newName
			fields := strings.Fields(comment.Text)
			localName := fields[1]
			newName := ""
			if len(fields) == 3 {
				newName = fields[2]
			}
			switch newName {
			case "runtime.lastmoduledatap", "runtime.moduledataverify1":
				// Linknaming to the var and function above is used by github.com/bytedance/sonic/loader
				// to inject functions into the runtime, but that breaks as garble patches
				// the runtime to change the function header magic number.
				//
				// Given that Go is locking down access to runtime internals via go:linkname,
				// and what sonic does was never supported and is a hack,
				// refuse to build before the user sees confusing run-time panics.
				return fmt.Errorf("garble does not support packages with a //go:linkname to %s", newName)
			}

			localName, newName = tf.transformLinkname(localName, newName)
			fields[1] = localName
			if len(fields) == 3 {
				fields[2] = newName
			}

			if flagDebug { // TODO(mvdan): remove once https://go.dev/issue/53465 if fixed
				log.Printf("linkname %q changed to %q", comment.Text, strings.Join(fields, " "))
			}
			comment.Text = strings.Join(fields, " ")
		}
	}
	return nil
}

func (tf *transformer) transformLinkname(localName, newName string) (string, string) {
	// obfuscate the local name, if the current package is obfuscated
	if tf.curPkg.ToObfuscate && !compilerIntrinsics[tf.curPkg.ImportPath][localName] {
		localName = hashWithPackage(tf.curPkg, localName)
	}
	if newName == "" {
		return localName, ""
	}
	// If the new name is of the form "pkgpath.Name", and we've obfuscated
	// "Name" in that package, rewrite the directive to use the obfuscated name.
	dotCnt := strings.Count(newName, ".")
	if dotCnt < 1 {
		// cgo-generated code uses linknames to made up symbol names,
		// which do not have a package path at all.
		// Replace the comment in case the local name was obfuscated.
		return localName, newName
	}
	switch newName {
	case "main.main", "main..inittask", "runtime..inittask":
		// The runtime uses some special symbols with "..".
		// We aren't touching those at the moment.
		return localName, newName
	}

	pkgSplit := 0
	var foreignName string
	var lpkg *listedPackage
	for {
		i := strings.Index(newName[pkgSplit:], ".")
		if i < 0 {
			// We couldn't find a prefix that matched a known package.
			// Probably a made up name like above, but with a dot.
			return localName, newName
		}
		pkgSplit += i
		pkgPath := newName[:pkgSplit]
		pkgSplit++ // skip over the dot

		if strings.HasSuffix(pkgPath, "_test") {
			// runtime uses a go:linkname to metrics_test;
			// we don't need this to work for now on regular builds,
			// though we might need to rethink this if we want "go test std" to work.
			continue
		}

		var err error
		lpkg, err = listPackage(tf.curPkg, pkgPath)
		if err == nil {
			foreignName = newName[pkgSplit:]
			break
		}
		if errors.Is(err, ErrNotFound) {
			// No match; find the next dot.
			continue
		}
		if errors.Is(err, ErrNotDependency) {
			fmt.Fprintf(os.Stderr,
				"//go:linkname refers to %s - add `import _ %q` for garble to find the package",
				newName, pkgPath)
			return localName, newName
		}
		panic(err) // shouldn't happen
	}

	if !lpkg.ToObfuscate || compilerIntrinsics[lpkg.ImportPath][foreignName] {
		// We're not obfuscating that package or name.
		return localName, newName
	}

	var newForeignName string
	if receiver, name, ok := strings.Cut(foreignName, "."); ok {
		if receiver, ok = strings.CutPrefix(receiver, "(*"); ok {
			// pkg/path.(*Receiver).method
			receiver, _ = strings.CutSuffix(receiver, ")")
			receiver = "(*" + hashWithPackage(lpkg, receiver) + ")"
		} else {
			// pkg/path.Receiver.method
			receiver = hashWithPackage(lpkg, receiver)
		}
		// Exported methods are never obfuscated.
		//
		// TODO(mvdan): We're duplicating the logic behind these decisions.
		// Reuse the logic with transformCompile.
		if !token.IsExported(name) {
			name = hashWithPackage(lpkg, name)
		}
		newForeignName = receiver + "." + name
	} else {
		// pkg/path.function
		newForeignName = hashWithPackage(lpkg, foreignName)
	}

	newName = lpkg.obfuscatedImportPath() + "." + newForeignName
	return localName, newName
}

// processImportCfg parses the importcfg file passed to a compile or link step.
// It also builds a new importcfg file to account for obfuscated import paths.
func (tf *transformer) processImportCfg(flags []string, requiredPkgs []string) (newImportCfg string, _ error) {
	importCfg := flagValue(flags, "-importcfg")
	if importCfg == "" {
		return "", fmt.Errorf("could not find -importcfg argument")
	}
	data, err := os.ReadFile(importCfg)
	if err != nil {
		return "", err
	}

	var packagefiles, importmaps [][2]string

	// using for track required but not imported packages
	var newIndirectImports map[string]bool
	if requiredPkgs != nil {
		newIndirectImports = make(map[string]bool)
		for _, pkg := range requiredPkgs {
			// unsafe is a special case, it's not a real dependency
			if pkg == "unsafe" {
				continue
			}

			newIndirectImports[pkg] = true
		}
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		verb, args, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		switch verb {
		case "importmap":
			beforePath, afterPath, found := strings.Cut(args, "=")
			if !found {
				continue
			}
			importmaps = append(importmaps, [2]string{beforePath, afterPath})
		case "packagefile":
			importPath, objectPath, found := strings.Cut(args, "=")
			if !found {
				continue
			}
			packagefiles = append(packagefiles, [2]string{importPath, objectPath})
			delete(newIndirectImports, importPath)
		}
	}

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
		lpkg, err := listPackage(tf.curPkg, beforePath)
		if err != nil {
			return "", err
		}
		if lpkg.ToObfuscate {
			// Note that beforePath is not the canonical path.
			// For beforePath="vendor/foo", afterPath and
			// lpkg.ImportPath can be just "foo".
			// Don't use obfuscatedImportPath here.
			beforePath = hashWithPackage(lpkg, beforePath)

			afterPath = lpkg.obfuscatedImportPath()
		}
		fmt.Fprintf(newCfg, "importmap %s=%s\n", beforePath, afterPath)
	}

	if len(newIndirectImports) > 0 {
		f, err := os.Open(filepath.Join(sharedTempDir, actionGraphFileName))
		if err != nil {
			return "", fmt.Errorf("cannot open action graph file: %v", err)
		}
		defer f.Close()

		var actions []struct {
			Mode    string
			Package string
			Objdir  string
		}
		if err := json.NewDecoder(f).Decode(&actions); err != nil {
			return "", fmt.Errorf("cannot parse action graph file: %v", err)
		}

		// theoretically action graph can be long, to optimise it process it in one pass
		// with an early exit when all the required imports are found
		for _, action := range actions {
			if action.Mode != "build" {
				continue
			}
			if ok := newIndirectImports[action.Package]; !ok {
				continue
			}

			packagefiles = append(packagefiles, [2]string{action.Package, filepath.Join(action.Objdir, "_pkg_.a")}) // file name hardcoded in compiler
			delete(newIndirectImports, action.Package)
			if len(newIndirectImports) == 0 {
				break
			}
		}

		if len(newIndirectImports) > 0 {
			return "", fmt.Errorf("cannot resolve required packages from action graph file: %v", requiredPkgs)
		}
	}

	for _, pair := range packagefiles {
		impPath, pkgfile := pair[0], pair[1]
		lpkg, err := listPackage(tf.curPkg, impPath)
		if err != nil {
			// TODO: it's unclear why an importcfg can include an import path
			// that's not a dependency in an edge case with "go test ./...".
			// See exporttest/*.go in testdata/scripts/test.txt.
			// For now, spot the pattern and avoid the unnecessary error;
			// the dependency is unused, so the packagefile line is redundant.
			// This still triggers as of go1.24.
			if strings.HasSuffix(tf.curPkg.ImportPath, ".test]") && strings.HasPrefix(tf.curPkg.ImportPath, impPath) {
				continue
			}
			return "", err
		}
		impPath = lpkg.obfuscatedImportPath()
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

func (tf *transformer) useAllImports(file *ast.File) {
	if tf.usedAllImportsFiles == nil {
		tf.usedAllImportsFiles = make(map[*ast.File]bool)
	} else if ok := tf.usedAllImportsFiles[file]; ok {
		return
	}
	tf.usedAllImportsFiles[file] = true

	for _, imp := range file.Imports {
		if imp.Name != nil && imp.Name.Name == "_" {
			continue
		}

		pkgName := tf.info.PkgNameOf(imp)
		pkgScope := pkgName.Imported().Scope()
		var nameObj types.Object
		for _, name := range pkgScope.Names() {
			if obj := pkgScope.Lookup(name); obj.Exported() && isSafeForInstanceType(obj.Type()) {
				nameObj = obj
				break
			}
		}
		if nameObj == nil {
			// A very unlikely situation where there is no suitable declaration for a reference variable
			// and almost certainly means that there is another import reference in code.
			continue
		}
		spec := &ast.ValueSpec{Names: []*ast.Ident{ast.NewIdent("_")}}
		decl := &ast.GenDecl{Specs: []ast.Spec{spec}}

		nameIdent := ast.NewIdent(nameObj.Name())
		var nameExpr ast.Expr
		switch {
		case imp.Name == nil: // import "pkg/path"
			nameExpr = &ast.SelectorExpr{
				X:   ast.NewIdent(pkgName.Name()),
				Sel: nameIdent,
			}
		case imp.Name.Name != ".": // import path2 "pkg/path"
			nameExpr = &ast.SelectorExpr{
				X:   ast.NewIdent(imp.Name.Name),
				Sel: nameIdent,
			}
		default: // import . "pkg/path"
			nameExpr = nameIdent
		}

		switch nameObj.(type) {
		case *types.Const:
			// const _ = <value>
			decl.Tok = token.CONST
			spec.Values = []ast.Expr{nameExpr}
		case *types.Var, *types.Func:
			// var _ = <value>
			decl.Tok = token.VAR
			spec.Values = []ast.Expr{nameExpr}
		case *types.TypeName:
			// var _ <type>
			decl.Tok = token.VAR
			spec.Type = nameExpr
		default:
			continue // skip *types.Builtin and others
		}

		// Ensure that types.Info.Uses is up to date.
		tf.info.Uses[nameIdent] = nameObj
		file.Decls = append(file.Decls, decl)
	}
}

// transformGoFile obfuscates the provided Go syntax file.
func (tf *transformer) transformGoFile(file *ast.File) *ast.File {
	// Only obfuscate the literals here if the flag is on
	// and if the package in question is to be obfuscated.
	//
	// We can't obfuscate literals in the runtime and its dependencies,
	// because obfuscated literals sometimes escape to heap,
	// and that's not allowed in the runtime itself.
	if flagLiterals && tf.curPkg.ToObfuscate {
		file = literals.Obfuscate(tf.obfRand, file, tf.info, tf.linkerVariableStrings, randomName)

		// some imported constants might not be needed anymore, remove unnecessary imports
		tf.useAllImports(file)
	}

	pre := func(cursor *astutil.Cursor) bool {
		node, ok := cursor.Node().(*ast.Ident)
		if !ok {
			return true
		}
		name := node.Name
		if name == "_" {
			return true // unnamed remains unnamed
		}
		obj := tf.info.ObjectOf(node)
		if obj == nil {
			_, isImplicit := tf.info.Defs[node]
			_, parentIsFile := cursor.Parent().(*ast.File)
			if !isImplicit || parentIsFile {
				// We only care about nil objects in the switch scenario below.
				return true
			}
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
			obj = types.NewVar(node.Pos(), tf.pkg, name, nil)
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
			// Alternatively, if we don't have an alias, we still want to
			// use the embedded type, not the field.
			tname := namedType(obj.Type())
			if tname == nil {
				return true // unnamed type (probably a basic type, e.g. int)
			}
			obj = tname
			pkg = obj.Pkg()
		}
		if pkg == nil {
			return true // universe scope
		}

		// TODO: We match by object name here, which is actually imprecise.
		// For example, in package embed we match the type FS, but we would also
		// match any field or method named FS.
		// Can we instead use an object map like ReflectObjects?
		path := pkg.Path()
		switch path {
		case "sync/atomic", "runtime/internal/atomic":
			if name == "align64" {
				return true
			}
		case "embed":
			// FS is detected by the compiler for //go:embed.
			if name == "FS" {
				return true
			}
		case "reflect":
			switch name {
			// Per the linker's deadcode.go docs,
			// the Method and MethodByName methods are what drive the logic.
			case "Method", "MethodByName":
				return true
			}
		case "crypto/x509/pkix":
			// For better or worse, encoding/asn1 detects a "SET" suffix on slice type names
			// to tell whether those slices should be treated as sets or sequences.
			// Do not obfuscate those names to prevent breaking x509 certificates.
			// TODO: we can surely do better; ideally propose a non-string-based solution
			// upstream, or as a fallback, obfuscate to a name ending with "SET".
			if strings.HasSuffix(name, "SET") {
				return true
			}
		}

		lpkg, err := listPackage(tf.curPkg, path)
		if err != nil {
			panic(err) // shouldn't happen
		}
		if !lpkg.ToObfuscate {
			return true // we're not obfuscating this package
		}
		hashToUse := lpkg.GarbleActionID
		debugName := "variable"

		// log.Printf("%s: %#v %T", fset.Position(node.Pos()), node, obj)
		switch obj := obj.(type) {
		case *types.Var:
			if !obj.IsField() {
				// Identifiers denoting variables are always obfuscated.
				break
			}
			debugName = "field"
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
				panic("could not find struct for field " + name)
			}
			node.Name = hashWithStruct(strct, obj)
			if flagDebug { // TODO(mvdan): remove once https://go.dev/issue/53465 if fixed
				log.Printf("%s %q hashed with struct fields to %q", debugName, name, node.Name)
			}
			return true

		case *types.TypeName:
			debugName = "type"
		case *types.Func:
			if compilerIntrinsics[path][name] {
				return true
			}

			sign := obj.Signature()
			if sign.Recv() == nil {
				debugName = "func"
			} else {
				debugName = "method"
			}
			if obj.Exported() && sign.Recv() != nil {
				return true // might implement an interface
			}
			switch name {
			case "main", "init", "TestMain":
				return true // don't break them
			}
			if strings.HasPrefix(name, "Test") && isTestSignature(sign) {
				return true // don't break tests
			}
		default:
			return true // we only want to rename the above
		}

		node.Name = hashWithPackage(lpkg, name)
		// TODO: probably move the debugf lines inside the hash funcs
		if flagDebug { // TODO(mvdan): remove once https://go.dev/issue/53465 if fixed
			log.Printf("%s %q hashed with %x… to %q", debugName, name, hashToUse[:4], node.Name)
		}
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
		lpkg, err := listPackage(tf.curPkg, path)
		if err != nil {
			panic(err) // should never happen
		}
		// Note that a main package is imported via its original import path.
		imp.Path.Value = strconv.Quote(lpkg.obfuscatedImportPath())
		// If the import was unnamed, give it the name of the
		// original package name, to keep references working.
		if imp.Name == nil {
			imp.Name = &ast.Ident{
				NamePos: imp.Path.ValuePos, // ensure it ends up on the same line
				Name:    lpkg.Name,
			}
		}
		return true
	}

	return astutil.Apply(file, pre, post).(*ast.File)
}

func (tf *transformer) transformLink(args []string) ([]string, error) {
	// We can't split by the ".a" extension, because cached object files
	// lack any extension.
	flags, args := splitFlagsFromArgs(args)

	newImportCfg, err := tf.processImportCfg(flags, nil)
	if err != nil {
		return nil, err
	}

	// TODO: unify this logic with the -X handling when using -literals.
	// We should be able to handle both cases via the syntax tree.
	//
	// Make sure -X works with obfuscated identifiers.
	// To cover both obfuscated and non-obfuscated names,
	// duplicate each flag with a obfuscated version.
	flagValueIter(flags, "-X", func(val string) {
		// val is in the form of "foo.com/bar.name=value".
		fullName, stringValue, found := strings.Cut(val, "=")
		if !found {
			return // invalid
		}

		// fullName is "foo.com/bar.name"
		i := strings.LastIndexByte(fullName, '.')
		path, name := fullName[:i], fullName[i+1:]

		// If the package path is "main", it's the current top-level
		// package we are linking. Otherwise, find it in the cache.
		lpkg := tf.curPkg
		if path != "main" {
			lpkg = sharedCache.ListedPackages[path]
		}
		if lpkg == nil {
			// We couldn't find the package.
			// Perhaps a typo, perhaps not part of the build.
			// cmd/link ignores those, so we should too.
			return
		}
		newName := hashWithPackage(lpkg, name)
		flags = append(flags, fmt.Sprintf("-X=%s.%s=%s", lpkg.obfuscatedImportPath(), newName, stringValue))
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
