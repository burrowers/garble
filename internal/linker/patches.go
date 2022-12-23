// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"bufio"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	//go:embed patches/*.patch
	linkerPatches embed.FS

	patchesVer      string
	patchesModFiles []string

	baseSrcSubdir = filepath.Join("src", "cmd")
)

func init() {
	tmpVer, tmpModFiles, err := getPatchesVerAndModFiles()
	if err != nil {
		panic(fmt.Errorf("cannot retrieve patches info: %v", err))
	}
	patchesVer = tmpVer
	patchesModFiles = tmpModFiles
}

type walkPatchFunc func(path string, reader io.Reader) error

func walkPatches(walkFunc walkPatchFunc) error {
	return fs.WalkDir(linkerPatches, "patches", func(path string, d fs.DirEntry, err error) error {
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
		return walkFunc(path, f)
	})
}

func getPatchesVerAndModFiles() (string, []string, error) {
	hash := sha256.New()
	var modifiedFiles []string
	err := walkPatches(func(path string, reader io.Reader) error {
		hash.Write([]byte(path))

		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			hash.Write(scanner.Bytes())

			text := scanner.Text()
			if !strings.HasPrefix(text, "diff --git") {
				continue
			}

			// Extract modified file from: diff --git a/filename b/filename
			fields := strings.Fields(text)
			if len(fields) != 4 {
				continue
			}

			modifiedFile := fields[len(fields)-1]
			modifiedFile = modifiedFile[strings.IndexRune(modifiedFile, '/'):]

			modifiedFiles = append(modifiedFiles, modifiedFile)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return base64.RawStdEncoding.EncodeToString(hash.Sum(nil)), modifiedFiles, nil
}

func applyPatches(workingDirectory string) error {
	return walkPatches(func(path string, reader io.Reader) error {
		cmd := exec.Command("git", "-C", workingDirectory, "apply")
		cmd.Stdin = reader

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("apply patch %s failed: %v", path, err)
		}
		return nil
	})
}
