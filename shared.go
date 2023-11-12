// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

//go:generate ./scripts/gen-go-std-tables.sh

// sharedCacheType is shared as a read-only cache between the many garble toolexec
// sub-processes.
//
// Note that we fill this cache once from the root process in saveListedPackages,
// store it into a temporary file via gob encoding, and then reuse that file
// in each of the garble toolexec sub-processes.
type sharedCacheType struct {
	ExecPath          string   // absolute path to the garble binary being used
	ForwardBuildFlags []string // build flags fed to the original "garble ..." command

	CacheDir string // absolute path to the GARBLE_CACHE directory being used

	// ListedPackages contains data obtained via 'go list -json -export -deps'.
	// This allows us to obtain the non-obfuscated export data of all dependencies,
	// useful for type checking of the packages as we obfuscate them.
	ListedPackages map[string]*listedPackage

	// We can't use garble's own module version, as it may not exist.
	// We can't use the stamped VCS information either,
	// as uncommitted changes simply show up as "dirty".
	//
	// The only unique way to identify garble's version without being published
	// or committed is to use its content ID from the build cache.
	BinaryContentID []byte

	GOGARBLE string

	// GoVersionSemver is a semver-compatible version of the Go toolchain
	// currently being used, as reported by "go env GOVERSION".
	// Note that the version of Go that built the garble binary might be newer.
	// Also note that a devel version like "go1.22-231f290e51" is
	// currently represented as "v1.22".
	GoVersionSemver string

	// Filled directly from "go env".
	// Keep in sync with fetchGoEnv.
	GoEnv struct {
		GOOS string // i.e. the GOOS build target

		GOMOD     string
		GOVERSION string
		GOROOT    string
	}
}

var sharedCache *sharedCacheType

// loadSharedCache the shared data passed from the entry garble process
func loadSharedCache() error {
	if sharedCache != nil {
		panic("shared cache loaded twice?")
	}
	startTime := time.Now()
	f, err := os.Open(filepath.Join(sharedTempDir, "main-cache.gob"))
	if err != nil {
		return fmt.Errorf(`cannot open shared file: %v\ndid you run "go [command] -toolexec=garble" instead of "garble [command]"?`, err)
	}
	defer func() {
		log.Printf("shared cache loaded in %s from %s", debugSince(startTime), f.Name())
	}()
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(&sharedCache); err != nil {
		return fmt.Errorf("cannot decode shared file: %v", err)
	}
	return nil
}

// saveSharedCache creates a temporary directory to share between garble processes.
// This directory also includes the gob-encoded cache global.
func saveSharedCache() (string, error) {
	if sharedCache == nil {
		panic("saving a missing cache?")
	}
	dir, err := os.MkdirTemp("", "garble-shared")
	if err != nil {
		return "", err
	}

	cachePath := filepath.Join(dir, "main-cache.gob")
	if err := writeGobExclusive(cachePath, &sharedCache); err != nil {
		return "", err
	}
	return dir, nil
}

func createExclusive(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
}

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

func writeGobExclusive(name string, val any) error {
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

	Dir             string
	CompiledGoFiles []string
	IgnoredGoFiles  []string
	Imports         []string

	Error *packageError // to report package loading errors to the user

	// The fields below are not part of 'go list', but are still reused
	// between garble processes. Use "Garble" as a prefix to ensure no
	// collisions with the JSON fields from 'go list'.

	// GarbleActionID is a hash combining the Action ID from BuildID,
	// with Garble's own inputs as per addGarbleToHash.
	// It is set even when ToObfuscate is false, as it is also used for random
	// seeds and build cache paths, and not just to obfuscate names.
	GarbleActionID [sha256.Size]byte `json:"-"`

	// ToObfuscate records whether the package should be obfuscated.
	// When true, GarbleActionID must not be empty.
	ToObfuscate bool `json:"-"`
}

type packageError struct {
	Pos string
	Err string
}

func (p *listedPackage) obfuscatedImportPath() string {
	// We can't obfuscate these standard library import paths,
	// as the toolchain expects to recognize the packages by them:
	//
	//   * runtime: it is special in many ways
	//   * reflect: its presence turns down dead code elimination
	//   * embed: its presence enables using //go:embed
	switch p.ImportPath {
	case "runtime", "reflect", "embed":
		return p.ImportPath
	}
	if compilerIntrinsicsPkgs[p.ImportPath] {
		return p.ImportPath
	}
	if !p.ToObfuscate {
		return p.ImportPath
	}
	newPath := hashWithPackage(p, p.ImportPath)
	log.Printf("import path %q hashed with %x to %q", p.ImportPath, p.GarbleActionID, newPath)
	return newPath
}

