// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bufio"
	"bytes"
	cryptorand "crypto/rand"
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
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rogpeppe/go-internal/cache"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ssa"
	"mvdan.cc/garble/internal/ctrlflow"

	"mvdan.cc/garble/internal/linker"
	"mvdan.cc/garble/internal/literals"
)

var flagSet = flag.NewFlagSet("garble", flag.ContinueOnError)

var (
	flagLiterals bool
	flagTiny     bool
	flagDebug    bool
	flagDebugDir string
	flagSeed     seedFlag
	// TODO(pagran): in the future, when control flow obfuscation will be stable migrate to flag
	flagControlFlow = os.Getenv("GARBLE_EXPERIMENTAL_CONTROLFLOW") == "1"
)

func init() {
	flagSet.Usage = usage
	flagSet.BoolVar(&flagLiterals, "literals", false, "Obfuscate literals such as strings")
	flagSet.BoolVar(&flagTiny, "tiny", false, "Optimize for binary size, losing some ability to reverse the process")
	flagSet.BoolVar(&flagDebug, "debug", false, "Print debug logs to stderr")
	flagSet.StringVar(&flagDebugDir, "debugdir", "", "Write the obfuscated source to a directory, e.g. -debugdir=out")
	flagSet.Var(&flagSeed, "seed", "Provide a base64-encoded seed, e.g. -seed=o9WDTZ4CN4w\nFor a random seed, provide -seed=random")
}

var rxGarbleFlag = regexp.MustCompile(`-(?:literals|tiny|debug|debugdir|seed)(?:$|=)`)

type seedFlag struct {
	random bool
	bytes  []byte
}

func (f seedFlag) present() bool { return len(f.bytes) > 0 }

func (f seedFlag) String() string {
	return base64.RawStdEncoding.EncodeToString(f.bytes)
}

func (f *seedFlag) Set(s string) error {
	if s == "random" {
		f.random = true // to show the random seed we chose

		f.bytes = make([]byte, 16) // random 128 bit seed
		if _, err := cryptorand.Read(f.bytes); err != nil {
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

		// TODO: Note that we always use 8 bytes; any bytes after that are
		// entirely ignored. That may be confusing to the end user.
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
	run            replace "go run"
	reverse        de-obfuscate output such as stack traces
	version        print the version and build settings of the garble binary

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
	// Presumably OK to share fset across packages.
	fset = token.NewFileSet()

	sharedTempDir = os.Getenv("GARBLE_SHARED")
	parentWorkDir = os.Getenv("GARBLE_PARENT_WORK")
)

const actionGraphFileName = "action-graph.json"

type importerWithMap struct {
	importMap  map[string]string
	importFrom func(path, dir string, mode types.ImportMode) (*types.Package, error)
}

func (im importerWithMap) Import(path string) (*types.Package, error) {
	panic("should never be called")
}

func (im importerWithMap) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path2 := im.importMap[path]; path2 != "" {
		path = path2
	}
	return im.importFrom(path, dir, mode)
}

func importerForPkg(lpkg *listedPackage) importerWithMap {
	return importerWithMap{
		importFrom: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			pkg, err := listPackage(lpkg, path)
			if err != nil {
				return nil, err
			}
			return os.Open(pkg.Export)
		}).(types.ImporterFrom).ImportFrom,
		importMap: lpkg.ImportMap,
	}
}

// uniqueLineWriter sits underneath log.SetOutput to deduplicate log lines.
// We log bits of useful information for debugging,
// and logging the same detail twice is not going to help the user.
// Duplicates are relatively normal, given that names tend to repeat.
type uniqueLineWriter struct {
	out  io.Writer
	seen map[string]bool
}

func (w *uniqueLineWriter) Write(p []byte) (n int, err error) {
	if !flagDebug {
		panic("unexpected use of uniqueLineWriter with -debug unset")
	}
	if bytes.Count(p, []byte("\n")) != 1 {
		panic(fmt.Sprintf("log write wasn't just one line: %q", p))
	}
	if w.seen[string(p)] {
		return len(p), nil
	}
	if w.seen == nil {
		w.seen = make(map[string]bool)
	}
	w.seen[string(p)] = true
	return w.out.Write(p)
}

// debugSince is like time.Since but resulting in shorter output.
// A build process takes at least hundreds of milliseconds,
// so extra decimal points in the order of microseconds aren't meaningful.
func debugSince(start time.Time) time.Duration {
	return time.Since(start).Truncate(10 * time.Microsecond)
}

