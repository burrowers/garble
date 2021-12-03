package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	Options flagOptions // garble options being used, i.e. our own flags

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

	// From "go env", primarily.
	GoEnv struct {
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
	f, err := os.Open(filepath.Join(sharedTempDir, "main-cache.gob"))
	if err != nil {
		return fmt.Errorf(`cannot open shared file, this is most likely due to not running "garble [command]"`)
	}
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

// flagOptions are derived from the flags
type flagOptions struct {
	ObfuscateLiterals bool
	Tiny              bool
	GarbleDir         string
	DebugDir          string
	Seed              []byte
}

// setFlagOptions sets flagOptions from the user supplied flags.
func setFlagOptions() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	if cache != nil {
		panic("opts set twice?")
	}
	opts = &flagOptions{
		GarbleDir:         wd,
		ObfuscateLiterals: flagObfuscateLiterals,
		Tiny:              flagGarbleTiny,
	}

	if flagSeed == "random" {
		opts.Seed = make([]byte, 16) // random 128 bit seed
		if _, err := rand.Read(opts.Seed); err != nil {
			return fmt.Errorf("error generating random seed: %v", err)
		}

	} else if len(flagSeed) > 0 {
		// We expect unpadded base64, but to be nice, accept padded
		// strings too.
		flagSeed = strings.TrimRight(flagSeed, "=")
		seed, err := base64.RawStdEncoding.DecodeString(flagSeed)
		if err != nil {
			return fmt.Errorf("error decoding seed: %v", err)
		}

		if len(seed) < 8 {
			return fmt.Errorf("-seed needs at least 8 bytes, have %d", len(seed))
		}

		opts.Seed = seed
	}

	if flagDebugDir != "" {
		if !filepath.IsAbs(flagDebugDir) {
			flagDebugDir = filepath.Join(wd, flagDebugDir)
		}

		if err := os.RemoveAll(flagDebugDir); err == nil || errors.Is(err, fs.ErrExist) {
			err := os.MkdirAll(flagDebugDir, 0o755)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("debugdir error: %v", err)
		}

		opts.DebugDir = flagDebugDir
	}

	return nil
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

	GarbleActionID []byte

	ToObfuscate bool
}

func (p *listedPackage) obfuscatedImportPath() string {
	if p.Name == "main" || p.ImportPath == "embed" || !p.ToObfuscate {
		return p.ImportPath
	}
	newPath := hashWith(p.GarbleActionID, p.ImportPath)
	// log.Printf("%q hashed with %x to %q", p.ImportPath, p.GarbleActionID, newPath)
	return newPath
}

// setListedPackages gets information about the current package
// and all of its dependencies
func setListedPackages(patterns []string) error {
	args := []string{"list", "-json", "-deps", "-export", "-trimpath"}
	args = append(args, cache.ForwardBuildFlags...)
	args = append(args, patterns...)
	cmd := exec.Command("go", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("go list error: %v", err)
	}

	binaryBuildID, err := buildidOf(cache.ExecPath)
	if err != nil {
		return err
	}
	cache.BinaryContentID = decodeHash(splitContentID(binaryBuildID))

	dec := json.NewDecoder(stdout)
	cache.ListedPackages = make(map[string]*listedPackage)
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return err
		}
		if pkg.Export != "" {
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
		if (pkg.Name == "main" && strings.HasSuffix(path, ".test")) || toObfuscate(path) {
			pkg.ToObfuscate = true
			anyToObfuscate = true
		}
	}

	if !anyToObfuscate {
		return fmt.Errorf("GOGARBLE=%q does not match any packages to be built", cache.GOGARBLE)
	}

	return nil
}

// listPackage gets the listedPackage information for a certain package
func listPackage(path string) (*listedPackage, error) {
	// If the path is listed in the top-level ImportMap, use its mapping instead.
	// This is a common scenario when dealing with vendored packages in GOROOT.
	// The map is flat, so we don't need to recurse.
	if path2 := curPkg.ImportMap[path]; path2 != "" {
		path = path2
	}

	pkg, ok := cache.ListedPackages[path]
	if !ok {
		return nil, fmt.Errorf("path not found in listed packages: %s", path)
	}
	return pkg, nil
}
