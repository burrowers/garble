package main

import (
	"crypto/sha256"
	"maps"
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
	fsCache, err := openCache()
	if err != nil {
		return err
	}
	data, err := artifacts.MarshalMsg(nil)
	if err != nil {
		return err
	}
	return fsCache.PutBytes(debugArtifactsCacheID(lpkg.GarbleActionID, kind), data)
}

func restoreDebugArtifactsForPkg(fsCache *cache.Cache, lpkg *listedPackage, kind string) error {
	filename, _, err := fsCache.GetFile(debugArtifactsCacheID(lpkg.GarbleActionID, kind))
	if err != nil {
		return nil // cache miss is expected
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	var artifacts cachedDebugArtifacts
	if _, err := artifacts.UnmarshalMsg(data); err != nil {
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
	listed := sharedCache.ListedPackages.all()
	for _, importPath := range slices.Sorted(maps.Keys(listed)) {
		lpkg := listed[importPath]
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
	missingArtifacts := false
	for _, lpkg := range sharedCache.ListedPackages.all() {
		if len(lpkg.CompiledGoFiles) > 0 {
			if !debugArtifactsExistForPkg(fsCache, lpkg, debugCacheKindCompile) {
				missingArtifacts = true
			}
		}
		if len(lpkg.SFiles) > 0 {
			if !debugArtifactsExistForPkg(fsCache, lpkg, debugCacheKindAsm) {
				missingArtifacts = true
			}
		}
	}
	// For -debugdir to be complete, we either need artifacts in cache for every
	// package input, or we need to force one full rebuild with -a.
	return missingArtifacts, nil
}
