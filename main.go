// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

// garble obfuscates Go code by wrapping the Go toolchain.
package main

import (
	"bytes"
	"cmp"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/token"
	"go/version"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/garble/internal/linker"
)

const actionGraphFileName = "action-graph.json"

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

var flagSet = flag.NewFlagSet("garble", flag.ExitOnError)
var rxGarbleFlag = regexp.MustCompile(`-(?:literals|tiny|debug|debugdir|seed)(?:$|=)`)

var (
	flagLiterals bool
	flagTiny     bool
	flagDebug    bool
	flagDebugDir string
	flagSeed     seedFlag
	// TODO(pagran): in the future, when control flow obfuscation will be stable migrate to flag
	flagControlFlow = os.Getenv("GARBLE_EXPERIMENTAL_CONTROLFLOW") == "1"

	// Presumably OK to share fset across packages.
	fset = token.NewFileSet()

	sharedTempDir = os.Getenv("GARBLE_SHARED")
)

func init() {
	flagSet.Usage = usage
	flagSet.BoolVar(&flagLiterals, "literals", false, "Obfuscate literals such as strings")
	flagSet.BoolVar(&flagTiny, "tiny", false, "Optimize for binary size, losing some ability to reverse the process")
	flagSet.BoolVar(&flagDebug, "debug", false, "Print debug logs to stderr")
	flagSet.StringVar(&flagDebugDir, "debugdir", "", "Write the obfuscated source to a directory, e.g. -debugdir=out")
	flagSet.Var(&flagSeed, "seed", "Provide a base64-encoded seed, e.g. -seed=o9WDTZ4CN4w\nFor a random seed, provide -seed=random")
}

