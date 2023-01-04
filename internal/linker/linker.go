// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"mvdan.cc/garble/internal/patches"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	MagicValueEnv = "GARBLE_LNK_MAGIC"

	cacheDirName   = ".garble"
	versionExt     = ".version"
	garbleCacheDir = "GARBLE_CACHE_DIR"
)

var (
	//go:embed patches/*.patch
	linkerPatchesFs embed.FS

	linkerPatchesVer string
	linkerPatches    map[string]string

	baseSrcSubdir = filepath.Join("src", "cmd")
)

func init() {
	tmpVer, tmpPatch, err := patches.LoadPatches(linkerPatchesFs)
	if err != nil {
		panic(fmt.Errorf("cannot retrieve patches info: %v", err))
	}
	linkerPatchesVer = tmpVer
	linkerPatches = tmpPatch
}

func copyFile(src, target string) error {
	targetDir := filepath.Dir(target)
	if err := os.MkdirAll(targetDir, os.ModePerm); err != nil {
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

func existsFile(path string) bool {
	stat, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !stat.IsDir()
}

func applyPatches(srcDirectory, workingDirectory string) (map[string]string, error) {
	mod := make(map[string]string)
	for fileName, patch := range linkerPatches {
		oldPath := filepath.Join(srcDirectory, fileName)
		newPath := filepath.Join(workingDirectory, fileName)
		mod[oldPath] = newPath

		if err := patches.ApplyPatch(workingDirectory, patch); err != nil {
			return nil, fmt.Errorf("apply patch for %s failed: %v", fileName, err)
		}
	}
	return mod, nil
}

func cachePath(goExe string) string {
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
	return filepath.Join(cacheDir, cacheDirName, "link"+goExe)
}

func getCurrentVersion(goVersion string) string {
	return linkerPatchesVer + " " + goVersion
}

func checkVersion(linkerPath, goVersion string) (bool, error) {
	versionPath := linkerPath + versionExt
	version, err := os.ReadFile(versionPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return string(version) == getCurrentVersion(goVersion), nil
}

func writeVersion(linkerPath, goVersion string) error {
	versionPath := linkerPath + versionExt
	return os.WriteFile(versionPath, []byte(getCurrentVersion(goVersion)), os.ModePerm)
}

type overlayFile struct {
	Replace map[string]string
}

func compileLinker(workingDirectory string, overlay map[string]string, outputLinkPath string) error {
	file, err := json.Marshal(&overlayFile{Replace: overlay})
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(workingDirectory, "overlay.json")
	if err := os.WriteFile(overlayPath, file, os.ModePerm); err != nil {
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

func GetModifiedLinker(goRoot, goVersion, goExe, tempDirectory string) (string, error) {
	outputLinkPath := cachePath(goExe)
	isCorrectVer, err := checkVersion(outputLinkPath, goVersion)
	if err != nil {
		return "", err
	}
	if isCorrectVer && existsFile(outputLinkPath) {
		return outputLinkPath, nil
	}

	srcDir := filepath.Join(goRoot, baseSrcSubdir)
	workingDirectory := filepath.Join(tempDirectory, "linker-src")

	overlay, err := applyPatches(srcDir, workingDirectory)
	if err != nil {
		return "", err
	}
	if err := compileLinker(workingDirectory, overlay, outputLinkPath); err != nil {
		return "", err
	}
	if err := writeVersion(outputLinkPath, goVersion); err != nil {
		return "", err
	}
	return outputLinkPath, nil
}
