// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/module"
)

// sharedCache is shared as a read-only cache between the many garble toolexec
// sub-processes.
//
// Note that we fill this cache once from the root process in saveListedPackages,
// store it into a temporary file via gob encoding, and then reuse that file
// in each of the garble toolexec sub-processes.
type sharedCache struct {
	ExecPath          string   // absolute path to the garble binary being used
	ForwardBuildFlags []string // build flags fed to the original "garble ..." command

	// ListedPackages contains data obtained via 'go list -json -export -deps'.
	// This allows us to obtain the non-obfuscated export data of all dependencies,
	// useful for type checking of the packages as we obfuscate them.
	ListedPackages map[string]*listedPackage

	// We can't rely on the module version to exist,
	// because it's missing in local builds without 'go install'.
	// For now, use 'go tool buildid' on the garble binary.
	// Just like Go's own cache, we use hex-encoded sha256 sums.
	// Once https://github.com/golang/go/issues/37475 is fixed,
	// we can likely just use that.
	BinaryContentID []byte

	GOGARBLE string

	// Filled directly from "go env".
	// Remember to update the exec call when adding or removing names.
	GoEnv struct {
		GOOS string // i.e. the GOOS build target

		GOPRIVATE string
		GOMOD     string
		GOVERSION string
		GOCACHE   string
	}
}

var cache *sharedCache

// loadSharedCache the shared data passed from the entry garble process
func loadSharedCache() error {
	if cache != nil {
		panic("shared cache loaded twice?")
	}
	startTime := time.Now()
	f, err := os.Open(filepath.Join(sharedTempDir, "main-cache.gob"))
	if err != nil {
		return fmt.Errorf(`cannot open shared file, this is most likely due to not running "garble [command]"`)
	}
	defer func() {
		debugf("shared cache loaded in %s from %s", debugSince(startTime), f.Name())
	}()
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(&cache); err != nil {
		return err
	}
	return nil
}

// saveSharedCache creates a temporary directory to share between garble processes.
// This directory also includes the gob-encoded cache global.
func saveSharedCache() (string, error) {
	if cache == nil {
		panic("saving a missing cache?")
	}
	dir, err := os.MkdirTemp("", "garble-shared")
	if err != nil {
		return "", err
	}

	sharedCache := filepath.Join(dir, "main-cache.gob")
	if err := writeGobExclusive(sharedCache, &cache); err != nil {
		return "", err
	}
	return dir, nil
}

func createExclusive(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
}

// TODO(mvdan): consider using proper atomic file writes.
// Or possibly even "lockedfile", mimicking cmd/go.

func writeFileExclusive(name string, data []byte) error {
	f, err := createExclusive(name)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err2 := f.Close(); err == nil {
		err = err2
	}
	return err
}

func writeGobExclusive(name string, val interface{}) error {
	f, err := createExclusive(name)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(val); err != nil {
		return err
	}
	if err2 := f.Close(); err == nil {
		err = err2
	}
	return err
}

// listedPackage contains the 'go list -json -export' fields obtained by the
// root process, shared with all garble sub-processes via a file.
type listedPackage struct {
	Name       string
	ImportPath string
	ForTest    string
	Export     string
	BuildID    string
	Deps       []string
	ImportMap  map[string]string
	Standard   bool

	Dir     string
	GoFiles []string
	Imports []string

	// The fields below are not part of 'go list', but are still reused
	// between garble processes. Use "Garble" as a prefix to ensure no
	// collisions with the JSON fields from 'go list'.

	// TODO(mvdan): consider filling this iff ToObfuscate==true,
	// which will help ensure we don't obfuscate any of their names otherwise.
	GarbleActionID []byte

	// ToObfuscate records whether the package should be obfuscated.
	ToObfuscate bool
}

func (p *listedPackage) obfuscatedImportPath() string {
	if p.Name == "main" {
		panic("main packages should never need to obfuscate their import paths")
	}
	// We can't obfuscate the embed package's import path,
	// as the toolchain expects to recognize the package by it.
	if p.ImportPath == "embed" || !p.ToObfuscate {
		return p.ImportPath
	}
	newPath := hashWithPackage(p, p.ImportPath)
	debugf("import path %q hashed with %x to %q", p.ImportPath, p.GarbleActionID, newPath)
	return newPath
}

