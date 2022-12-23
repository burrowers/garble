// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package linker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const MagicValueEnv = "GARBLE_LNK_MAGIC"

type overlayFile struct {
	Replace map[string]string
}

func compileLinker(workingDirectory string, overlay map[string]string, outputLinkPath string) error {
	file, _ := json.Marshal(&overlayFile{Replace: overlay})
	overlayPath := filepath.Join(workingDirectory, "overlay.json")
	if err := os.WriteFile(overlayPath, file, os.ModePerm); err != nil {
		return err
	}

	out, err := exec.Command("go", "build", "-overlay", overlayPath, "-o", outputLinkPath, "cmd/link").CombinedOutput()
	if err != nil {
		return fmt.Errorf("compiler compile error: %v\n\n%s", err, string(out))
	}
	return nil
}

func copyModFiles(srcDir, workingDirectory string) (map[string]string, error) {
	overlay := make(map[string]string)
	for _, name := range patchesModFiles {
		src := filepath.Join(srcDir, name)
		target := filepath.Join(workingDirectory, name)
		overlay[src] = target

		if err := copyFile(src, target); err != nil {
			return nil, err
		}
	}
	return overlay, nil
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

	overlay, err := copyModFiles(srcDir, workingDirectory)
	if err != nil {
		return "", err
	}
	if err := applyPatches(workingDirectory); err != nil {
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
