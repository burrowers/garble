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
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/module"
)

//go:generate go run scripts/gen_go_std_tables.go

// sharedCacheType is shared as a read-only cache between the many garble toolexec
// sub-processes.
//
// Note that we fill this cache once from the root process in saveListedPackages,
// store it into a temporary file via gob encoding, and then reuse that file
// in each of the garble toolexec sub-processes.
type sharedCacheType struct {
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

	// GoCmd is [GoEnv.GOROOT]/bin/go, so that we run exactly the same version
	// of the Go tool that the original "go build" invocation did.
	GoCmd string

	// Filled directly from "go env".
	// Keep in sync with fetchGoEnv.
	GoEnv struct {
		GOOS   string // the GOOS build target
		GOARCH string // the GOARCH build target

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
	// Always close the file, and return the first error we get.
	err = gob.NewEncoder(f).Encode(val)
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
	ImportMap  map[string]string
	Standard   bool

	Dir             string
	CompiledGoFiles []string // all .go files to build
	SFiles          []string // all .s (asm) files to build
	Imports         []string

	Error *packageError // to report package loading errors to the user

	// The fields below are not part of 'go list', but are still reused
	// between garble processes. Use "Garble" as a prefix to ensure no
	// collisions with the JSON fields from 'go list'.

	// allDeps is like the Deps field given by 'go list', but in the form of a map
	// for the sake of fast lookups. It's also unnecessary to consume or store Deps
	// as returned by 'go list', as it can be reconstructed from Imports.
	allDeps map[string]struct{}

	// GarbleActionID is a hash combining the Action ID from BuildID,
	// with Garble's own inputs as per addGarbleToHash.
	// It is set even when ToObfuscate is false, as it is also used for random
	// seeds and build cache paths, and not just to obfuscate names.
	GarbleActionID [sha256.Size]byte `json:"-"`

	// ToObfuscate records whether the package should be obfuscated.
	// When true, GarbleActionID must not be empty.
	ToObfuscate bool `json:"-"`
}

func (p *listedPackage) hasDep(path string) bool {
	if p.allDeps == nil {
		p.allDeps = make(map[string]struct{}, len(p.Imports)*2)
		p.addImportsFrom(p)
	}
	_, ok := p.allDeps[path]
	return ok
}

func (p *listedPackage) addImportsFrom(from *listedPackage) {
	for _, path := range from.Imports {
		if path == "C" {
			// `go list -json` shows "C" in Imports but not Deps.
			// See https://go.dev/issue/60453.
			continue
		}
		if path2 := from.ImportMap[path]; path2 != "" {
			path = path2
		}
		if _, ok := p.allDeps[path]; ok {
			continue // already added
		}
		p.allDeps[path] = struct{}{}
		p.addImportsFrom(sharedCache.ListedPackages[path])
	}
}

type packageError struct {
	Pos string
	Err string
}

// obfuscatedPackageName returns a package's obfuscated package name,
// which may be unchanged in some cases where we cannot obfuscate it.
// Note that package main is unchanged as it is treated in a special way by the toolchain.
func (p *listedPackage) obfuscatedPackageName() string {
	if p.Name == "main" || !p.ToObfuscate {
		return p.Name
	}
	// The package name itself is obfuscated like any other name.
	return hashWithPackage(p, p.Name)
}

// obfuscatedSourceDir returns an obfuscated directory name which can be used
// to write obfuscated source files to. This directory name should be unique per package,
// even when building many main packages at once, such as in `go test ./...`.
func (p *listedPackage) obfuscatedSourceDir() string {
	return hashWithPackage(p, p.ImportPath)
}

// obfuscatedImportPath returns a package's obfuscated import path,
// which may be unchanged in some cases where we cannot obfuscate it.
// Note that package main always has the unchanged import path "main" as part of a build,
// but not if it's a main package as part of a test, which can be imported.
func (p *listedPackage) obfuscatedImportPath() string {
	if p.Name == "main" && p.ForTest == "" {
		return "main"
	}
	if !p.ToObfuscate {
		return p.ImportPath
	}
	// We can't obfuscate these standard library import paths,
	// as the toolchain expects to recognize the packages by them:
	//
	//   * runtime: it is special in many ways
	//   * reflect: its presence turns down dead code elimination
	//   * embed: its presence enables using //go:embed
	//   * others like syscall are allowed by import path to have more ABI tricks
	switch p.ImportPath {
	case "runtime", "reflect", "embed",
		// TODO: collect directly from cmd/internal/objabi/pkgspecial.go,
		// in this particular case from allowAsmABIPkgs.
		"syscall",
		"internal/bytealg",
		"internal/chacha8rand",
		"internal/runtime/syscall/linux",
		"internal/runtime/syscall/windows",
		"internal/runtime/startlinetest":
		return p.ImportPath
	}
	// Intrinsics are matched by package import path as well.
	if _, ok := compilerIntrinsics[p.ImportPath]; ok {
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
		// as runtimeAndLinknamed already contains transitive dependencies.
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
		args = slices.DeleteFunc(args, func(arg string) bool {
			return strings.HasPrefix(arg, "-mod=") || strings.HasPrefix(arg, "-modfile=")
		})
	}

	args = append(args, packages...)
	cmd := exec.Command(sharedCache.GoCmd, args...)

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
			if !mainBuild && strings.Contains(perr.Err, "build constraints exclude all Go files") {
				// Some packages in runtimeAndLinknamed need a build tag to be importable,
				// like crypto/internal/boring/fipstls with boringcrypto,
				// so any pkg.Error should be ignored when the build tag isn't set.
			} else if !mainBuild && strings.Contains(perr.Err, "is not in std") {
				// When we support multiple Go versions at once, some packages may only
				// exist in the newer version, so we fail to list them with the older.
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

var listedRuntimeAndLinknamed = false

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
	// but luckily those few cases are covered by runtimeAndLinknamed as well.
	//
	// If ListedPackages lacks such a package we fill it via runtimeAndLinknamed.
	// TODO: can we instead add runtimeAndLinknamed to the top-level "go list" args?
	if from.Standard {
		if ok {
			return pkg, nil
		}
		if listedRuntimeAndLinknamed {
			return nil, fmt.Errorf("package %q still missing after go list call", path)
		}
		startTime := time.Now()
		missing := make([]string, 0, len(runtimeAndLinknamed))
		for _, linknamed := range runtimeAndLinknamed {
			switch {
			case sharedCache.ListedPackages[linknamed] != nil:
				// We already have it; skip.
			case sharedCache.GoEnv.GOOS != "js" && linknamed == "syscall/js":
				// GOOS-specific package.
			case sharedCache.GoEnv.GOOS != "darwin" && sharedCache.GoEnv.GOOS != "ios" && linknamed == "crypto/x509/internal/macos":
				// GOOS-specific package.
			default:
				missing = append(missing, linknamed)
			}
		}
		// We don't need any information about their dependencies, in this case.
		if err := appendListedPackages(missing, false); err != nil {
			return nil, fmt.Errorf("failed to load missing runtime-linknamed packages: %v", err)
		}
		pkg, ok := sharedCache.ListedPackages[path]
		if !ok {
			return nil, fmt.Errorf("std listed another std package that we can't find: %s", path)
		}
		listedRuntimeAndLinknamed = true
		log.Printf("listed %d missing runtime-linknamed packages in %s", len(missing), debugSince(startTime))
		return pkg, nil
	}
	if !ok {
		return nil, fmt.Errorf("list %s: %w", path, ErrNotFound)
	}

	// Packages outside std can list any package,
	// as long as they depend on it directly or indirectly.
	if from.hasDep(pkg.ImportPath) {
		return pkg, nil
	}

	// As a special case, any package can list runtime or its dependencies,
	// since those are always an implicit dependency.
	// We need to handle this ourselves as runtime does not appear in Deps.
	// TODO: it might be faster to bring back a "runtimeAndDeps" map or func.
	if pkg.ImportPath == "runtime" {
		return pkg, nil
	}
	if sharedCache.ListedPackages["runtime"].hasDep(pkg.ImportPath) {
		return pkg, nil
	}

	return nil, fmt.Errorf("list %s: %w", path, ErrNotDependency)
}
