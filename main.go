// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

// garble obfuscates Go code by wrapping the Go toolchain.
package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"go/token"
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
