package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"os"
	"path/filepath"
	"slices"

	"github.com/rogpeppe/go-internal/cache"
)

const (
	debugDirSourceSubdir  = "source"
	debugDirGarbledSubdir = "garbled"

	debugCacheKindCompile = "compile"
	debugCacheKindAsm     = "asm"
)

type cachedDebugArtifacts struct {
	SourceFiles  map[string][]byte
	GarbledFiles map[string][]byte
}

func (a cachedDebugArtifacts) empty() bool {
	return len(a.SourceFiles) == 0 && len(a.GarbledFiles) == 0
}

func debugArtifactsCacheID(garbleActionID [sha256.Size]byte, kind string) [sha256.Size]byte {
	hasher := sha256.New()
	hasher.Write(garbleActionID[:])
	hasher.Write([]byte("\x00debugdir-cache-v1\x00"))
	hasher.Write([]byte(kind))
	var sum [sha256.Size]byte
	hasher.Sum(sum[:0])
	return sum
}

func writeDebugDirFile(subdir string, pkg *listedPackage, relPath string, content []byte) error {
	pkgDir := filepath.Join(flagDebugDir, subdir, filepath.FromSlash(pkg.ImportPath))
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return err
	}
	dstPath := filepath.Join(pkgDir, relPath)
	return os.WriteFile(dstPath, content, 0o666)
}

func saveDebugArtifactsForPkg(lpkg *listedPackage, kind string, artifacts cachedDebugArtifacts) error {
	if flagDebugDir == "" || artifacts.empty() {
		return nil
	}
	if len(lpkg.GarbleActionID) == 0 {
		return nil
	}
	fsCache, err := openCache()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(artifacts); err != nil {
		return err
	}
	return fsCache.PutBytes(debugArtifactsCacheID(lpkg.GarbleActionID, kind), buf.Bytes())
}

func loadDebugArtifactsForPkg(fsCache *cache.Cache, lpkg *listedPackage, kind string) (cachedDebugArtifacts, bool, error) {
	filename, _, err := fsCache.GetFile(debugArtifactsCacheID(lpkg.GarbleActionID, kind))
	if err != nil {
		return cachedDebugArtifacts{}, false, nil // cache miss is expected
	}
	f, err := os.Open(filename)
	if err != nil {
		return cachedDebugArtifacts{}, false, err
	}
	defer f.Close()
	var artifacts cachedDebugArtifacts
	if err := gob.NewDecoder(f).Decode(&artifacts); err != nil {
		return cachedDebugArtifacts{}, false, err
	}
	return artifacts, true, nil
}

func restoreDebugArtifactsForPkg(fsCache *cache.Cache, lpkg *listedPackage, kind string) error {
	artifacts, ok, err := loadDebugArtifactsForPkg(fsCache, lpkg, kind)
	if err != nil || !ok {
		return err
	}
	for relPath, content := range artifacts.SourceFiles {
		if err := writeDebugDirFile(debugDirSourceSubdir, lpkg, relPath, content); err != nil {
			return err
		}
	}
	for relPath, content := range artifacts.GarbledFiles {
		if err := writeDebugDirFile(debugDirGarbledSubdir, lpkg, relPath, content); err != nil {
			return err
		}
	}
	return nil
}

func restoreDebugDirFromCache() error {
	if flagDebugDir == "" {
		return nil
	}
	fsCache, err := openCache()
	if err != nil {
		return err
	}
	importPaths := make([]string, 0, len(sharedCache.ListedPackages))
	for importPath := range sharedCache.ListedPackages {
		importPaths = append(importPaths, importPath)
	}
	slices.Sort(importPaths)
	for _, importPath := range importPaths {
		lpkg := sharedCache.ListedPackages[importPath]
		if len(lpkg.GarbleActionID) == 0 {
			continue
		}
		if err := restoreDebugArtifactsForPkg(fsCache, lpkg, debugCacheKindCompile); err != nil {
			return err
		}
		if err := restoreDebugArtifactsForPkg(fsCache, lpkg, debugCacheKindAsm); err != nil {
			return err
		}
	}
	return nil
}

func debugArtifactsExistForPkg(fsCache *cache.Cache, lpkg *listedPackage, kind string) bool {
	_, _, err := fsCache.GetFile(debugArtifactsCacheID(lpkg.GarbleActionID, kind))
	return err == nil
}

func debugDirNeedsRebuild() (bool, error) {
	if flagDebugDir == "" {
		return false, nil
	}
	fsCache, err := openCache()
	if err != nil {
		return false, err
	}
	sawBuildInputs := false
	for _, lpkg := range sharedCache.ListedPackages {
		if len(lpkg.GarbleActionID) == 0 {
			continue
		}
		if len(lpkg.CompiledGoFiles) > 0 {
			sawBuildInputs = true
			if debugArtifactsExistForPkg(fsCache, lpkg, debugCacheKindCompile) {
				return false, nil
			}
		}
		if len(lpkg.SFiles) > 0 {
			sawBuildInputs = true
			if debugArtifactsExistForPkg(fsCache, lpkg, debugCacheKindAsm) {
				return false, nil
			}
		}
	}
	// If no debug artifacts exist yet, force one full rebuild to warm cache.
	// Once at least one cache entry exists, incremental builds can repopulate
	// from cache + newly rebuilt packages without forcing -a.
	return sawBuildInputs, nil
}