func main() {
	if dir := os.Getenv("GARBLE_WRITE_CPUPROFILES"); dir != "" {
		f, err := os.CreateTemp(dir, "garble-cpu-*.pprof")
		if err != nil {
			panic(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			panic(err)
		}
		defer func() {
			pprof.StopCPUProfile()
			if err := f.Close(); err != nil {
				panic(err)
			}
		}()
	}
	defer func() {
		if dir := os.Getenv("GARBLE_WRITE_MEMPROFILES"); dir != "" {
			f, err := os.CreateTemp(dir, "garble-mem-*.pprof")
			if err != nil {
				panic(err)
			}
			runtime.GC() // get up-to-date statistics
			if err := pprof.WriteHeapProfile(f); err != nil {
				panic(err)
			}
			if err := f.Close(); err != nil {
				panic(err)
			}
		}
		if os.Getenv("GARBLE_WRITE_ALLOCS") == "true" {
			var memStats runtime.MemStats
			runtime.ReadMemStats(&memStats)
			fmt.Fprintf(os.Stderr, "garble allocs: %d\n", memStats.Mallocs)
		}
	}()
	flagSet.Parse(os.Args[1:])
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
		os.Exit(2)
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
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type errJustExit int

func (e errJustExit) Error() string { return fmt.Sprintf("exit: %d", e) }

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
			return errJustExit(0)
		}
		if len(args) == 1 {
			return mainErr([]string{args[0], "-h"})
		}
		usage()
		return errJustExit(0)
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
				return fmt.Errorf("TOOLEXEC_IMPORTPATH package not found in listed packages: %s", toolexecImportPath)
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

	execPath, err := os.Executable()
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

	binaryBuildID, err := buildidOf(execPath)
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

	if flagDebugDir != "" {
		origDir := flagDebugDir
		flagDebugDir, err = filepath.Abs(flagDebugDir)
		if err != nil {
			return nil, err
		}
		sentinel := filepath.Join(flagDebugDir, ".garble-debugdir")
		if entries, err := os.ReadDir(flagDebugDir); errors.Is(err, fs.ErrNotExist) {
		} else if err == nil && len(entries) == 0 {
			// It's OK to delete an existing directory as long as it's empty.
		} else if _, err := os.Lstat(sentinel); err == nil {
			// It's OK to delete a non-empty directory which was created by an earlier
			// invocation of `garble -debugdir`, which we know by leaving a sentinel file.
			if err := os.RemoveAll(flagDebugDir); err != nil {
				return nil, fmt.Errorf("could not empty debugdir: %v", err)
			}
		} else {
			return nil, fmt.Errorf("debugdir %q has unknown contents; empty it first", origDir)
		}

		if err := os.MkdirAll(flagDebugDir, 0o755); err != nil {
			return nil, fmt.Errorf("could not create debugdir directory: %v", err)
		}
		if err := os.WriteFile(sentinel, nil, 0o666); err != nil {
			return nil, fmt.Errorf("could not create debugdir sentinel: %v", err)
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
	quotedExecPath, err := cmdgoQuotedJoin([]string{execPath})
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

func goVersionOK() bool {
	const (
		minGoVersion  = "go1.25.0" // the minimum Go version we support; could be a bugfix release if needed
		unsupportedGo = "go1.26"   // the first major version we don't support
	)

	toolchainVersion := sharedCache.GoEnv.GOVERSION
	if toolchainVersion == "" {
		// Go 1.15.x and older did not have GOVERSION yet; they are too old anyway.
		fmt.Fprintf(os.Stderr, "Go version is too old; please upgrade to %s or newer\n", minGoVersion)
		return false
	}

	if !version.IsValid(toolchainVersion) {
		fmt.Fprintf(os.Stderr, "Go version %q appears to be invalid or too old; use %s or newer\n", toolchainVersion, minGoVersion)
		return false
	}
	if version.Compare(toolchainVersion, minGoVersion) < 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too old; please upgrade to %s or newer\n", toolchainVersion, minGoVersion)
		return false
	}
	if version.Compare(toolchainVersion, unsupportedGo) >= 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too new; Go linker patches aren't available for %s or later yet\n", toolchainVersion, unsupportedGo)
		return false
	}

	// Ensure that the version of Go that built the garble binary is equal or
	// newer than cache.GoVersionSemver.
	builtVersion := cmp.Or(os.Getenv("GARBLE_TEST_GOVERSION"), runtime.Version())
	if !version.IsValid(builtVersion) {
		// If garble built itself, we don't know what Go version was used.
		// Fall back to not performing the check against the toolchain version.
		return true
	}
	if version.Compare(builtVersion, toolchainVersion) < 0 {
		fmt.Fprintf(os.Stderr, `
garble was built with %q and can't be used with the newer %q; rebuild it with a command like:
    go install mvdan.cc/garble@latest
`[1:], builtVersion, toolchainVersion)
		return false
	}

	return true
}

func usage() {
	fmt.Fprint(os.Stderr, `
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
	fmt.Fprint(os.Stderr, `

For more information, see https://github.com/burrowers/garble.
`[1:])
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
		if val, ok := strings.CutPrefix(arg, name+"="); ok {
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
		// Keep in sync with [sharedCacheType.GoEnv].
		"GOOS", "GOARCH", "GOMOD", "GOVERSION", "GOROOT",
	).Output()
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
	// TODO: remove once https://github.com/golang/go/issues/75953 is fixed,
	// such that `go env GOVERSION` can always be parsed by go/version.
	sharedCache.GoEnv.GOVERSION, _, _ = strings.Cut(sharedCache.GoEnv.GOVERSION, " ")

	// Some Go version managers switch between Go versions via a GOROOT which symlinks
	// to one of the available versions. Given that later we build a patched linker
	// from GOROOT/src via `go build -overlay`, we need to resolve any symlinks.
	// Note that this edge case has no tests as it's relatively rare.
	sharedCache.GoEnv.GOROOT, err = filepath.EvalSymlinks(sharedCache.GoEnv.GOROOT)
	if err != nil {
		return err
	}

	sharedCache.GoCmd = filepath.Join(sharedCache.GoEnv.GOROOT, "bin", "go")
	sharedCache.GOGARBLE = cmp.Or(os.Getenv("GOGARBLE"), "*") // we default to obfuscating everything
	return nil
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
		return 0, fmt.Errorf("log write wasn't just one line: %q", p)
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

func hasHelpFlag(flags []string) bool {
	for _, f := range flags {
		switch f {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}
