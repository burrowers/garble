// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var (
	//go:embed patches/*.patch
	linkerPatches embed.FS

	patchesVer string
	patches    []*gitdiff.File

	baseSrcSubdir = filepath.Join("src", "cmd")
)

func init() {
	tmpVer, tmpModFiles, err := getPatchesVerAndModFiles()
	if err != nil {
		panic(fmt.Errorf("cannot retrieve patches info: %v", err))
	}
	patchesVer = tmpVer
	patches = tmpModFiles
}

func getPatchesVerAndModFiles() (string, []*gitdiff.File, error) {
	versionHash := sha256.New()
	var patches []*gitdiff.File
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

		if _, err := io.Copy(versionHash, f); err != nil {
			return err
		}

		seeker := f.(io.Seeker)
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}

		files, _, err := gitdiff.Parse(f)
		if err != nil {
			return err
		}
		patches = append(patches, files...)
		return nil
	})

	if err != nil {
		return "", nil, err
	}
	return base64.RawStdEncoding.EncodeToString(versionHash.Sum(nil)), patches, nil
}

func applyPatch(oldPath, newPath string, patch *gitdiff.File) error {
	oldFile, err := os.Open(oldPath)
	if err != nil {
		return err
	}
	defer oldFile.Close()

	newFileDir := filepath.Dir(newPath)
	if err := os.MkdirAll(newFileDir, os.ModePerm); err != nil {
		return err
	}

	newFile, err := os.Create(newPath)
	if err != nil {
		return err
	}
	defer newFile.Close()

	return gitdiff.Apply(newFile, oldFile, patch)
}

func applyPatches(srcDirectory, workingDirectory string) (map[string]string, error) {
	mod := make(map[string]string)
	for _, patch := range patches {
		oldPath := filepath.Join(srcDirectory, patch.OldName)
		newPath := filepath.Join(workingDirectory, patch.NewName)
		mod[oldPath] = newPath

		if err := applyPatch(oldPath, newPath, patch); err != nil {
			return nil, fmt.Errorf("apply patch for %s failed: %v", patch.OldName, err)
		}
	}
	return mod, nil
}
