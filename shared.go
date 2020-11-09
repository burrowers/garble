package main

import (
	"bytes"
	"crypto/rand"
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

// shared this data is shared between the different garble processes
type shared struct {
	Options        options
	ListedPackages listedPackages
}

var cache *shared

// loadShared the shared data passed from the entry garble process
func loadShared() error {
	if cache == nil {
		f, err := os.Open(os.Getenv("GARBLE_SHARED"))
		if err != nil {
			return fmt.Errorf(`cannot open shared file, this is most likely due to not running "garble [command]"`)
		}
		defer f.Close()
		if err := gob.NewDecoder(f).Decode(&cache); err != nil {
			return err
		}
	}

	return nil
}

// saveShared the shared data to a file in order for subsequent
// garble processes to have access to the same data
func saveShared() (string, error) {
	f, err := ioutil.TempFile("", "garble-shared")
	if err != nil {
		return "", err
	}

	defer f.Close()

	if err := gob.NewEncoder(f).Encode(&cache); err != nil {
		return "", err
	}

	os.Setenv("GARBLE_SHARED", f.Name())

	return f.Name(), nil
}

// options are derived from the flags
type options struct {
	GarbleLiterals bool
	Tiny           bool
	GarbleDir      string
	DebugDir       string
	Seed           []byte
	Random         bool
}

// setOptions sets all options from the user supplied flags
func setOptions() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	opts = &options{
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

	cache = &shared{Options: *opts}

	return nil
}

// listedPackages contains data obtained via 'go list -json -export -deps'. This
// allows us to obtain the non-garbled export data of all dependencies, useful
// for type checking of the packages as we obfuscate them.
//
// Note that we obtain this data once in saveListedPackages, store it into a
// temporary file via gob encoding, and then reuse that file in each of the
// garble processes that wrap a package compilation.
type listedPackages map[string]*listedPackage

// listedPackage contains information useful for obfuscating a package
type listedPackage struct {
	ImportPath string
	Export     string
	Deps       []string
	ImportMap  map[string]string

	// TODO(mvdan): reuse this field once TOOLEXEC_IMPORTPATH is used
	private bool
}

// setListedPackages gets information about the current package
// and all of its dependencies
func setListedPackages(flags, patterns []string) error {
	args := []string{"list", "-json", "-deps", "-export"}
	args = append(args, flags...)
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
	dec := json.NewDecoder(stdout)
	cache.ListedPackages = make(listedPackages)
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return err
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
	pkg, ok := cache.ListedPackages[path]
	if !ok {
		if fromPkg, ok := cache.ListedPackages[curPkgPath]; ok {
			if path2 := fromPkg.ImportMap[path]; path2 != "" {
				return listPackage(path2)
			}
		}
		return nil, fmt.Errorf("path not found in listed packages: %s", path)
	}
	return pkg, nil
}