// garbleBuildFlags are always passed to top-level build commands such as
// "go build", "go list", or "go test".
var garbleBuildFlags = []string{"-trimpath", "-buildvcs=false"}

// appendListedPackages gets information about the current package
// and all of its dependencies
func appendListedPackages(packages []string, mainBuild bool) error {
	startTime := time.Now()
	args := []string{
		"list",
		// Similar flags to what go/packages uses.
		"-json", "-export", "-compiled", "-e",
	}
	if mainBuild {
		// When loading the top-level packages we are building,
		// we want to transitively load all their dependencies as well.
		// That is not the case when loading standard library packages,
		// as runtimeLinknamed already contains transitive dependencies.
		args = append(args, "-deps")
	}
	args = append(args, garbleBuildFlags...)
	args = append(args, sharedCache.ForwardBuildFlags...)

	if !mainBuild {
		// If the top-level build included the -mod or -modfile flags,
		// they should be used when loading the top-level packages.
		// However, when loading standard library packages,
		// using those flags would likely result in an error,
		// as the standard library uses its own Go module and vendoring.
		args = append(args, "-mod=", "-modfile=")
	}

	args = append(args, packages...)
	cmd := exec.Command("go", args...)

	defer func() {
		log.Printf("original build info obtained in %s via: go %s", debugSince(startTime), strings.Join(args, " "))
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
	if sharedCache.ListedPackages == nil {
		sharedCache.ListedPackages = make(map[string]*listedPackage)
	}
	var pkgErrors strings.Builder
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return err
		}

		if perr := pkg.Error; perr != nil {
			if pkg.Standard && len(pkg.CompiledGoFiles) == 0 && len(pkg.IgnoredGoFiles) > 0 {
				// Some packages in runtimeLinknamed need a build tag to be importable,
				// like crypto/internal/boring/fipstls with boringcrypto,
				// so any pkg.Error should be ignored when the build tag isn't set.
			} else if pkg.ImportPath == "math/rand/v2" && semver.Compare(sharedCache.GoVersionSemver, "v1.22") < 0 {
				// added in Go 1.22, so Go 1.21 runs into a "not found" error.
			} else {
				if pkgErrors.Len() > 0 {
					pkgErrors.WriteString("\n")
				}
				if perr.Pos != "" {
					pkgErrors.WriteString(perr.Pos)
					pkgErrors.WriteString(": ")
				}
				// Error messages sometimes include a trailing newline.
				pkgErrors.WriteString(strings.TrimRight(perr.Err, "\n"))
			}
		}

		// Note that we use the `-e` flag above with `go list`.
		// If a package fails to load, the Incomplete and Error fields will be set.
		// We still record failed packages in the ListedPackages map,
		// because some like crypto/internal/boring/fipstls simply fall under
		// "build constraints exclude all Go files" and can be ignored.
		// Real build errors will still be surfaced by `go build -toolexec` later.
		if sharedCache.ListedPackages[pkg.ImportPath] != nil {
			return fmt.Errorf("duplicate package: %q", pkg.ImportPath)
		}
		if pkg.BuildID != "" {
			actionID := decodeBuildIDHash(splitActionID(pkg.BuildID))
			pkg.GarbleActionID = addGarbleToHash(actionID)
		}

		sharedCache.ListedPackages[pkg.ImportPath] = &pkg
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go list error: %v:\nargs: %q\n%s", err, args, stderr.Bytes())
	}
	if pkgErrors.Len() > 0 {
		return errors.New(pkgErrors.String())
	}

	anyToObfuscate := false
	for path, pkg := range sharedCache.ListedPackages {
		// If "GOGARBLE=foo/bar", "foo/bar_test" should also match.
		if pkg.ForTest != "" {
			path = pkg.ForTest
		}
		switch {
		// We do not support obfuscating the runtime nor its dependencies.
		case runtimeAndDeps[path],
			// "unknown pc" crashes on windows in the cgo test otherwise.
			path == "runtime/cgo":

		// No point in obfuscating empty packages, like OS-specific ones that don't match.
		case len(pkg.CompiledGoFiles) == 0:

		// Test main packages like "foo/bar.test" are always obfuscated,
		// just like unnamed and plugin main packages.
		case pkg.Name == "main" && strings.HasSuffix(path, ".test"),
			path == "command-line-arguments",
			strings.HasPrefix(path, "plugin/unnamed"),
			module.MatchPrefixPatterns(sharedCache.GOGARBLE, path):

			pkg.ToObfuscate = true
			anyToObfuscate = true
			if len(pkg.GarbleActionID) == 0 {
				return fmt.Errorf("package %q to be obfuscated lacks build id?", pkg.ImportPath)
			}
		}
	}

	// Don't error if the user ran: GOGARBLE='*' garble build runtime
	if !anyToObfuscate && !module.MatchPrefixPatterns(sharedCache.GOGARBLE, "runtime") {
		return fmt.Errorf("GOGARBLE=%q does not match any packages to be built", sharedCache.GOGARBLE)
	}

	return nil
}

