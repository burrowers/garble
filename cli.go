package main

import (
	"bytes"
	"cmp"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/version"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

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

	// rxVersion looks for a version like "go1.2" or "go1.2.3" in `go env GOVERSION`.
	rxVersion := regexp.MustCompile(`go\d+\.\d+(?:\.\d+)?`)

	toolchainVersionFull := sharedCache.GoEnv.GOVERSION
	sharedCache.GoVersion = rxVersion.FindString(toolchainVersionFull)
	if sharedCache.GoVersion == "" {
		// Go 1.15.x and older did not have GOVERSION yet; they are too old anyway.
		fmt.Fprintf(os.Stderr, "Go version is too old; please upgrade to %s or newer\n", minGoVersion)
		return false
	}

	if version.Compare(sharedCache.GoVersion, minGoVersion) < 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too old; please upgrade to %s or newer\n", toolchainVersionFull, minGoVersion)
		return false
	}
	if version.Compare(sharedCache.GoVersion, unsupportedGo) >= 0 {
		fmt.Fprintf(os.Stderr, "Go version %q is too new; Go linker patches aren't available for %s or later yet\n", toolchainVersionFull, unsupportedGo)
		return false
	}

	// Ensure that the version of Go that built the garble binary is equal or
	// newer than cache.GoVersionSemver.
	builtVersionFull := cmp.Or(os.Getenv("GARBLE_TEST_GOVERSION"), runtime.Version())
	builtVersion := rxVersion.FindString(builtVersionFull)
	if builtVersion == "" {
		// If garble built itself, we don't know what Go version was used.
		// Fall back to not performing the check against the toolchain version.
		return true
	}
	if version.Compare(builtVersion, sharedCache.GoVersion) < 0 {
		fmt.Fprintf(os.Stderr, `
garble was built with %q and can't be used with the newer %q; rebuild it with a command like:
    go install mvdan.cc/garble@latest
`[1:], builtVersionFull, toolchainVersionFull)
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
