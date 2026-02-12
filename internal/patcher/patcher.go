// Copyright (c) 2022, The Garble Authors.
// See LICENSE for licensing information.

package patcher

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
	"sort"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/rogpeppe/go-internal/lockedfile"
)

const (
	PkgPathMapEnv = "GARBLE_PKGPATH_MAP"
	SymbolMapEnv  = "GARBLE_SYMBOL_MAP"

	MagicValueEnv  = "GARBLE_LINK_MAGIC"
	TinyEnv        = "GARBLE_LINK_TINY"
	EntryOffKeyEnv = "GARBLE_LINK_ENTRYOFF_KEY"
	// GoSrcEnv can be set to override Go source files while building patched tools.
	GoSrcEnv = "GARBLE_GO_SRC"

	// Bump when tool patch/build semantics change to invalidate cached binaries.
	toolchainBuildVersion = "v2"
)

// Files that we may need to overlay from the modified source while building
// cmd/compile and cmd/asm.
var compilerOverlayFiles = []string{
	"cmd/compile/internal/ssagen/intrinsics.go",
	"cmd/compile/internal/ssa/rewrite.go",
	"cmd/compile/internal/types/pkg.go",
	"cmd/compile/internal/types/type.go",
	"cmd/compile/internal/ir/func.go",
	"cmd/compile/internal/base/garble.go",
	"cmd/compile/internal/escape/call.go",
	"cmd/compile/internal/inline/inl.go",
	"cmd/compile/internal/walk/expr.go",
	"cmd/compile/internal/noder/reader.go",
	"cmd/compile/internal/ssagen/nowb.go",
	"cmd/compile/internal/reflectdata/reflect.go",
	"cmd/internal/objabi/pkgspecial.go",
}

// Files that we may need to overlay from the modified source while building
// cmd/link.
var linkerOverlayFiles = []string{
	"cmd/link/internal/loader/loader.go",
	"cmd/link/internal/ld/pcln.go",
	"cmd/link/internal/ld/deadcode.go",
	"cmd/link/internal/ld/dwarf.go",
	"cmd/link/internal/ld/lib.go",
	"cmd/link/internal/ld/symtab.go",
	"cmd/link/internal/ld/data.go",
	"cmd/link/internal/ld/main.go",
	"cmd/link/internal/ld/inittask.go",
	"cmd/link/internal/ld/xcoff.go",
	"cmd/link/internal/ppc64/asm.go",
	"cmd/link/internal/arm64/asm.go",
	"cmd/internal/goobj/builtin.go",
}

//go:embed patches/*/*.patch
var toolchainPatchesFS embed.FS

