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
	"go/version"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/rogpeppe/go-internal/lockedfile"
)

const (
	MagicValueEnv  = "GARBLE_LINK_MAGIC"
	TinyEnv        = "GARBLE_LINK_TINY"
	EntryOffKeyEnv = "GARBLE_LINK_ENTRYOFF_KEY"
)

//go:embed patches/*/*.patch
var linkerPatchesFS embed.FS

func loadLinkerPatches(majorGoVersion string) (version string, modFiles map[string]bool, patches [][]byte, err error) {
	modFiles = make(map[string]bool)
	versionHash := sha256.New()
	if err := fs.WalkDir(linkerPatchesFS, "patches/"+majorGoVersion, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
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
	}); err != nil {
		return "", nil, nil, err
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

	// If one of parent folders of workingDir contains repository, set current directory is not enough because git
	// by default treats workingDir as a subfolder of repository, so it will break git apply. Adding --git-dir flag blocks this behavior.
	cmd := exec.Command("git", "--git-dir", workingDir, "apply", "--verbose")
	cmd.Dir = workingDir
	// Ensure that the output messages are in plain English.
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	cmd.Stdin = bytes.NewReader(bytes.Join(patches, []byte("\n")))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to 'git apply' patches: %v:\n%s", err, out)
	}

	// Running git without errors does not guarantee that all patches have been applied.
	// Make sure that all passed patches have been applied correctly.
	rx := regexp.MustCompile(`(?m)^Applied patch .+ cleanly\.$`)
	if appliedPatches := len(rx.FindAllIndex(out, -1)); appliedPatches != len(patches) {
		return nil, fmt.Errorf("expected %d applied patches, actually %d:\n\n%s", len(patches), appliedPatches, string(out))
	}
	return mod, nil
}

func cachePath(cacheDir string) (string, error) {
	// Use a subdirectory to clarify what we're using it for.
	// Name it "tool", like Go's pkg/tool, as we might want to rebuild
	// other Go toolchain programs like the compiler or assembler in the future.
	cacheDir = filepath.Join(cacheDir, "tool")
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return "", err
	}
	goExe := ""
	if runtime.GOOS == "windows" {
		goExe = ".exe"
	}

	// Note that we only keep one patched and built linker in the cache.
	// If the user switches between Go versions or garble versions often,
	// this may result in rebuilds since we don't keep multiple binaries in the cache.
	// We can consider keeping multiple versions of the binary in our cache in the future,
	// similar to how GOCACHE works with multiple built versions of the same package.
	return filepath.Join(cacheDir, "link"+goExe), nil
}

func getCurrentVersion(goVersion, patchesVer string) string {
	// Note that we assume that if a Go toolchain reports itself as e.g. go1.24.1,
	// it really is that upstream Go version with no alterations or edits.
	// If any modifications are made, it should report itself as e.g. go1.24.1-corp.
	// The alternative would be to use the content ID hash of the cmd/link binary.
	return goVersion + " " + patchesVer + "\n"
}

const versionExt = ".version"

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

func buildLinker(goRoot, workingDir string, overlay map[string]string, outputLinkPath string) error {
	file, err := json.Marshal(&struct{ Replace map[string]string }{overlay})
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(workingDir, "overlay.json")
	if err := os.WriteFile(overlayPath, file, 0o777); err != nil {
		return err
	}

	goCmd := filepath.Join(goRoot, "bin", "go")
	cmd := exec.Command(goCmd, "build", "-overlay", overlayPath, "-o", outputLinkPath, "cmd/link")
	// Ignore any build settings from the environment or GOENV.
	// We want to build cmd/link like the rest of the toolchain,
	// regardless of what build options are set for the current build.
	//
	// TODO: a nicer way would be to use the same flags recorded in the current
	// cmd/link binary, which can be seen via:
	//
	//   go version -m ~/tip/pkg/tool/linux_amd64/link
	//
	// and which can be done from Go via debug/buildinfo.ReadFile.
	cmd.Env = append(cmd.Environ(),
		"GOENV=off", "GOOS=", "GOARCH=", "GOEXPERIMENT=", "GOFLAGS=",
	)
	// Building cmd/link is possible from anywhere, but to avoid any possible side effects build in a temp directory
	cmd.Dir = workingDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compiler compile error: %v\n\n%s", err, string(out))
	}

	return nil
}

func PatchLinker(goRoot, goVersion, cacheDir, tempDir string) (string, func(), error) {
	patchesVer, modFiles, patches, err := loadLinkerPatches(version.Lang(goVersion))
	if err != nil {
		return "", nil, fmt.Errorf("cannot retrieve linker patches: %v", err)
	}

	outputLinkPath, err := cachePath(cacheDir)
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

	srcDir := filepath.Join(goRoot, "src")
	workingDir := filepath.Join(tempDir, "linker-src")

	overlay, err := applyPatches(srcDir, workingDir, modFiles, patches)
	if err != nil {
		return "", nil, err
	}
	if err := buildLinker(goRoot, workingDir, overlay, outputLinkPath); err != nil {
		return "", nil, err
	}
	if err := writeVersion(outputLinkPath, goVersion, patchesVer); err != nil {
		return "", nil, err
	}
	successBuild = true
	return outputLinkPath, unlock, nil
}
