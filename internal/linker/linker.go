// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"encoding/json"
	"fmt"
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

func cachePath() string {
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
	linkerBin := filepath.Join(cacheDir, cacheDirName, "link")
	if runtime.GOOS == "windows" {
		linkerBin += ".exe"
	}
	return linkerBin
}

func getCurrentVersion(goVersion string) string {
	return patchesVer + " " + goVersion
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

func existsFile(path string) bool {
	stat, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !stat.IsDir()
}

func GetModifiedLinker(goRoot, goVersion, tempDirectory string) (string, error) {
	outputLinkPath := cachePath()
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
