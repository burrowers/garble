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
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/rogpeppe/go-internal/lockedfile"
)

const (
	MagicValueEnv = "GARBLE_LINKER_MAGIC"

	cacheDirName   = "garble"
	versionExt     = ".version"
	garbleCacheDir = "GARBLE_CACHE_DIR"
	baseSrcSubdir  = "src"
)

var (
	//go:embed patches/*.patch
	linkerPatchesFS embed.FS
)

func loadLinkerPatches() (string, map[string]string, error) {
	versionHash := sha256.New()
	patches := make(map[string]string)
	err := fs.WalkDir(linkerPatchesFS, ".", func(path string, d fs.DirEntry, err error) error {
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
			patches[file.OldName] = string(patchBytes)
		}
		return nil
	})

	if err != nil {
		return "", nil, err
	}
	return base64.RawStdEncoding.EncodeToString(versionHash.Sum(nil)), patches, nil
}

// TODO(pagran): Remove git dependency in future
// more information in README.md
func applyPatch(workingDir, patch string) error {
	cmd := exec.Command("git", "-C", workingDir, "apply")
	cmd.Stdin = strings.NewReader(patch)
	return cmd.Run()
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

func applyPatches(srcDir, workingDir string, patches map[string]string) (map[string]string, error) {
	mod := make(map[string]string)
	for fileName, patch := range patches {
		oldPath := filepath.Join(srcDir, fileName)
		newPath := filepath.Join(workingDir, fileName)
		mod[oldPath] = newPath

		if err := copyFile(oldPath, newPath); err != nil {
			return nil, err
		}

		if err := applyPatch(workingDir, patch); err != nil {
			return nil, fmt.Errorf("apply patch for %s failed: %v", fileName, err)
		}
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

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compiler compile error: %v\n\n%s", err, string(out))
	}

	return nil
}

func PatchLinker(goRoot, goVersion, goExe, tempDir string) (string, error) {
	patchesVer, patches, err := loadLinkerPatches()
	if err != nil {
		panic(fmt.Errorf("cannot retrieve linker patches: %v", err))
	}

	outputLinkPath, err := cachePath(goExe)
	if err != nil {
		return "", err
	}

	mutex := lockedfile.MutexAt(outputLinkPath + ".lock")
	unlock, err := mutex.Lock()
	if err != nil {
		return "", err
	}
	defer unlock()

	isCorrectVer, err := checkVersion(outputLinkPath, goVersion, patchesVer)
	if err != nil {
		return "", err
	}
	if isCorrectVer && fileExists(outputLinkPath) {
		return outputLinkPath, nil
	}

	srcDir := filepath.Join(goRoot, baseSrcSubdir)
	workingDir := filepath.Join(tempDir, "linker-src")

	overlay, err := applyPatches(srcDir, workingDir, patches)
	if err != nil {
		return "", err
	}
	if err := buildLinker(workingDir, overlay, outputLinkPath); err != nil {
		return "", err
	}
	if err := writeVersion(outputLinkPath, goVersion, patchesVer); err != nil {
		return "", err
	}
	return outputLinkPath, nil
}
