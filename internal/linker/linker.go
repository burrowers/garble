// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/rogpeppe/go-internal/lockedfile"
)

const (
	MagicValueEnv = "GARBLE_LINK_MAGIC"
	TinyEnv       = "GARBLE_LINK_TINY"

	cacheDirName   = "garble"
	versionExt     = ".version"
	garbleCacheDir = "GARBLE_CACHE_DIR"
	baseSrcSubdir  = "src"
)

var (
	//go:embed patches/*.patch
	linkerPatchesFS embed.FS
)

func loadLinkerPatches() (version string, modFiles map[string]bool, patches [][]byte, err error) {
	modFiles = make(map[string]bool)
	versionHash := sha256.New()
	err = fs.WalkDir(linkerPatchesFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		patchBytes, err := linkerPatchesFS.ReadFile(path)
		if err != nil {
			return err
		}

		if _, err := versionHash.Write(patchBytes); err != nil {
			return err
		}

		files, _, err := gitdiff.Parse(bytes.NewReader(patchBytes))
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsNew || file.IsDelete || file.IsCopy || file.IsRename {
				panic("only modification patch is supported")
			}
			modFiles[file.OldName] = true
		}
		patches = append(patches, patchBytes)
		return nil
	})

	if err != nil {
		return
	}
	version = base64.RawStdEncoding.EncodeToString(versionHash.Sum(nil))
	return
}

func copyFile(src, target string) error {
	targetDir := filepath.Dir(target)
	if err := os.MkdirAll(targetDir, 0o777); err != nil {
		return err
	}
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	targetFile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer targetFile.Close()
	_, err = io.Copy(targetFile, srcFile)
	return err
}

func fileExists(path string) bool {
	stat, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !stat.IsDir()
}

// TODO(pagran): Remove git dependency in future
// more information in README.md
func applyPatches(srcDir, workingDir string, modFiles map[string]bool, patches [][]byte) (map[string]string, error) {
	mod := make(map[string]string)
	for fileName := range modFiles {
		oldPath := filepath.Join(srcDir, fileName)
		newPath := filepath.Join(workingDir, fileName)
		mod[oldPath] = newPath

		if err := copyFile(oldPath, newPath); err != nil {
			return nil, err
		}
	}

	cmd := exec.Command("git", "apply")
	cmd.Dir = workingDir
	cmd.Stdin = bytes.NewReader(bytes.Join(patches, []byte("\n")))
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return mod, nil
}

func cachePath(goExe string) (string, error) {
	var cacheDir string
	if val, ok := os.LookupEnv(garbleCacheDir); ok {
		cacheDir = val
	} else {
		userCacheDir, err := os.UserCacheDir()
		if err != nil {
			panic(fmt.Errorf("cannot retreive user cache directory: %v", err))
		}
		cacheDir = userCacheDir
	}

	cacheDir = filepath.Join(cacheDir, cacheDirName)
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return "", err
	}

	// Note that we only keep one patched and built linker in the cache.
	// If the user switches between Go versions or garble versions often,
	// this may result in rebuilds since we don't keep multiple binaries in the cache.
	// We can consider keeping multiple versions of the binary in our cache in the future,
	// similar to how GOCACHE works with multiple built versions of the same package.
	return filepath.Join(cacheDir, "link"+goExe), nil
}

func getCurrentVersion(goVersion, patchesVer string) string {
	return goVersion + " " + patchesVer
}

func checkVersion(linkerPath, goVersion, patchesVer string) (bool, error) {
	versionPath := linkerPath + versionExt
	version, err := os.ReadFile(versionPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return string(version) == getCurrentVersion(goVersion, patchesVer), nil
}

func writeVersion(linkerPath, goVersion, patchesVer string) error {
	versionPath := linkerPath + versionExt
	return os.WriteFile(versionPath, []byte(getCurrentVersion(goVersion, patchesVer)), 0o777)
}

func buildLinker(workingDir string, overlay map[string]string, outputLinkPath string) error {
	file, err := json.Marshal(&struct{ Replace map[string]string }{overlay})
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(workingDir, "overlay.json")
	if err := os.WriteFile(overlayPath, file, 0o777); err != nil {
		return err
	}

	cmd := exec.Command("go", "build", "-overlay", overlayPath, "-o", outputLinkPath, "cmd/link")
	// Explicitly setting GOOS and GOARCH variables prevents conflicts during cross-build
	cmd.Env = append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	// Building cmd/link is possible from anywhere, but to avoid any possible side effects build in a temp directory
	cmd.Dir = workingDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compiler compile error: %v\n\n%s", err, string(out))
	}

	return nil
}

func PatchLinker(goRoot, goVersion, goExe, tempDir string) (string, func(), error) {
	patchesVer, modFiles, patches, err := loadLinkerPatches()
	if err != nil {
		panic(fmt.Errorf("cannot retrieve linker patches: %v", err))
	}

	outputLinkPath, err := cachePath(goExe)
	if err != nil {
		return "", nil, err
	}

	mutex := lockedfile.MutexAt(outputLinkPath + ".lock")
	unlock, err := mutex.Lock()
	if err != nil {
		return "", nil, err
	}

	// If build is successful, mutex unlocking must be on the caller's side
	successBuild := false
	defer func() {
		if !successBuild {
			unlock()
		}
	}()

	isCorrectVer, err := checkVersion(outputLinkPath, goVersion, patchesVer)
	if err != nil {
		return "", nil, err
	}
	if isCorrectVer && fileExists(outputLinkPath) {
		successBuild = true
		return outputLinkPath, unlock, nil
	}

	srcDir := filepath.Join(goRoot, baseSrcSubdir)
	workingDir := filepath.Join(tempDir, "linker-src")

	overlay, err := applyPatches(srcDir, workingDir, modFiles, patches)
	if err != nil {
		return "", nil, err
	}
	if err := buildLinker(workingDir, overlay, outputLinkPath); err != nil {
		return "", nil, err
	}
	if err := writeVersion(outputLinkPath, goVersion, patchesVer); err != nil {
		return "", nil, err
	}
	successBuild = true
	return outputLinkPath, unlock, nil
}
