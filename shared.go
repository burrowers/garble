package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sharedCache this data is sharedCache between the different garble processes.
//
// Note that we fill this cache once from the root process in saveListedPackages,
// store it into a temporary file via gob encoding, and then reuse that file
// in each of the garble toolexec sub-processes.
type sharedCache struct {
	ExecPath   string   // absolute path to the garble binary being used
	BuildFlags []string // build flags fed to the original "garble ..." command

	Options flagOptions // garble options being used, i.e. our own flags

	// ListedPackages contains data obtained via 'go list -json -export -deps'. This
	// allows us to obtain the non-garbled export data of all dependencies, useful
	// for type checking of the packages as we obfuscate them.
	ListedPackages map[string]*listedPackage
	MainImportPath string // TODO: remove with TOOLEXEC_IMPORTPATH
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
	dir, err := ioutil.TempDir("", "garble-shared")
	if err != nil {
		return "", err
	}

	sharedCache := filepath.Join(dir, "main-cache.gob")
	f, err := os.Create(sharedCache)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if err := gob.NewEncoder(f).Encode(&cache); err != nil {
		return "", err
	}
	return dir, nil
}

// flagOptions are derived from the flags
type flagOptions struct {
	GarbleLiterals bool
	Tiny           bool
	GarbleDir      string
	DebugDir       string
	Seed           []byte
	Random         bool
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
		GarbleDir:      wd,
		GarbleLiterals: flagGarbleLiterals,
		Tiny:           flagGarbleTiny,
	}

	if flagSeed == "random" {
		opts.Seed = make([]byte, 16) // random 128 bit seed
		if _, err := rand.Read(opts.Seed); err != nil {
			return fmt.Errorf("error generating random seed: %v", err)
		}

		opts.Random = true

	} else {
		flagSeed = strings.TrimRight(flagSeed, "=")
		seed, err := base64.RawStdEncoding.DecodeString(flagSeed)
		if err != nil {
			return fmt.Errorf("error decoding seed: %v", err)
		}

		if len(seed) != 0 && len(seed) < 8 {
			return fmt.Errorf("the seed needs to be at least 8 bytes, but is only %v bytes", len(seed))
		}

		opts.Seed = seed
	}

	if flagDebugDir != "" {
		if !filepath.IsAbs(flagDebugDir) {
			flagDebugDir = filepath.Join(wd, flagDebugDir)
		}

		if err := os.RemoveAll(flagDebugDir); err == nil || os.IsNotExist(err) {
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
	Export     string
	BuildID    string
	Deps       []string
	ImportMap  map[string]string

	Dir     string
	GoFiles []string

	// The fields below are not part of 'go list', but are still reused
	// between garble processes. Use "Garble" as a prefix to ensure no
	// collisions with the JSON fields from 'go list'.

	GarbleActionID []byte

	// TODO(mvdan): reuse this field once TOOLEXEC_IMPORTPATH is used
	private bool
}

// setListedPackages gets information about the current package
// and all of its dependencies
func setListedPackages(patterns []string) error {
	args := []string{"list", "-json", "-deps", "-export", "-trimpath"}
	args = append(args, cache.BuildFlags...)
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
	binaryContentID := decodeHash(splitContentID(binaryBuildID))

	dec := json.NewDecoder(stdout)
	cache.ListedPackages = make(map[string]*listedPackage)
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return err
		}
		if pkg.Export != "" {
			buildID := pkg.BuildID
			if buildID == "" {
				// go list only includes BuildID in 1.16+
				buildID, err = buildidOf(pkg.Export)
				if err != nil {
					panic(err) // shouldn't happen
				}
			}
			actionID := decodeHash(splitActionID(buildID))
			h := sha256.New()
			h.Write(actionID)
			h.Write(binaryContentID)

			pkg.GarbleActionID = h.Sum(nil)[:buildIDComponentLength]
		}
		if pkg.Name == "main" {
			if cache.MainImportPath != "" {
				return fmt.Errorf("found two main packages: %s %s", cache.MainImportPath, pkg.ImportPath)
			}
			cache.MainImportPath = pkg.ImportPath
		}
		cache.ListedPackages[pkg.ImportPath] = &pkg
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go list error: %v: %s", err, stderr.Bytes())
	}

	anyPrivate := false
	for path, pkg := range cache.ListedPackages {
		if isPrivate(path) {
			pkg.private = true
			anyPrivate = true
		}
	}

	if !anyPrivate {
		return fmt.Errorf("GOPRIVATE=%q does not match any packages to be built", os.Getenv("GOPRIVATE"))
	}
	for path, pkg := range cache.ListedPackages {
		if pkg.private {
			continue
		}
		for _, depPath := range pkg.Deps {
			if cache.ListedPackages[depPath].private {
				return fmt.Errorf("public package %q can't depend on obfuscated package %q (matched via GOPRIVATE=%q)",
					path, depPath, os.Getenv("GOPRIVATE"))
			}
		}
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
