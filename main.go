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

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/tools/go/ast/astutil"
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

	// obfRand is initialized by transformCompile and used during obfuscation.
	// It is left nil at init time, so that we only use it after it has been
	// properly initialized with a deterministic seed.
	// It must only be used for deterministic obfuscation;
	// if it is used for any other purpose, we may lose determinism.
	obfRand *mathrand.Rand
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
	if err := mainErr(args); err != nil {
		if code, ok := err.(errJustExit); ok {
			return int(code)
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

// toolchainVersionSemver is a semver-compatible version of the Go toolchain currently
// being used, as reported by "go env GOVERSION".
// Note that the version of Go that built the garble binary might be newer.
var toolchainVersionSemver string

func goVersionOK() bool {
	const (
		minGoVersionSemver = "v1.20.0"
		suggestedGoVersion = "1.20.x"
	)

	// rxVersion looks for a version like "go1.2" or "go1.2.3"
	rxVersion := regexp.MustCompile(`go\d+\.\d+(?:\.\d+)?`)

	toolchainVersionFull := cache.GoEnv.GOVERSION
	toolchainVersion := rxVersion.FindString(cache.GoEnv.GOVERSION)
	if toolchainVersion == "" {
		// Go 1.15.x and older do not have GOVERSION yet.
		// We could go the extra mile and fetch it via 'go toolchainVersion',
		// but we'd have to error anyway.
		fmt.Fprintf(os.Stderr, "Go version is too old; please upgrade to Go %s or newer\n", suggestedGoVersion)
		return false
	}

	toolchainVersionSemver = "v" + strings.TrimPrefix(toolchainVersion, "go")
	if semver.Compare(toolchainVersionSemver, minGoVersionSemver) < 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too old; please upgrade to Go %s or newer\n", toolchainVersionFull, suggestedGoVersion)
		return false
	}

	// Ensure that the version of Go that built the garble binary is equal or
	// newer than toolchainVersionSemver.
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
	if semver.Compare(builtVersionSemver, toolchainVersionSemver) < 0 {
		fmt.Fprintf(os.Stderr, "garble was built with %q and is being used with %q; please rebuild garble with the newer version\n",
			builtVersionFull, toolchainVersionFull)
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
		// TODO: remove when this code is dead, hopefully in Go 1.21.
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
	case "build", "test":
		cmd, err := toolexecCmd(command, args)
		defer os.RemoveAll(os.Getenv("GARBLE_SHARED"))
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
		transform := transformFuncs[tool]
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

			toolexecImportPath := os.Getenv("TOOLEXEC_IMPORTPATH")
			curPkg = cache.ListedPackages[toolexecImportPath]
			if curPkg == nil {
				return fmt.Errorf("TOOLEXEC_IMPORTPATH not found in listed packages: %s", toolexecImportPath)
			}

			var err error
			if transformed, err = transform(transformed); err != nil {
				return err
			}
			log.Printf("transformed args for %s in %s: %s", tool, debugSince(startTime), strings.Join(transformed, " "))
		} else {
			log.Printf("skipping transform on %s with args: %s", tool, strings.Join(transformed, " "))
		}

		executablePath := args[0]
		if tool == "link" {
			modifiedLinkPath, unlock, err := linker.PatchLinker(cache.GoEnv.GOROOT, cache.GoEnv.GOVERSION, cache.GoEnv.GOEXE, sharedTempDir)
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

	binaryBuildID, err := buildidOf(cache.ExecPath)
	if err != nil {
		return nil, err
	}
	cache.BinaryContentID = decodeHash(splitContentID(binaryBuildID))

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
	quotedExecPath, err := cmdgoQuotedJoin([]string{cache.ExecPath})
	if err != nil {
		// Can only happen if the absolute path to the garble binary contains
		// both single and double quotes. Seems extremely unlikely.
		return nil, err
	}
	toolexecFlag.WriteString(quotedExecPath)
	appendFlags(&toolexecFlag, false)
	toolexecFlag.WriteString(" toolexec")
	goArgs = append(goArgs, toolexecFlag.String())

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

var transformFuncs = map[string]func([]string) ([]string, error){
	"asm":     transformAsm,
	"compile": transformCompile,
	"link":    transformLink,
}

func transformAsm(args []string) ([]string, error) {
	flags, paths := splitFlagsFromFiles(args, ".s")

	// When assembling, the import path can make its way into the output object file.
	if curPkg.Name != "main" && curPkg.ToObfuscate {
		flags = flagSetValue(flags, "-p", curPkg.obfuscatedImportPath())
	}

	flags = alterTrimpath(flags)

	// The assembler runs twice; the first with -gensymabis,
	// where we continue below and we obfuscate all the source.
	// The second time, without -gensymabis, we reconstruct the paths to the
	// obfuscated source files and reuse them to avoid work.
	newPaths := make([]string, 0, len(paths))
	if !slices.Contains(args, "-gensymabis") {
		for _, path := range paths {
			name := hashWithPackage(curPkg, filepath.Base(path)) + ".s"
			pkgDir := filepath.Join(sharedTempDir, curPkg.obfuscatedImportPath())
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
					replaceAsmNames(&includeBuf, content)

					// For now, we replace `foo.h` or `dir/foo.h` with `garbled_foo.h`.
					// The different name ensures we don't use the unobfuscated file.
					// This is far from perfect, but does the job for the time being.
					// In the future, use a randomized name.
					basename := filepath.Base(path)
					newPath = "garbled_" + basename

					if _, err := writeSourceFile(basename, newPath, includeBuf.Bytes()); err != nil {
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
			replaceAsmNames(&buf, []byte(line))

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
		newName := hashWithPackage(curPkg, basename) + ".s"
		if path, err := writeSourceFile(basename, newName, buf.Bytes()); err != nil {
			return nil, err
		} else {
			newPaths = append(newPaths, path)
		}
		f.Close() // do not keep len(paths) files open
	}

	return append(flags, newPaths...), nil
}

func replaceAsmNames(buf *bytes.Buffer, remaining []byte) {
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
		lpkg := curPkg
		if asmPkgPath != "" && asmPkgPath != "main" {
			if asmPkgPath != curPkg.Name {
				goPkgPath := asmPkgPath
				goPkgPath = strings.ReplaceAll(goPkgPath, string(asmPeriod), string(goPeriod))
				goPkgPath = strings.ReplaceAll(goPkgPath, string(asmSlash), string(goSlash))
				var err error
				lpkg, err = listPackage(goPkgPath)
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
				log.Printf("asm name %q hashed with %x to %q", name, curPkg.GarbleActionID, newName)
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
func writeSourceFile(basename, obfuscated string, content []byte) (string, error) {
	// Uncomment for some quick debugging. Do not delete.
	// fmt.Fprintf(os.Stderr, "\n-- %s/%s --\n%s", curPkg.ImportPath, basename, content)

	if flagDebugDir != "" {
		pkgDir := filepath.Join(flagDebugDir, filepath.FromSlash(curPkg.ImportPath))
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
	pkgDir := filepath.Join(sharedTempDir, curPkg.obfuscatedImportPath())
	if err := os.MkdirAll(pkgDir, 0o777); err != nil {
		return "", err
	}
	dstPath := filepath.Join(pkgDir, obfuscated)
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

	var files []*ast.File
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
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

	// Literal obfuscation uses math/rand, so seed it deterministically.
	randSeed := curPkg.GarbleActionID
	if flagSeed.present() {
		randSeed = flagSeed.bytes
	}
	// log.Printf("seeding math/rand with %x\n", randSeed)
	obfRand = mathrand.New(mathrand.NewSource(int64(binary.BigEndian.Uint64(randSeed))))

	if err := tf.prefillObjectMaps(files); err != nil {
		return nil, err
	}

	// If this is a package to obfuscate, swap the -p flag with the new package path.
	// We don't if it's the main package, as that just uses "-p main".
	// We only set newPkgPath if we're obfuscating the import path,
	// to replace the original package name in the package clause below.
	newPkgPath := ""
	if curPkg.Name != "main" && curPkg.ToObfuscate {
		newPkgPath = curPkg.obfuscatedImportPath()
		flags = flagSetValue(flags, "-p", newPkgPath)
	}

	newPaths := make([]string, 0, len(files))

	for i, file := range files {
		basename := filepath.Base(paths[i])
		log.Printf("obfuscating %s", basename)
		if curPkg.ImportPath == "runtime" {
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
		tf.handleDirectives(file.Comments)
		file = tf.transformGoFile(file)
		// newPkgPath might be the original ImportPath in some edge cases like
		// compilerIntrinsics; we don't want to use slashes in package names.
		// TODO: when we do away with those edge cases, only check the string is
		// non-empty.
		if newPkgPath != "" && newPkgPath != curPkg.ImportPath {
			file.Name.Name = newPkgPath
		}

		src, err := printFile(file)
		if err != nil {
			return nil, err
		}
		// It is possible to end up in an edge case where two instances of the
		// same package have different Action IDs, but their obfuscation and
		// builds produce exactly the same results.
		// In such an edge case, Go's build cache is smart enough for the second
		// instance to reuse the first's build artifact.
		// However, garble's caching via garbleExportFile is not as smart,
		// as we base the location of these files purely based on Action IDs.
		// Thus, the incremental build can fail to find garble's cached file.
		// To sidestep this bug entirely, ensure that different action IDs never
		// produce the same cached output when building with garble.
		// Note that this edge case tends to happen when a -seed is provided,
		// as then a package's Action ID is not used as an obfuscation seed.
		// TODO(mvdan): replace this workaround with an actual fix if we can.
		// This workaround is presumably worse on the build cache,
		// as we end up with extra near-duplicate cached artifacts.
		if i == 0 {
			src = append(src, fmt.Sprintf(
				"\nvar garbleActionID = %q\n", hashToString(curPkg.GarbleActionID),
			)...)
		}

		// We hide Go source filenames via "//line" directives,
		// so there is no need to use obfuscated filenames here.
		if path, err := writeSourceFile(basename, basename, src); err != nil {
			return nil, err
		} else {
			newPaths = append(newPaths, path)
		}
	}
	flags = flagSetValue(flags, "-importcfg", newImportCfg)

	if err := writeGobExclusive(
		garbleExportFile(curPkg),
		cachedOutput,
	); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}

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
	if curPkg.ToObfuscate && !compilerIntrinsicsFuncs[curPkg.ImportPath+"."+localName] {
		localName = hashWithPackage(curPkg, localName)
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

		var err error
		lpkg, err = listPackage(pkgPath)
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
		lpkg, err := listPackage(beforePath)
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
	for _, pair := range packagefiles {
		impPath, pkgfile := pair[0], pair[1]
		lpkg, err := listPackage(impPath)
		if err != nil {
			// TODO: it's unclear why an importcfg can include an import path
			// that's not a dependency in an edge case with "go test ./...".
			// See exporttest/*.go in testdata/scripts/test.txt.
			// For now, spot the pattern and avoid the unnecessary error;
			// the dependency is unused, so the packagefile line is redundant.
			// This still triggers as of go1.20.
			if strings.HasSuffix(curPkg.ImportPath, ".test]") && strings.HasPrefix(curPkg.ImportPath, impPath) {
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

	reflectParameter struct {
		Position int  // 0-indexed
		Variadic bool // ...int
	}

	typeName struct {
		PkgPath, Name string
	}
)

// TODO: read-write globals like these should probably be inside transformer

// knownCannotObfuscateUnexported is like KnownCannotObfuscate but for
// unexported names. We don't need to store this in the build cache,
// because these names cannot be referenced by downstream packages.
var knownCannotObfuscateUnexported = map[types.Object]bool{}

// cachedOutput contains information that will be stored as per garbleExportFile.
// Note that cachedOutput gets loaded from all direct package dependencies,
// and gets filled while obfuscating the current package, so it ends up
// containing entries for the current package and its transitive dependencies.
var cachedOutput = struct {
	// KnownReflectAPIs is a static record of what std APIs use reflection on their
	// parameters, so we can avoid obfuscating types used with them.
	//
	// TODO: we're not including fmt.Printf, as it would have many false positives,
	// unless we were smart enough to detect which arguments get used as %#v or %T.
	KnownReflectAPIs map[funcFullName][]reflectParameter

	// KnownCannotObfuscate is filled with the fully qualified names from each
	// package that we cannot obfuscate.
	// This record is necessary for knowing what names from imported packages
	// weren't obfuscated, so we can obfuscate their local uses accordingly.
	KnownCannotObfuscate map[objectString]struct{}

	// KnownEmbeddedAliasFields records which embedded fields use a type alias.
	// They are the only instance where a type alias matters for obfuscation,
	// because the embedded field name is derived from the type alias itself,
	// and not the type that the alias points to.
	// In that way, the type alias is obfuscated as a form of named type,
	// bearing in mind that it may be owned by a different package.
	KnownEmbeddedAliasFields map[objectString]typeName
}{
	KnownReflectAPIs: map[funcFullName][]reflectParameter{
		"reflect.TypeOf":  {{Position: 0, Variadic: false}},
		"reflect.ValueOf": {{Position: 0, Variadic: false}},
	},
	KnownCannotObfuscate:     map[objectString]struct{}{},
	KnownEmbeddedAliasFields: map[objectString]typeName{},
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
	startTime := time.Now()
	loaded := 0
	for _, path := range curPkg.Deps {
		pkg, err := listPackage(path)
		if err != nil {
			panic(err) // shouldn't happen
		}
		if pkg.Export == "" {
			continue // nothing to load
		}
		// this function literal is used for the deferred close
		if err := func() error {
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
		}(); err != nil {
			return fmt.Errorf("cannot load garble export file for %s: %w", path, err)
		}
		loaded++
	}
	log.Printf("%d cached output files loaded in %s", loaded, debugSince(startTime))
	return nil
}

func (tf *transformer) findReflectFunctions(files []*ast.File) {
	seenReflectParams := make(map[*types.Var]bool)
	visitFuncDecl := func(funcDecl *ast.FuncDecl) {
		funcObj := tf.info.Defs[funcDecl.Name].(*types.Func)
		funcType := funcObj.Type().(*types.Signature)
		funcParams := funcType.Params()

		maps.Clear(seenReflectParams)
		for i := 0; i < funcParams.Len(); i++ {
			seenReflectParams[funcParams.At(i)] = false
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
			calledFunc, _ := tf.info.Uses[sel.Sel].(*types.Func)
			if calledFunc == nil || calledFunc.Pkg() == nil {
				return true
			}

			fullName := calledFunc.FullName()
			for _, reflectParam := range cachedOutput.KnownReflectAPIs[fullName] {
				// We need a range to handle any number of variadic arguments,
				// which could be 0 or multiple.
				// The non-variadic case is always one argument,
				// but we still use the range to deduplicate code.
				argStart := reflectParam.Position
				argEnd := argStart + 1
				if reflectParam.Variadic {
					argEnd = len(call.Args)
				}
				for _, arg := range call.Args[argStart:argEnd] {
					ident, ok := arg.(*ast.Ident)
					if !ok {
						continue
					}
					obj, _ := tf.info.Uses[ident].(*types.Var)
					if obj == nil {
						continue
					}
					if _, ok := seenReflectParams[obj]; ok {
						seenReflectParams[obj] = true
					}
				}
			}

			var reflectParams []reflectParameter
			for i := 0; i < funcParams.Len(); i++ {
				if seenReflectParams[funcParams.At(i)] {
					reflectParams = append(reflectParams, reflectParameter{
						Position: i,
						Variadic: funcType.Variadic() && i == funcParams.Len()-1,
					})
				}
			}
			if len(reflectParams) > 0 {
				cachedOutput.KnownReflectAPIs[funcObj.FullName()] = reflectParams
			}

			return true
		})
	}

	lenPrevKnownReflectAPIs := len(cachedOutput.KnownReflectAPIs)
	for _, file := range files {
		for _, decl := range file.Decls {
			if decl, ok := decl.(*ast.FuncDecl); ok {
				visitFuncDecl(decl)
			}
		}
	}

	// if a new reflectAPI is found we need to Re-evaluate all functions which might be using that API
	if len(cachedOutput.KnownReflectAPIs) > lenPrevKnownReflectAPIs {
		tf.findReflectFunctions(files)
	}
}

// cmd/bundle will include a go:generate directive in its output by default.
// Ours specifies a version and doesn't assume bundle is in $PATH, so drop it.

//go:generate go run golang.org/x/tools/cmd/bundle@v0.5.0 -o cmdgo_quoted.go -prefix cmdgoQuoted cmd/internal/quoted
//go:generate sed -i /go:generate/d cmdgo_quoted.go

// prefillObjectMaps collects objects which should not be obfuscated,
// such as those used as arguments to reflect.TypeOf or reflect.ValueOf.
// Since we obfuscate one package at a time, we only detect those if the type
// definition and the reflect usage are both in the same package.
func (tf *transformer) prefillObjectMaps(files []*ast.File) error {
	tf.linkerVariableStrings = make(map[*types.Var]string)

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
	ldflags, err := cmdgoQuotedSplit(flagValue(cache.ForwardBuildFlags, "-ldflags"))
	if err != nil {
		return err
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
		if path != curPkg.ImportPath && (path != "main" || curPkg.Name != "main") {
			return // not the current package
		}

		obj, _ := tf.pkg.Scope().Lookup(name).(*types.Var)
		if obj == nil {
			return // no such variable; skip
		}
		tf.linkerVariableStrings[obj] = stringValue
	})

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

		fnType, _ := tf.info.Uses[ident].(*types.Func)
		if fnType == nil || fnType.Pkg() == nil {
			return true
		}

		fullName := fnType.FullName()
		for _, reflectParam := range cachedOutput.KnownReflectAPIs[fullName] {
			argStart := reflectParam.Position
			argEnd := argStart + 1
			if reflectParam.Variadic {
				argEnd = len(call.Args)
			}
			for _, arg := range call.Args[argStart:argEnd] {
				argType := tf.info.TypeOf(arg)
				tf.recursivelyRecordAsNotObfuscated(argType)
			}
		}

		return true
	}
	for _, file := range files {
		ast.Inspect(file, visit)
	}
	return nil
}

// transformer holds all the information and state necessary to obfuscate a
// single Go package.
type transformer struct {
	// The type-checking results; the package itself, and the Info struct.
	pkg  *types.Package
	info *types.Info

	// linkerVariableStrings is also initialized by prefillObjectMaps.
	// It records objects for variables used in -ldflags=-X flags,
	// as well as the strings the user wants to inject them with.
	linkerVariableStrings map[*types.Var]string

	// recordTypeDone helps avoid type cycles in recordType.
	// We only need to track named types, as all cycles must use them.
	recordTypeDone map[*types.Named]bool

	// fieldToStruct helps locate struct types from any of their field
	// objects. Useful when obfuscating field names.
	fieldToStruct map[*types.Var]*types.Struct
}

// newTransformer helps initialize some maps.
func newTransformer() *transformer {
	return &transformer{
		info: &types.Info{
			Types:     make(map[ast.Expr]types.TypeAndValue),
			Defs:      make(map[*ast.Ident]types.Object),
			Uses:      make(map[*ast.Ident]types.Object),
			Implicits: make(map[ast.Node]types.Object),
		},
		recordTypeDone: make(map[*types.Named]bool),
		fieldToStruct:  make(map[*types.Var]*types.Struct),
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
			tf.recordType(obj.Type(), nil)
		}
	}
	for name, obj := range tf.info.Uses {
		if obj == nil {
			continue
		}
		tf.recordType(obj.Type(), nil)

		// Record into KnownEmbeddedAliasFields.
		obj, ok := obj.(*types.TypeName)
		if !ok || !obj.IsAlias() {
			continue
		}
		vr, _ := tf.info.Defs[name].(*types.Var)
		if vr == nil || !vr.Embedded() {
			continue
		}
		vrStr := recordedObjectString(vr)
		if vrStr == "" {
			continue
		}
		aliasTypeName := typeName{
			PkgPath: obj.Pkg().Path(),
			Name:    obj.Name(),
		}
		cachedOutput.KnownEmbeddedAliasFields[vrStr] = aliasTypeName
	}
	for _, tv := range tf.info.Types {
		tf.recordType(tv.Type, nil)
	}
	return nil
}

// recordType visits every reachable type after typechecking a package.
// Right now, all it does is fill the fieldToStruct field.
// Since types can be recursive, we need a map to avoid cycles.
func (tf *transformer) recordType(used, origin types.Type) {
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
			tf.recordType(used.Elem(), origin.Elem())
		}
	case *types.Named:
		if tf.recordTypeDone[used] {
			return
		}
		tf.recordTypeDone[used] = true
		// If we have a generic struct like
		//
		//	type Foo[T any] struct { Bar T }
		//
		// then we want the hashing to use the original "Bar T",
		// because otherwise different instances like "Bar int" and "Bar bool"
		// will result in different hashes and the field names will break.
		// Ensure we record the original generic struct, if there is one.
		tf.recordType(used.Underlying(), used.Origin().Underlying())
	case *types.Struct:
		origin := origin.(*types.Struct)
		for i := 0; i < used.NumFields(); i++ {
			field := used.Field(i)
			tf.fieldToStruct[field] = origin

			if field.Embedded() {
				tf.recordType(field.Type(), origin.Field(i).Type())
			}
		}
	}
}

// TODO: consider caching recordedObjectString via a map,
// if that shows an improvement in our benchmark

func recordedObjectString(obj types.Object) objectString {
	pkg := obj.Pkg()
	if obj, ok := obj.(*types.Var); ok && obj.IsField() {
		// For exported fields, "pkgpath.Field" is not unique,
		// because two exported top-level types could share "Field".
		//
		// Moreover, note that not all fields belong to named struct types;
		// an API could be exposing:
		//
		//   var usedInReflection = struct{Field string}
		//
		// For now, a hack: assume that packages don't declare the same field
		// more than once in the same line. This works in practice, but one
		// could craft Go code to break this assumption.
		// Also note that the compiler's object files include filenames and line
		// numbers, but not column numbers nor byte offsets.
		// TODO(mvdan): give this another think, and add tests involving anon types.
		pos := fset.Position(obj.Pos())
		return fmt.Sprintf("%s.%s - %s:%d", pkg.Path(), obj.Name(),
			filepath.Base(pos.Filename), pos.Line)
	}
	// Names which are not at the top level cannot be imported,
	// so we don't need to record them either.
	// Note that this doesn't apply to fields, which are never top-level.
	if pkg.Scope() != obj.Parent() {
		return ""
	}
	// For top-level exported names, "pkgpath.Name" is unique.
	return pkg.Path() + "." + obj.Name()
}

// recordAsNotObfuscated records all the objects whose names we cannot obfuscate.
// An object is any named entity, such as a declared variable or type.
//
// As of June 2022, this only records types which are used in reflection.
// TODO(mvdan): If this is still the case in a year's time,
// we should probably rename "not obfuscated" and "cannot obfuscate" to be
// directly about reflection, e.g. "used in reflection".
func recordAsNotObfuscated(obj types.Object) {
	if obj.Pkg().Path() != curPkg.ImportPath {
		panic("called recordedAsNotObfuscated with a foreign object")
	}
	if !obj.Exported() {
		// Unexported names will never be used by other packages,
		// so we don't need to bother recording them in cachedOutput.
		knownCannotObfuscateUnexported[obj] = true
		return
	}

	objStr := recordedObjectString(obj)
	if objStr == "" {
		// If the object can't be described via a qualified string,
		// then other packages can't use it.
		// TODO: should we still record it in knownCannotObfuscateUnexported?
		return
	}
	cachedOutput.KnownCannotObfuscate[objStr] = struct{}{}
}

func recordedAsNotObfuscated(obj types.Object) bool {
	if knownCannotObfuscateUnexported[obj] {
		return true
	}
	objStr := recordedObjectString(obj)
	if objStr == "" {
		return false
	}
	_, ok := cachedOutput.KnownCannotObfuscate[objStr]
	return ok
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
	if flagLiterals && curPkg.ToObfuscate {
		file = literals.Obfuscate(obfRand, file, tf.info, tf.linkerVariableStrings)

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
			aliasTypeName, ok := cachedOutput.KnownEmbeddedAliasFields[vrStr]
			if ok {
				pkg2 := tf.pkg
				if path := aliasTypeName.PkgPath; pkg2.Path() != path {
					// If the package is a dependency, import it.
					// We can't grab the package via tf.pkg.Imports,
					// because some of the packages under there are incomplete.
					// ImportFrom will cache complete imports, anyway.
					var err error
					pkg2, err = origImporter.ImportFrom(path, parentWorkDir, 0)
					if err != nil {
						panic(err)
					}
				}
				tname, ok := pkg2.Scope().Lookup(aliasTypeName.Name).(*types.TypeName)
				if !ok {
					panic(fmt.Sprintf("KnownEmbeddedAliasFields pointed %q to a missing type %q", vrStr, aliasTypeName))
				}
				if !tname.IsAlias() {
					panic(fmt.Sprintf("KnownEmbeddedAliasFields pointed %q to a non-alias type %q", vrStr, aliasTypeName))
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
		path := pkg.Path()
		switch path {
		case "embed":
			// FS is detected by the compiler for //go:embed.
			// TODO: We probably want a conditional, otherwise we're not
			// obfuscating the embed package at all.
			return name == "FS"
		case "reflect":
			switch name {
			// Per the linker's deadcode.go docs,
			// the Method and MethodByName methods are what drive the logic.
			case "Method", "MethodByName":
				return true
			// Some packages reach into reflect internals, like go-spew.
			// It's not particularly right of them to do that,
			// and it's entirely unsupported, but try to accomodate for now.
			// At least it's enough to leave the rtype and Value types intact.
			case "rtype", "Value":
				tf.recursivelyRecordAsNotObfuscated(obj.Type())
				return true
			}
		}

		// The package that declared this object did not obfuscate it.
		if recordedAsNotObfuscated(obj) {
			return true
		}

		lpkg, err := listPackage(path)
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
				panic("could not find for " + name)
			}
			node.Name = hashWithStruct(strct, name)
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
		lpkg, err := listPackage(path)
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

// recursivelyRecordAsNotObfuscated calls recordAsNotObfuscated on any named
// types and fields under typ.
//
// Only the names declared in the current package are recorded. This is to ensure
// that reflection detection only happens within the package declaring a type.
// Detecting it in downstream packages could result in inconsistencies.
func (tf *transformer) recursivelyRecordAsNotObfuscated(t types.Type) {
	switch t := t.(type) {
	case *types.Named:
		obj := t.Obj()
		if pkg := obj.Pkg(); pkg == nil || pkg != tf.pkg {
			return // not from the specified package
		}
		if recordedAsNotObfuscated(obj) {
			return // prevent endless recursion
		}
		recordAsNotObfuscated(obj)

		// Record the underlying type, too.
		tf.recursivelyRecordAsNotObfuscated(t.Underlying())

	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			field := t.Field(i)

			// This check is similar to the one in *types.Named.
			// It's necessary for unnamed struct types,
			// as they aren't named but still have named fields.
			if field.Pkg() == nil || field.Pkg() != tf.pkg {
				return // not from the specified package
			}

			// Record the field itself, too.
			recordAsNotObfuscated(field)

			tf.recursivelyRecordAsNotObfuscated(field.Type())
		}

	case interface{ Elem() types.Type }:
		// Get past pointers, slices, etc.
		tf.recursivelyRecordAsNotObfuscated(t.Elem())
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
		lpkg := curPkg
		if path != "main" {
			lpkg = cache.ListedPackages[path]
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

// forwardBuildFlags is obtained from 'go help build' as of Go 1.20.
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

// booleanFlags is obtained from 'go help build' and 'go help testflag' as of Go 1.20.
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
		"GOOS", "GOMOD", "GOVERSION", "GOROOT", "GOEXE",
	).CombinedOutput()
	if err != nil {
		// TODO: cover this in the tests.
		fmt.Fprintf(os.Stderr, `Can't find the Go toolchain: %v

This is likely due to Go not being installed/setup correctly.

To install Go, see: https://go.dev/doc/install
`, err)
		return errJustExit(1)
	}
	if err := json.Unmarshal(out, &cache.GoEnv); err != nil {
		return fmt.Errorf(`cannot unmarshal from "go env -json": %w`, err)
	}
	cache.GOGARBLE = os.Getenv("GOGARBLE")
	if cache.GOGARBLE == "" {
		cache.GOGARBLE = "*" // we default to obfuscating everything
	}
	return nil
}