func loadToolchainPatches(majorGoVersion string) (version string, sourceFiles, patchFiles, deletedFiles map[string]bool, patches [][]byte, expectedAppliedPatches int, err error) {
	sourceFiles = make(map[string]bool)
	patchFiles = make(map[string]bool)
	deletedFiles = make(map[string]bool)

	versionHash := sha256.New()
	if err := fs.WalkDir(toolchainPatchesFS, "patches/"+majorGoVersion, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		patchBytes, err := toolchainPatchesFS.ReadFile(path)
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
		expectedAppliedPatches += len(files)
		for _, file := range files {
			if file.IsCopy || file.IsRename {
				return fmt.Errorf("unsupported patch operation (copy/rename) for %q", file.OldName)
			}
			if !file.IsNew {
				sourceFiles[file.OldName] = true
			}
			if file.IsDelete {
				deletedFiles[file.OldName] = true
				continue
			}
			patchFiles[file.NewName] = true
		}
		patches = append(patches, patchBytes)
		return nil
	}); err != nil {
		return "", nil, nil, nil, nil, 0, err
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

// TODO(pagran): Remove git dependency in future.
// More information in README.md.
func applyPatches(srcDir, workingDir string, sourceFiles map[string]bool, patches [][]byte, expectedAppliedPatches int) error {
	for fileName := range sourceFiles {
		oldPath := filepath.Join(srcDir, fileName)
		newPath := filepath.Join(workingDir, fileName)
		if err := copyFile(oldPath, newPath); err != nil {
			return err
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
		return fmt.Errorf("failed to 'git apply' patches: %v:\n%s", err, out)
	}

	// Running git without errors does not guarantee that all patches have been applied.
	// Make sure that all passed patches have been applied correctly.
	rx := regexp.MustCompile(`(?m)^Applied patch .+ cleanly\.$`)
	if appliedPatches := len(rx.FindAllIndex(out, -1)); appliedPatches != expectedAppliedPatches {
		return fmt.Errorf("expected %d applied patches, actually %d:\n\n%s", expectedAppliedPatches, appliedPatches, string(out))
	}
	return nil
}

func buildPatchOverlay(goRoot, workingDir string, patchFiles, deletedFiles map[string]bool) map[string]string {
	overlay := make(map[string]string)
	for fileName := range patchFiles {
		systemPath := filepath.Join(goRoot, "src", fileName)
		patchedPath := filepath.Join(workingDir, fileName)
		overlay[systemPath] = patchedPath
	}
	for fileName := range deletedFiles {
		systemPath := filepath.Join(goRoot, "src", fileName)
		overlay[systemPath] = ""
	}
	return overlay
}

func makeFileSet(files []string) map[string]bool {
	set := make(map[string]bool, len(files))
	for _, file := range files {
		set[file] = true
	}
	return set
}

func filterFiles(files map[string]bool, allowedFiles []string) map[string]bool {
	allowed := makeFileSet(allowedFiles)
	filtered := make(map[string]bool)
	for file := range files {
		if allowed[file] {
			filtered[file] = true
		}
	}
	return filtered
}

func normalizeGoRoot(goRoot string) string {
	// Toolchain upgrades via GOTOOLCHAIN can point GOROOT into GOMODCACHE, and
	// go build overlays cannot replace files from module cache paths.
	if strings.Contains(filepath.ToSlash(goRoot), "/pkg/mod/golang.org/toolchain@") {
		if hostGoRoot := runtime.GOROOT(); hostGoRoot != "" {
			return hostGoRoot
		}
	}
	return goRoot
}

func cachePathForTool(cacheDir, toolName string) (string, error) {
	cacheDir = filepath.Join(cacheDir, "tool")
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return "", err
	}
	goExe := ""
	if runtime.GOOS == "windows" {
		goExe = ".exe"
	}
	return filepath.Join(cacheDir, toolName+goExe), nil
}

func cachePath(cacheDir string) (string, error) {
	return cachePathForTool(cacheDir, "link")
}

func getCurrentVersion(goVersion, patchesVer string) string {
	// Note that we assume that if a Go toolchain reports itself as e.g. go1.24.1,
	// it really is that upstream Go version with no alterations or edits.
	// If any modifications are made, it should report itself as e.g. go1.24.1-corp.
	// The alternative would be to use the content ID hash of the binary.
	return goVersion + " " + patchesVer + "\n"
}

const versionExt = ".version"

func checkVersion(toolPath, goVersion, patchesVer string) (bool, error) {
	versionPath := toolPath + versionExt
	version, err := os.ReadFile(versionPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return string(version) == getCurrentVersion(goVersion, patchesVer), nil
}

func writeVersion(toolPath, goVersion, patchesVer string) error {
	versionPath := toolPath + versionExt
	return os.WriteFile(versionPath, []byte(getCurrentVersion(goVersion, patchesVer)), 0o777)
}

// hashFiles returns the SHA256 hash of multiple files' contents.
func hashFiles(paths []string) (string, error) {
	h := sha256.New()
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	return base64.RawStdEncoding.EncodeToString(h.Sum(nil)), nil
}

func collectOverlayFiles(goSrcRoot, goRoot string, patchFiles map[string]bool, extraFiles []string) (relFiles []string, absFiles []string) {
	if goSrcRoot == "" || goSrcRoot == goRoot {
		return nil, nil
	}

	seen := make(map[string]bool)
	for file := range patchFiles {
		modifiedPath := filepath.Join(goSrcRoot, "src", file)
		if fileExists(modifiedPath) {
			seen[file] = true
		}
	}
	for _, file := range extraFiles {
		modifiedPath := filepath.Join(goSrcRoot, "src", file)
		if fileExists(modifiedPath) {
			seen[file] = true
		}
	}

	for file := range seen {
		relFiles = append(relFiles, file)
	}
	sort.Strings(relFiles)

	for _, file := range relFiles {
		absFiles = append(absFiles, filepath.Join(goSrcRoot, "src", file))
	}
	return relFiles, absFiles
}

func buildTool(goRoot, workingDir string, overlay map[string]string, outputPath, toolPkg string) error {
	file, err := json.Marshal(&struct{ Replace map[string]string }{overlay})
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(workingDir, "overlay.json")
	if err := os.WriteFile(overlayPath, file, 0o777); err != nil {
		return err
	}

	goCmd := filepath.Join(goRoot, "bin", "go")
	cmd := exec.Command(goCmd, "build", "-overlay", overlayPath, "-o", outputPath, toolPkg)

	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GOPROXY=") ||
			strings.HasPrefix(e, "GOTOOLCHAIN=") ||
			strings.HasPrefix(e, "GOROOT=") ||
			strings.HasPrefix(e, "GOMODCACHE=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"GOENV=off", "GOOS=", "GOARCH=", "GOEXPERIMENT=", "GOFLAGS=",
		"GOROOT="+goRoot,
		"GOMODCACHE="+filepath.Join(workingDir, "gomodcache"),
		"GOPROXY=off",
	)
	cmd.Env = env
	cmd.Dir = workingDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s compile error: %v\n\n%s", toolPkg, err, string(out))
	}

	return nil
}

func patchAndBuildTool(toolName, toolPkg, goSrcRoot, goRoot, goVersion, cacheDir, tempDir string, extraOverlayFiles []string) (string, func(), error) {
	patchesVer, sourceFiles, patchFiles, deletedFiles, patches, expectedAppliedPatches, err := loadToolchainPatches(version.Lang(goVersion))
	if err != nil {
		return "", nil, fmt.Errorf("cannot retrieve toolchain patches: %v", err)
	}

	buildGoRoot := normalizeGoRoot(goRoot)
	toolPatchFiles := filterFiles(patchFiles, extraOverlayFiles)
	toolDeletedFiles := filterFiles(deletedFiles, extraOverlayFiles)

	overlayRelFiles, overlayAbsFiles := collectOverlayFiles(goSrcRoot, buildGoRoot, toolPatchFiles, extraOverlayFiles)
	overlayHash := ""
	if len(overlayAbsFiles) > 0 {
		overlayHash, err = hashFiles(overlayAbsFiles)
		if err != nil {
			return "", nil, fmt.Errorf("cannot hash toolchain overlay files: %v", err)
		}
	}
	fullVersion := patchesVer + "-" + overlayHash + "-" + toolchainBuildVersion

	outputPath, err := cachePathForTool(cacheDir, toolName)
	if err != nil {
		return "", nil, err
	}

	mutex := lockedfile.MutexAt(outputPath + ".lock")
	unlock, err := mutex.Lock()
	if err != nil {
		return "", nil, err
	}

	successBuild := false
	defer func() {
		if !successBuild {
			unlock()
		}
	}()

	isCorrectVer, err := checkVersion(outputPath, goVersion, fullVersion)
	if err != nil {
		return "", nil, err
	}
	if isCorrectVer && fileExists(outputPath) {
		successBuild = true
		return outputPath, unlock, nil
	}

	srcDir := filepath.Join(buildGoRoot, "src")
	workingDir := filepath.Join(tempDir, toolName+"-src")
	if err := os.RemoveAll(workingDir); err != nil {
		return "", nil, err
	}

	if err := applyPatches(srcDir, workingDir, sourceFiles, patches, expectedAppliedPatches); err != nil {
		return "", nil, err
	}

	overlay := buildPatchOverlay(buildGoRoot, workingDir, toolPatchFiles, toolDeletedFiles)
	for _, file := range overlayRelFiles {
		systemPath := filepath.Join(buildGoRoot, "src", file)
		modifiedPath := filepath.Join(goSrcRoot, "src", file)
		overlay[systemPath] = modifiedPath
	}

	if err := buildTool(buildGoRoot, workingDir, overlay, outputPath, toolPkg); err != nil {
		return "", nil, err
	}
	if err := writeVersion(outputPath, goVersion, fullVersion); err != nil {
		return "", nil, err
	}

	successBuild = true
	return outputPath, unlock, nil
}

func PatchCompiler(goSrcRoot, goRoot, goVersion, cacheDir, tempDir string) (string, func(), error) {
	return patchAndBuildTool("compile", "cmd/compile", goSrcRoot, goRoot, goVersion, cacheDir, tempDir, compilerOverlayFiles)
}

func PatchAssembler(goSrcRoot, goRoot, goVersion, cacheDir, tempDir string) (string, func(), error) {
	return patchAndBuildTool("asm", "cmd/asm", goSrcRoot, goRoot, goVersion, cacheDir, tempDir, compilerOverlayFiles)
}

func PatchLinker(goRoot, goVersion, cacheDir, tempDir string) (string, func(), error) {
	goSrcRoot := os.Getenv(GoSrcEnv)
	return patchAndBuildTool("link", "cmd/link", goSrcRoot, goRoot, goVersion, cacheDir, tempDir, linkerOverlayFiles)
}
