// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
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

func getCurrentVersion(goVersion string) []byte {
	return []byte(patchesVer + " " + goVersion)
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

	return bytes.Equal(version, getCurrentVersion(goVersion)), nil
}

func writeVersion(linkerPath, goVersion string) error {
	versionPath := linkerPath + versionExt
	return os.WriteFile(versionPath, getCurrentVersion(goVersion), os.ModePerm)
}