func main1() int {
	defer func() {
		if os.Getenv("GARBLE_WRITE_ALLOCS") != "true" {
			return
		}
		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)
		fmt.Fprintf(os.Stderr, "garble allocs: %d\n", memStats.Mallocs)
	}()
	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return 2
	}
	log.SetPrefix("[garble] ")
	log.SetFlags(0) // no timestamps, as they aren't very useful
	if flagDebug {
		// TODO: cover this in the tests.
		log.SetOutput(&uniqueLineWriter{out: os.Stderr})
	} else {
		log.SetOutput(io.Discard)
	}
	args := flagSet.Args()
	if len(args) < 1 {
		usage()
		return 2
	}

	// If a random seed was used, the user won't be able to reproduce the
	// same output or failure unless we print the random seed we chose.
	// If the build failed and a random seed was used,
	// the failure might not reproduce with a different seed.
	// Print it before we exit.
	if flagSeed.random {
		fmt.Fprintf(os.Stderr, "-seed chosen at random: %s\n", base64.RawStdEncoding.EncodeToString(flagSeed.bytes))
	}
	if err := mainErr(args); err != nil {
		if code, ok := err.(errJustExit); ok {
			return int(code)
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type errJustExit int

func (e errJustExit) Error() string { return fmt.Sprintf("exit: %d", e) }

func goVersionOK() bool {
	// TODO(mvdan): use go/version once we can require Go 1.22 or later: https://go.dev/issue/62039
	const (
		minGoVersionSemver = "v1.21.0"
		suggestedGoVersion = "1.21"
	)

	// rxVersion looks for a version like "go1.2" or "go1.2.3"
	rxVersion := regexp.MustCompile(`go\d+\.\d+(?:\.\d+)?`)

	toolchainVersionFull := sharedCache.GoEnv.GOVERSION
	toolchainVersion := rxVersion.FindString(toolchainVersionFull)
	if toolchainVersion == "" {
		// Go 1.15.x and older do not have GOVERSION yet.
		// We could go the extra mile and fetch it via 'go toolchainVersion',
		// but we'd have to error anyway.
		fmt.Fprintf(os.Stderr, "Go version is too old; please upgrade to Go %s or newer\n", suggestedGoVersion)
		return false
	}

	sharedCache.GoVersionSemver = "v" + strings.TrimPrefix(toolchainVersion, "go")
	if semver.Compare(sharedCache.GoVersionSemver, minGoVersionSemver) < 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too old; please upgrade to Go %s or newer\n", toolchainVersionFull, suggestedGoVersion)
		return false
	}

	// Ensure that the version of Go that built the garble binary is equal or
	// newer than cache.GoVersionSemver.
	builtVersionFull := os.Getenv("GARBLE_TEST_GOVERSION")
	if builtVersionFull == "" {
		builtVersionFull = runtime.Version()
	}
	builtVersion := rxVersion.FindString(builtVersionFull)
	if builtVersion == "" {
		// If garble built itself, we don't know what Go version was used.
		// Fall back to not performing the check against the toolchain version.
		return true
	}
	builtVersionSemver := "v" + strings.TrimPrefix(builtVersion, "go")
	if semver.Compare(builtVersionSemver, sharedCache.GoVersionSemver) < 0 {
		fmt.Fprintf(os.Stderr, `
garble was built with %q and is being used with %q; rebuild it with a command like:
    go install mvdan.cc/garble@latest
`[1:], builtVersionFull, toolchainVersionFull)
		return false
	}

	return true
}

func mainErr(args []string) error {
	command, args := args[0], args[1:]

	// Catch users reaching for `go build -toolexec=garble`.
	if command != "toolexec" && len(args) == 1 && args[0] == "-V=full" {
		return fmt.Errorf(`did you run "go [command] -toolexec=garble" instead of "garble [command]"?`)
	}

	switch command {
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
		info, ok := debug.ReadBuildInfo()
		if !ok {
			// The build binary was stripped of build info?
			// Could be the case if garble built itself.
			fmt.Println("unknown")
			return nil
		}
		mod := &info.Main
		if mod.Replace != nil {
			mod = mod.Replace
		}

		// For the tests.
		if v := os.Getenv("GARBLE_TEST_BUILDSETTINGS"); v != "" {
			var extra []debug.BuildSetting
			if err := json.Unmarshal([]byte(v), &extra); err != nil {
				return err
			}
			info.Settings = append(info.Settings, extra...)
		}

		// Until https://github.com/golang/go/issues/50603 is implemented,
		// manually construct something like a pseudo-version.
		// TODO: remove when this code is dead, hopefully in Go 1.22.
		if mod.Version == "(devel)" {
			var vcsTime time.Time
			var vcsRevision string
			for _, setting := range info.Settings {
				switch setting.Key {
				case "vcs.time":
					// If the format is invalid, we'll print a zero timestamp.
					vcsTime, _ = time.Parse(time.RFC3339Nano, setting.Value)
				case "vcs.revision":
					vcsRevision = setting.Value
					if len(vcsRevision) > 12 {
						vcsRevision = vcsRevision[:12]
					}
				}
			}
			if vcsRevision != "" {
				mod.Version = module.PseudoVersion("", "", vcsTime, vcsRevision)
			}
		}

		fmt.Printf("%s %s\n\n", mod.Path, mod.Version)
		fmt.Printf("Build settings:\n")
		for _, setting := range info.Settings {
			if setting.Value == "" {
				continue // do empty build settings even matter?
			}
			// The padding helps keep readability by aligning:
			//
			//   veryverylong.key value
			//          short.key some-other-value
			//
			// Empirically, 16 is enough; the longest key seen is "vcs.revision".
			fmt.Printf("%16s %s\n", setting.Key, setting.Value)
		}
		return nil
	case "reverse":
		return commandReverse(args)
	case "build", "test", "run":
		cmd, err := toolexecCmd(command, args)
		defer func() {
			if err := os.RemoveAll(os.Getenv("GARBLE_SHARED")); err != nil {
				fmt.Fprintf(os.Stderr, "could not clean up GARBLE_SHARED: %v\n", err)
			}
			// skip the trim if we didn't even start a build
			if sharedCache != nil {
				fsCache, err := openCache()
				if err == nil {
					err = fsCache.Trim()
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "could not trim GARBLE_CACHE: %v\n", err)
				}
			}
		}()
		if err != nil {
			return err
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		log.Printf("calling via toolexec: %s", cmd)
		return cmd.Run()

	case "toolexec":
		_, tool := filepath.Split(args[0])
		if runtime.GOOS == "windows" {
			tool = strings.TrimSuffix(tool, ".exe")
		}
		transform := transformMethods[tool]
		transformed := args[1:]
		if transform != nil {
			startTime := time.Now()
			log.Printf("transforming %s with args: %s", tool, strings.Join(transformed, " "))

			// We're in a toolexec sub-process, not directly called by the user.
			// Load the shared data and wrap the tool, like the compiler or linker.
			if err := loadSharedCache(); err != nil {
				return err
			}

			if len(args) == 2 && args[1] == "-V=full" {
				return alterToolVersion(tool, args)
			}
			var tf transformer
			toolexecImportPath := os.Getenv("TOOLEXEC_IMPORTPATH")
			tf.curPkg = sharedCache.ListedPackages[toolexecImportPath]
			if tf.curPkg == nil {
				return fmt.Errorf("TOOLEXEC_IMPORTPATH not found in listed packages: %s", toolexecImportPath)
			}
			tf.origImporter = importerForPkg(tf.curPkg)

			var err error
			if transformed, err = transform(&tf, transformed); err != nil {
				return err
			}
			log.Printf("transformed args for %s in %s: %s", tool, debugSince(startTime), strings.Join(transformed, " "))
		} else {
			log.Printf("skipping transform on %s with args: %s", tool, strings.Join(transformed, " "))
		}

		executablePath := args[0]
		if tool == "link" {
			modifiedLinkPath, unlock, err := linker.PatchLinker(sharedCache.GoEnv.GOROOT, sharedCache.GoEnv.GOVERSION, sharedCache.CacheDir, sharedTempDir)
			if err != nil {
				return fmt.Errorf("cannot get modified linker: %v", err)
			}
			defer unlock()

			executablePath = modifiedLinkPath
			os.Setenv(linker.MagicValueEnv, strconv.FormatUint(uint64(magicValue()), 10))
			os.Setenv(linker.EntryOffKeyEnv, strconv.FormatUint(uint64(entryOffKey()), 10))
			if flagTiny {
				os.Setenv(linker.TinyEnv, "true")
			}

			log.Printf("replaced linker with: %s", executablePath)
		}

		cmd := exec.Command(executablePath, transformed...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown command: %q", command)
	}
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
	for _, flag := range flags {
		if rxGarbleFlag.MatchString(flag) {
			return nil, fmt.Errorf("garble flags must precede command, like: garble %s build ./pkg", flag)
		}
	}

	// Here is the only place we initialize the cache.
	// The sub-processes will parse it from a shared gob file.
	sharedCache = &sharedCacheType{}

	// Note that we also need to pass build flags to 'go list', such
	// as -tags.
	sharedCache.ForwardBuildFlags, _ = filterForwardBuildFlags(flags)
	if command == "test" {
		sharedCache.ForwardBuildFlags = append(sharedCache.ForwardBuildFlags, "-test")
	}

	if err := fetchGoEnv(); err != nil {
		return nil, err
	}

	if !goVersionOK() {
		return nil, errJustExit(1)
	}

	var err error
	sharedCache.ExecPath, err = os.Executable()
	if err != nil {
		return nil, err
	}

	// Always an absolute directory; defaults to e.g. "~/.cache/garble".
	if dir := os.Getenv("GARBLE_CACHE"); dir != "" {
		sharedCache.CacheDir, err = filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
	} else {
		parentDir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		sharedCache.CacheDir = filepath.Join(parentDir, "garble")
	}

	binaryBuildID, err := buildidOf(sharedCache.ExecPath)
	if err != nil {
		return nil, err
	}
	sharedCache.BinaryContentID = decodeBuildIDHash(splitContentID(binaryBuildID))

	if err := appendListedPackages(args, true); err != nil {
		return nil, err
	}

	sharedTempDir, err = saveSharedCache()
	if err != nil {
		return nil, err
	}
	os.Setenv("GARBLE_SHARED", sharedTempDir)
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	os.Setenv("GARBLE_PARENT_WORK", wd)

	if flagDebugDir != "" {
		if !filepath.IsAbs(flagDebugDir) {
			flagDebugDir = filepath.Join(wd, flagDebugDir)
		}

		if err := os.RemoveAll(flagDebugDir); err != nil {
			return nil, fmt.Errorf("could not empty debugdir: %v", err)
		}
		if err := os.MkdirAll(flagDebugDir, 0o755); err != nil {
			return nil, err
		}
	}

	goArgs := append([]string{command}, garbleBuildFlags...)

	// Pass the garble flags down to each toolexec invocation.
	// This way, all garble processes see the same flag values.
	// Note that we can end up with a single argument to `go` in the form of:
	//
	//	-toolexec='/binary dir/garble' -tiny toolexec
	//
	// We quote the absolute path to garble if it contains spaces.
	// We can add extra flags to the end of the same -toolexec argument.
	var toolexecFlag strings.Builder
	toolexecFlag.WriteString("-toolexec=")
	quotedExecPath, err := cmdgoQuotedJoin([]string{sharedCache.ExecPath})
	if err != nil {
		// Can only happen if the absolute path to the garble binary contains
		// both single and double quotes. Seems extremely unlikely.
		return nil, err
	}
	toolexecFlag.WriteString(quotedExecPath)
	appendFlags(&toolexecFlag, false)
	toolexecFlag.WriteString(" toolexec")
	goArgs = append(goArgs, toolexecFlag.String())

	if flagControlFlow {
		goArgs = append(goArgs, "-debug-actiongraph", filepath.Join(sharedTempDir, actionGraphFileName))
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

var transformMethods = map[string]func(*transformer, []string) ([]string, error){
	"asm":     (*transformer).transformAsm,
	"compile": (*transformer).transformCompile,
	"link":    (*transformer).transformLink,
}

func (tf *transformer) transformAsm(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".s")

	// When assembling, the import path can make its way into the output object file.
	if tf.curPkg.Name != "main" && tf.curPkg.ToObfuscate {
		flags = flagSetValue(flags, "-p", tf.curPkg.obfuscatedImportPath())
	}

	flags = alterTrimpath(flags)

	// The assembler runs twice; the first with -gensymabis,
	// where we continue below and we obfuscate all the source.
	// The second time, without -gensymabis, we reconstruct the paths to the
	// obfuscated source files and reuse them to avoid work.
	newPaths := make([]string, 0, len(paths))
	if !slices.Contains(args, "-gensymabis") {
		for _, path := range paths {
			name := hashWithPackage(tf.curPkg, filepath.Base(path)) + ".s"
			pkgDir := filepath.Join(sharedTempDir, tf.curPkg.obfuscatedImportPath())
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

			// First, handle hash directives without leading whitespaces.

			// #include "foo.h"
			if quoted := strings.TrimPrefix(line, "#include"); quoted != line {
				quoted = strings.TrimSpace(quoted)
				path, err := strconv.Unquote(quoted)
				if err != nil {
					return nil, err
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

			// Leave "//" comments unchanged; they might be directives.
			line, comment, hasComment := strings.Cut(line, "//")

			// Anything else is regular assembly; replace the names.
			tf.replaceAsmNames(&buf, []byte(line))

			if hasComment {
				buf.WriteString("//")
				buf.WriteString(comment)
			}
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
		// Note that we don't obfuscate the package path "main".
		lpkg := tf.curPkg
		if asmPkgPath != "" && asmPkgPath != "main" {
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

		if lpkg.ToObfuscate && !compilerIntrinsicsFuncs[lpkg.ImportPath+"."+name] {
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
	pkgDir := filepath.Join(sharedTempDir, tf.curPkg.obfuscatedImportPath())
	if err := os.MkdirAll(pkgDir, 0o777); err != nil {
		return "", err
	}
	dstPath := filepath.Join(pkgDir, obfuscated)
	if err := writeFileExclusive(dstPath, content); err != nil {
		return "", err
	}
	return dstPath, nil
}

// parseFiles parses a list of Go files.
// It supports relative file paths, such as those found in listedPackage.CompiledGoFiles,
// as long as dir is set to listedPackage.Dir.
func parseFiles(dir string, paths []string) ([]*ast.File, error) {
	var files []*ast.File
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

func (tf *transformer) transformCompile(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".go")

	// We will force the linker to drop DWARF via -w, so don't spend time
	// generating it.
	flags = append(flags, "-dwarf=false")

	// The Go file paths given to the compiler are always absolute paths.
	files, err := parseFiles("", paths)
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

	// If this is a package to obfuscate, swap the -p flag with the new package path.
	// We don't if it's the main package, as that just uses "-p main".
	// We only set newPkgPath if we're obfuscating the import path,
	// to replace the original package name in the package clause below.
	newPkgPath := ""
	if tf.curPkg.Name != "main" && tf.curPkg.ToObfuscate {
		newPkgPath = tf.curPkg.obfuscatedImportPath()
		flags = flagSetValue(flags, "-p", newPkgPath)
	}

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
		tf.transformDirectives(file.Comments)
		file = tf.transformGoFile(file)
		// newPkgPath might be the original ImportPath in some edge cases like
		// compilerIntrinsics; we don't want to use slashes in package names.
		// TODO: when we do away with those edge cases, only check the string is
		// non-empty.
		if newPkgPath != "" && newPkgPath != tf.curPkg.ImportPath {
			file.Name.Name = newPkgPath
		}

		src, err := printFile(tf.curPkg, file)
		if err != nil {
			return nil, err
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
func (tf *transformer) transformDirectives(comments []*ast.CommentGroup) {
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
}

func (tf *transformer) transformLinkname(localName, newName string) (string, string) {
	// obfuscate the local name, if the current package is obfuscated
	if tf.curPkg.ToObfuscate && !compilerIntrinsicsFuncs[tf.curPkg.ImportPath+"."+localName] {
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
	var lpkg *listedPackage
	var foreignName string
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

	if !lpkg.ToObfuscate || compilerIntrinsicsFuncs[lpkg.ImportPath+"."+foreignName] {
		// We're not obfuscating that package or name.
		return localName, newName
	}

	var newForeignName string
	if receiver, name, ok := strings.Cut(foreignName, "."); ok {
		if lpkg.ImportPath == "reflect" && (receiver == "(*rtype)" || receiver == "Value") {
			// These receivers are not obfuscated.
			// See the TODO below.
		} else if strings.HasPrefix(receiver, "(*") {
			// pkg/path.(*Receiver).method
			receiver = strings.TrimPrefix(receiver, "(*")
			receiver = strings.TrimSuffix(receiver, ")")
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

	newPkgPath := lpkg.ImportPath
	if newPkgPath != "main" {
		newPkgPath = lpkg.obfuscatedImportPath()
	}
	newName = newPkgPath + "." + newForeignName
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
			newIndirectImports[pkg] = true
		}
	}

	for _, line := range strings.Split(string(data), "\n") {
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
			panic(err) // shouldn't happen
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
			// This still triggers as of go1.21.
			if strings.HasSuffix(tf.curPkg.ImportPath, ".test]") && strings.HasPrefix(tf.curPkg.ImportPath, impPath) {
				continue
			}
			panic(err) // shouldn't happen
		}
		if lpkg.Name != "main" {
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
	funcFullName = string // as per go/types.Func.FullName
	objectString = string // as per recordedObjectString

	typeName struct {
		PkgPath string // empty if builtin
		Name    string
	}
)

// pkgCache contains information about a package that will be stored in fsCache.
// Note that pkgCache is "deep", containing information about all packages
// which are transitive dependencies as well.
type pkgCache struct {
	// ReflectAPIs is a static record of what std APIs use reflection on their
	// parameters, so we can avoid obfuscating types used with them.
	//
	// TODO: we're not including fmt.Printf, as it would have many false positives,
	// unless we were smart enough to detect which arguments get used as %#v or %T.
	ReflectAPIs map[funcFullName]map[int]bool

	// ReflectObjects is filled with the fully qualified names from each
	// package that we cannot obfuscate due to reflection.
	// The included objects are named types and their fields,
	// since it is those names being obfuscated that could break the use of reflect.
	//
	// This record is necessary for knowing what names from imported packages
	// weren't obfuscated, so we can obfuscate their local uses accordingly.
	ReflectObjects map[objectString]struct{}

	// EmbeddedAliasFields records which embedded fields use a type alias.
	// They are the only instance where a type alias matters for obfuscation,
	// because the embedded field name is derived from the type alias itself,
	// and not the type that the alias points to.
	// In that way, the type alias is obfuscated as a form of named type,
	// bearing in mind that it may be owned by a different package.
	EmbeddedAliasFields map[objectString]typeName
}

func (c *pkgCache) CopyFrom(c2 pkgCache) {
	maps.Copy(c.ReflectAPIs, c2.ReflectAPIs)
	maps.Copy(c.ReflectObjects, c2.ReflectObjects)
	maps.Copy(c.EmbeddedAliasFields, c2.EmbeddedAliasFields)
}

func ssaBuildPkg(pkg *types.Package, files []*ast.File, info *types.Info) *ssa.Package {
	// Create SSA packages for all imports. Order is not significant.
	ssaProg := ssa.NewProgram(fset, 0)
	created := make(map[*types.Package]bool)
	var createAll func(pkgs []*types.Package)
	createAll = func(pkgs []*types.Package) {
		for _, p := range pkgs {
			if !created[p] {
				created[p] = true
				ssaProg.CreatePackage(p, nil, nil, true)
				createAll(p.Imports())
			}
		}
	}
	createAll(pkg.Imports())

	ssaPkg := ssaProg.CreatePackage(pkg, files, info, false)
	ssaPkg.Build()
	return ssaPkg
}

func openCache() (*cache.Cache, error) {
	// Use a subdirectory for the hashed build cache, to clarify what it is,
	// and to allow us to have other directories or files later on without mixing.
	dir := filepath.Join(sharedCache.CacheDir, "build")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	return cache.Open(dir)
}

func loadPkgCache(lpkg *listedPackage, pkg *types.Package, files []*ast.File, info *types.Info, ssaPkg *ssa.Package) (pkgCache, error) {
	fsCache, err := openCache()
	if err != nil {
		return pkgCache{}, err
	}
	filename, _, err := fsCache.GetFile(lpkg.GarbleActionID)
	// Already in the cache; load it directly.
	if err == nil {
		f, err := os.Open(filename)
		if err != nil {
			return pkgCache{}, err
		}
		defer f.Close()
		var loaded pkgCache
		if err := gob.NewDecoder(f).Decode(&loaded); err != nil {
			return pkgCache{}, fmt.Errorf("gob decode: %w", err)
		}
		return loaded, nil
	}
	return computePkgCache(fsCache, lpkg, pkg, files, info, ssaPkg)
}

func computePkgCache(fsCache *cache.Cache, lpkg *listedPackage, pkg *types.Package, files []*ast.File, info *types.Info, ssaPkg *ssa.Package) (pkgCache, error) {
	// Not yet in the cache. Load the cache entries for all direct dependencies,
	// build our cache entry, and write it to disk.
	// Note that practically all errors from Cache.GetFile are a cache miss;
	// for example, a file might exist but be empty if another process
	// is filling the same cache entry concurrently.
	//
	// TODO: if A (curPkg) imports B and C, and B also imports C,
	// then loading the gob files from both B and C is unnecessary;
	// loading B's gob file would be enough. Is there an easy way to do that?
	computed := pkgCache{
		ReflectAPIs: map[funcFullName]map[int]bool{
			"reflect.TypeOf":  {0: true},
			"reflect.ValueOf": {0: true},
		},
		ReflectObjects:      map[objectString]struct{}{},
		EmbeddedAliasFields: map[objectString]typeName{},
	}
	for _, imp := range lpkg.Imports {
		if imp == "C" {
			// `go list -json` shows "C" in Imports but not Deps.
			// See https://go.dev/issue/60453.
			continue
		}
		// Shadowing lpkg ensures we don't use the wrong listedPackage below.
		lpkg, err := listPackage(lpkg, imp)
		if err != nil {
			panic(err) // shouldn't happen
		}
		if lpkg.BuildID == "" {
			continue // nothing to load
		}
		if err := func() error { // function literal for the deferred close
			if filename, _, err := fsCache.GetFile(lpkg.GarbleActionID); err == nil {
				// Cache hit; append new entries to computed.
				f, err := os.Open(filename)
				if err != nil {
					return err
				}
				defer f.Close()
				if err := gob.NewDecoder(f).Decode(&computed); err != nil {
					return fmt.Errorf("gob decode: %w", err)
				}
				return nil
			}
			// Missing or corrupted entry in the cache for a dependency.
			// Could happen if GARBLE_CACHE was emptied but GOCACHE was not.
			// Compute it, which can recurse if many entries are missing.
			files, err := parseFiles(lpkg.Dir, lpkg.CompiledGoFiles)
			if err != nil {
				return err
			}
			origImporter := importerForPkg(lpkg)
			pkg, info, err := typecheck(lpkg.ImportPath, files, origImporter)
			if err != nil {
				return err
			}
			computedImp, err := computePkgCache(fsCache, lpkg, pkg, files, info, nil)
			if err != nil {
				return err
			}
			computed.CopyFrom(computedImp)
			return nil
		}(); err != nil {
			return pkgCache{}, fmt.Errorf("pkgCache load for %s: %w", imp, err)
		}
	}

	// Fill EmbeddedAliasFields from the type info.
	for name, obj := range info.Uses {
		obj, ok := obj.(*types.TypeName)
		if !ok || !obj.IsAlias() {
			continue
		}
		vr, _ := info.Defs[name].(*types.Var)
		if vr == nil || !vr.Embedded() {
			continue
		}
		vrStr := recordedObjectString(vr)
		if vrStr == "" {
			continue
		}
		aliasTypeName := typeName{
			Name: obj.Name(),
		}
		if pkg := obj.Pkg(); pkg != nil {
			aliasTypeName.PkgPath = pkg.Path()
		}
		computed.EmbeddedAliasFields[vrStr] = aliasTypeName
	}

	// Fill the reflect info from SSA, which builds on top of the syntax tree and type info.
	inspector := reflectInspector{
		pkg:             pkg,
		checkedAPIs:     make(map[string]bool),
		propagatedInstr: map[ssa.Instruction]bool{},
		result:          computed, // append the results
	}
	if ssaPkg == nil {
		ssaPkg = ssaBuildPkg(pkg, files, info)
	}
	inspector.recordReflection(ssaPkg)

	// Unlikely that we could stream the gob encode, as cache.Put wants an io.ReadSeeker.
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(computed); err != nil {
		return pkgCache{}, err
	}
	if err := fsCache.PutBytes(lpkg.GarbleActionID, buf.Bytes()); err != nil {
		return pkgCache{}, err
	}
	return computed, nil
}

// cmd/bundle will include a go:generate directive in its output by default.
// Ours specifies a version and doesn't assume bundle is in $PATH, so drop it.

//go:generate go run golang.org/x/tools/cmd/bundle -o cmdgo_quoted.go -prefix cmdgoQuoted cmd/internal/quoted
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

		// -X represents the main package as "main", not its import path.
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
	// TODO(mvdan): we should probably set types.Config.GoVersion from go.mod
	origTypesConfig := types.Config{Importer: origImporter}
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
	if origin == nil {
		origin = used
	}
	type Container interface{ Elem() types.Type }
	switch used := used.(type) {
	case Container:
		// origin may be a *types.TypeParam, which is not a Container.
		// For now, we haven't found a need to recurse in that case.
		// We can edit this code in the future if we find an example,
		// because we panic if a field is not in fieldToStruct.
		if origin, ok := origin.(Container); ok {
			recordType(used.Elem(), origin.Elem(), done, fieldToStruct)
		}
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
		for i := 0; i < used.NumFields(); i++ {
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
func isSafeForInstanceType(typ types.Type) bool {
	switch t := typ.(type) {
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

		// Simple import has no ast.Ident and is stored in Implicits separately.
		pkgObj := tf.info.Implicits[imp]
		if pkgObj == nil {
			pkgObj = tf.info.Defs[imp.Name] // renamed or dot import
		}

		pkgScope := pkgObj.(*types.PkgName).Imported().Scope()
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
				X:   ast.NewIdent(pkgObj.Name()),
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
		file = literals.Obfuscate(tf.obfRand, file, tf.info, tf.linkerVariableStrings)

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
			vrStr := recordedObjectString(vr)
			aliasTypeName, ok := tf.curPkgCache.EmbeddedAliasFields[vrStr]
			if ok {
				aliasScope := tf.pkg.Scope()
				if path := aliasTypeName.PkgPath; path == "" {
					aliasScope = types.Universe
				} else if path != tf.pkg.Path() {
					// If the package is a dependency, import it.
					// We can't grab the package via tf.pkg.Imports,
					// because some of the packages under there are incomplete.
					// ImportFrom will cache complete imports, anyway.
					pkg2, err := tf.origImporter.ImportFrom(path, parentWorkDir, 0)
					if err != nil {
						panic(err)
					}
					aliasScope = pkg2.Scope()
				}
				tname, ok := aliasScope.Lookup(aliasTypeName.Name).(*types.TypeName)
				if !ok {
					panic(fmt.Sprintf("EmbeddedAliasFields pointed %q to a missing type %q", vrStr, aliasTypeName))
				}
				if !tname.IsAlias() {
					panic(fmt.Sprintf("EmbeddedAliasFields pointed %q to a non-alias type %q", vrStr, aliasTypeName))
				}
				obj = tname
			} else {
				named := namedType(obj.Type())
				if named == nil {
					return true // unnamed type (probably a basic type, e.g. int)
				}
				obj = named.Obj()
			}
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

		// The package that declared this object did not obfuscate it.
		if usedForReflect(tf.curPkgCache, obj) {
			return true
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
			if compilerIntrinsicsFuncs[path+"."+name] {
				return true
			}

			sign := obj.Type().(*types.Signature)
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
		// If the import was unnamed, give it the name of the
		// original package name, to keep references working.
		lpkg, err := listPackage(tf.curPkg, path)
		if err != nil {
			panic(err) // should never happen
		}
		if !lpkg.ToObfuscate {
			return true
		}
		if lpkg.Name != "main" {
			newPath := lpkg.obfuscatedImportPath()
			imp.Path.Value = strconv.Quote(newPath)
		}
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
		// package we are linking.
		// Otherwise, find it in the cache.
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
		// As before, the main package must remain as "main".
		newPath := path
		if path != "main" {
			newPath = lpkg.obfuscatedImportPath()
		}
		newName := hashWithPackage(lpkg, name)
		flags = append(flags, fmt.Sprintf("-X=%s.%s=%s", newPath, newName, stringValue))
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
	trimpath := flagValue(flags, "-trimpath")

	// Add our temporary dir to the beginning of -trimpath, so that we don't
	// leak temporary dirs. Needs to be at the beginning, since there may be
	// shorter prefixes later in the list, such as $PWD if TMPDIR=$PWD/tmp.
	return flagSetValue(flags, "-trimpath", sharedTempDir+"=>;"+trimpath)
}

// forwardBuildFlags is obtained from 'go help build' as of Go 1.21.
var forwardBuildFlags = map[string]bool{
	// These shouldn't be used in nested cmd/go calls.
	"-a": false,
	"-n": false,
	"-x": false,
	"-v": false,

	// These are always set by garble.
	"-trimpath": false,
	"-toolexec": false,
	"-buildvcs": false,

	"-C":             true,
	"-asan":          true,
	"-asmflags":      true,
	"-buildmode":     true,
	"-compiler":      true,
	"-cover":         true,
	"-covermode":     true,
	"-coverpkg":      true,
	"-gccgoflags":    true,
	"-gcflags":       true,
	"-installsuffix": true,
	"-ldflags":       true,
	"-linkshared":    true,
	"-mod":           true,
	"-modcacherw":    true,
	"-modfile":       true,
	"-msan":          true,
	"-overlay":       true,
	"-p":             true,
	"-pgo":           true,
	"-pkgdir":        true,
	"-race":          true,
	"-tags":          true,
	"-work":          true,
	"-workfile":      true,
}

// booleanFlags is obtained from 'go help build' and 'go help testflag' as of Go 1.21.
var booleanFlags = map[string]bool{
	// Shared build flags.
	"-a":          true,
	"-asan":       true,
	"-buildvcs":   true,
	"-cover":      true,
	"-i":          true,
	"-linkshared": true,
	"-modcacherw": true,
	"-msan":       true,
	"-n":          true,
	"-race":       true,
	"-trimpath":   true,
	"-v":          true,
	"-work":       true,
	"-x":          true,

	// Test flags (TODO: support its special -args flag)
	"-benchmem": true,
	"-c":        true,
	"-failfast": true,
	"-fullpath": true,
	"-json":     true,
	"-short":    true,
}

func filterForwardBuildFlags(flags []string) (filtered []string, firstUnknown string) {
	for i := 0; i < len(flags); i++ {
		arg := flags[i]
		if strings.HasPrefix(arg, "--") {
			arg = arg[1:] // "--name" to "-name"; keep the short form
		}

		name, _, _ := strings.Cut(arg, "=") // "-name=value" to "-name"

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
//
// We iterate from the end rather than from the start, to better protect
// oursrelves from flag arguments that may look like paths, such as:
//
//	compile [flags...] -p pkg/path.go [more flags...] file1.go file2.go
//
// For now, since those confusing flags are always followed by more flags,
// iterating in reverse order works around them entirely.
func splitFlagsFromFiles(all []string, ext string) (flags, paths []string) {
	for i := len(all) - 1; i >= 0; i-- {
		arg := all[i]
		if strings.HasPrefix(arg, "-") || !strings.HasSuffix(arg, ext) {
			cutoff := i + 1 // arg is a flag, not a path
			return all[:cutoff:cutoff], all[cutoff:]
		}
	}
	return nil, all
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
		// Keep in sync with sharedCache.GoEnv.
		"GOOS", "GOMOD", "GOVERSION", "GOROOT",
	).CombinedOutput()
	if err != nil {
		// TODO: cover this in the tests.
		fmt.Fprintf(os.Stderr, `Can't find the Go toolchain: %v

This is likely due to Go not being installed/setup correctly.

To install Go, see: https://go.dev/doc/install
`, err)
		return errJustExit(1)
	}
	if err := json.Unmarshal(out, &sharedCache.GoEnv); err != nil {
		return fmt.Errorf(`cannot unmarshal from "go env -json": %w`, err)
	}
	sharedCache.GOGARBLE = os.Getenv("GOGARBLE")
	if sharedCache.GOGARBLE == "" {
		sharedCache.GOGARBLE = "*" // we default to obfuscating everything
	}
	return nil
}