var listedRuntimeLinknamed = false

var ErrNotFound = errors.New("not found")

var ErrNotDependency = errors.New("not a dependency")

// listPackage gets the listedPackage information for a certain package
func listPackage(from *listedPackage, path string) (*listedPackage, error) {
	if path == from.ImportPath {
		return from, nil
	}

	// If the path is listed in the top-level ImportMap, use its mapping instead.
	// This is a common scenario when dealing with vendored packages in GOROOT.
	// The map is flat, so we don't need to recurse.
	if path2 := from.ImportMap[path]; path2 != "" {
		path = path2
	}

	pkg, ok := sharedCache.ListedPackages[path]

	// A std package may list any other package in std, even those it doesn't depend on.
	// This is due to how runtime linkname-implements std packages,
	// such as sync/atomic or reflect, without importing them in any way.
	// A few other cases don't involve runtime, like time/tzdata linknaming to time,
	// but luckily those few cases are covered by runtimeLinknamed as well.
	//
	// If ListedPackages lacks such a package we fill it via runtimeLinknamed.
	// TODO: can we instead add runtimeLinknamed to the top-level "go list" args?
	if from.Standard {
		if ok {
			return pkg, nil
		}
		if listedRuntimeLinknamed {
			panic(fmt.Sprintf("package %q still missing after go list call", path))
		}
		startTime := time.Now()
		missing := make([]string, 0, len(runtimeLinknamed))
		for _, linknamed := range runtimeLinknamed {
			switch {
			case sharedCache.ListedPackages[linknamed] != nil:
				// We already have it; skip.
			case sharedCache.GoEnv.GOOS != "js" && linknamed == "syscall/js":
				// GOOS-specific package.
			case sharedCache.GoEnv.GOOS != "darwin" && linknamed == "crypto/x509/internal/macos":
				// GOOS-specific package.
			default:
				missing = append(missing, linknamed)
			}
		}
		// We don't need any information about their dependencies, in this case.
		if err := appendListedPackages(missing, false); err != nil {
			panic(err) // should never happen
		}
		pkg, ok := sharedCache.ListedPackages[path]
		if !ok {
			panic(fmt.Sprintf("std listed another std package that we can't find: %s", path))
		}
		listedRuntimeLinknamed = true
		log.Printf("listed %d missing runtime-linknamed packages in %s", len(missing), debugSince(startTime))
		return pkg, nil
	}
	if !ok {
		return nil, fmt.Errorf("list %s: %w", path, ErrNotFound)
	}

	// Packages outside std can list any package,
	// as long as they depend on it directly or indirectly.
	for _, dep := range from.Deps {
		if dep == pkg.ImportPath {
			return pkg, nil
		}
	}

	// As a special case, any package can list runtime or its dependencies,
	// since those are always an implicit dependency.
	// We need to handle this ourselves as runtime does not appear in Deps.
	// TODO: it might be faster to bring back a "runtimeAndDeps" map or func.
	if pkg.ImportPath == "runtime" {
		return pkg, nil
	}
	for _, dep := range sharedCache.ListedPackages["runtime"].Deps {
		if dep == pkg.ImportPath {
			return pkg, nil
		}
	}

	return nil, fmt.Errorf("list %s: %w", path, ErrNotDependency)
}