// appendListedPackages gets information about the current package
// and all of its dependencies
func appendListedPackages(packages []string, withDeps bool) error {
	startTime := time.Now()
	// TODO: perhaps include all top-level build flags set by garble,
	// including -buildvcs=false.
	// They shouldn't affect "go list" here, but might as well be consistent.
	args := []string{"list", "-json", "-export", "-trimpath"}
	if withDeps {
		args = append(args, "-deps")
	}
	args = append(args, cache.ForwardBuildFlags...)
	args = append(args, packages...)
	cmd := exec.Command("go", args...)

	defer func() {
		debugf("original build info obtained in %s via: go %s", debugSince(startTime), strings.Join(args, " "))
	}()

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
	if cache.ListedPackages == nil {
		cache.ListedPackages = make(map[string]*listedPackage)
	}
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return err
		}
		if cache.ListedPackages[pkg.ImportPath] != nil {
			return fmt.Errorf("duplicate package: %q", pkg.ImportPath)
		}
		if pkg.BuildID != "" {
			actionID := decodeHash(splitActionID(pkg.BuildID))
			pkg.GarbleActionID = addGarbleToHash(actionID)
		}
		cache.ListedPackages[pkg.ImportPath] = &pkg
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go list error: %v: %s", err, stderr.Bytes())
	}

	anyToObfuscate := false
	for path, pkg := range cache.ListedPackages {
		// If "GOGARBLE=foo/bar", "foo/bar_test" should also match.
		if pkg.ForTest != "" {
			path = pkg.ForTest
		}
		// Test main packages like "foo/bar.test" are always obfuscated,
		// just like main packages.
		switch {
		case cannotObfuscate[path], runtimeAndDeps[path]:
			// We don't support obfuscating these yet.

		case pkg.Name == "main" && strings.HasSuffix(path, ".test"),
			path == "command-line-arguments",
			strings.HasPrefix(path, "plugin/unnamed"),
			module.MatchPrefixPatterns(cache.GOGARBLE, path):

			pkg.ToObfuscate = true
			anyToObfuscate = true
		}
	}

	// Don't error if the user ran: GOGARBLE='*' garble build runtime
	if !anyToObfuscate && !module.MatchPrefixPatterns(cache.GOGARBLE, "runtime") {
		return fmt.Errorf("GOGARBLE=%q does not match any packages to be built", cache.GOGARBLE)
	}

	return nil
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

// Obtained from "go list -deps runtime" on Go 1.18beta1.
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

var listedRuntimeLinknamed = false

// listPackage gets the listedPackage information for a certain package
func listPackage(path string) (*listedPackage, error) {
	if path == curPkg.ImportPath {
		return curPkg, nil
	}

	// If the path is listed in the top-level ImportMap, use its mapping instead.
	// This is a common scenario when dealing with vendored packages in GOROOT.
	// The map is flat, so we don't need to recurse.
	if path2 := curPkg.ImportMap[path]; path2 != "" {
		path = path2
	}

	pkg, ok := cache.ListedPackages[path]

	// The runtime may list any package in std, even those it doesn't depend on.
	// This is due to how it linkname-implements std packages,
	// such as sync/atomic or reflect, without importing them in any way.
	// If ListedPackages lacks such a package we fill it with "std".
	if curPkg.ImportPath == "runtime" {
		if ok {
			return pkg, nil
		}
		if listedRuntimeLinknamed {
			panic(fmt.Sprintf("package %q still missing after go list call", path))
		}
		startTime := time.Now()
		// Obtained via scripts/runtime-linknamed-nodeps.sh as of Go 1.18beta1.
		runtimeLinknamed := []string{
			"crypto/x509/internal/macos",
			"internal/poll",
			"internal/reflectlite",
			"net",
			"os",
			"os/signal",
			"plugin",
			"reflect",
			"runtime/debug",
			"runtime/metrics",
			"runtime/pprof",
			"runtime/trace",
			"sync",
			"sync/atomic",
			"syscall",
			"syscall/js",
			"time",
		}
		missing := make([]string, 0, len(runtimeLinknamed))
		for _, linknamed := range runtimeLinknamed {
			switch {
			case cache.ListedPackages[linknamed] != nil:
				// We already have it; skip.
			case cache.GoEnv.GOOS != "js" && linknamed == "syscall/js":
				// GOOS-specific package.
			case cache.GoEnv.GOOS != "darwin" && linknamed == "crypto/x509/internal/macos":
				// GOOS-specific package.
			default:
				missing = append(missing, linknamed)
			}
		}
		// We don't need any information about their dependencies, in this case.
		if err := appendListedPackages(missing, false); err != nil {
			panic(err) // should never happen
		}
		pkg, ok := cache.ListedPackages[path]
		if !ok {
			panic(fmt.Sprintf("runtime listed a std package we can't find: %s", path))
		}
		listedRuntimeLinknamed = true
		debugf("listed %d missing runtime-linknamed packages in %s", len(missing), debugSince(startTime))
		return pkg, nil
	}
	// Packages other than runtime can list any package,
	// as long as they depend on it directly or indirectly.
	if !ok {
		return nil, fmt.Errorf("path not found in listed packages: %s", path)
	}
	for _, dep := range curPkg.Deps {
		if dep == pkg.ImportPath {
			return pkg, nil
		}
	}
	return nil, fmt.Errorf("refusing to list non-dependency package: %s", path)
}
