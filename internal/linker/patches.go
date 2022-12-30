// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

var (
	//go:embed patches/*.patch
	linkerPatches embed.FS

	patchesVer string
	patches    map[string]*bytes.Reader

	baseSrcSubdir = filepath.Join("src", "cmd")
)

func init() {
	tmpVer, tmpPatches, err := getPatchesVerAndModFiles()
	if err != nil {
		panic(fmt.Errorf("cannot retrieve patches info: %v", err))
	}
	patchesVer = tmpVer
	patches = tmpPatches
}

func getPatchesVerAndModFiles() (string, map[string]*bytes.Reader, error) {
	versionHash := sha256.New()
	patches := make(map[string]*bytes.Reader)
	err := fs.WalkDir(linkerPatches, "patches", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := linkerPatches.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		var patchBuf bytes.Buffer
		if _, err := io.Copy(&patchBuf, f); err != nil {
			return err
		}

		reader := bytes.NewReader(patchBuf.Bytes())
		if _, err := reader.WriteTo(versionHash); err != nil {
			return err
		}

		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			return err
		}

		files, _, err := gitdiff.Parse(reader)
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDelete || file.IsRename {
				panic("delete and rename patch not supported")
			}

			if _, err := reader.Seek(0, io.SeekStart); err != nil {
				return err
			}
			patches[file.OldName] = reader
		}
		return nil
	})

	if err != nil {
		return "", nil, err
	}
	return base64.RawStdEncoding.EncodeToString(versionHash.Sum(nil)), patches, nil
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

func applyPatch(workingDirectory, oldPath, newPath string, patch *bytes.Reader) error {
	if err := copyFile(oldPath, newPath); err != nil {
		return err
	}

	if _, err := patch.Seek(0, io.SeekStart); err != nil {
		return err
	}

	cmd := exec.Command("git", "-C", workingDirectory, "apply")
	cmd.Stdin = patch
	return cmd.Run()
}

func applyPatches(srcDirectory, workingDirectory string) (map[string]string, error) {
	mod := make(map[string]string)
	for fileName, patchReader := range patches {
		oldPath := filepath.Join(srcDirectory, fileName)
		newPath := filepath.Join(workingDirectory, fileName)
		mod[oldPath] = newPath

		if err := applyPatch(workingDirectory, oldPath, newPath, patchReader); err != nil {
			return nil, fmt.Errorf("apply patch for %s failed: %v", fileName, err)
		}
	}
	return mod, nil
}
