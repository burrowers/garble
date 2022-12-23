// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"io"
	"os"
	"path/filepath"
)

func existsFile(path string) bool {
	stat, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !stat.IsDir()
}

func copyFile(src, target string) error {
	targetDir := filepath.Dir(target)
	if err := os.MkdirAll(targetDir, os.ModeDir); err != nil {
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
