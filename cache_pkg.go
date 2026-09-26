package main

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/types"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rogpeppe/go-internal/cache"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ssa"
)

// "maps binkeys" lets pkgCache.ReflectAPIs use its int-keyed inner map.
//go:generate go tool msgp -file=$GOFILE -o=cache_pkg_gen.go -io=false -tests=false -unexported -d "maps binkeys"

// importerWithMap holds func fields and is never serialized.
//msgp:ignore importerWithMap

// goAsmNames maps go_asm.h names to obfuscated ones. It is a named type so msgp
// can generate its marshalers; see [transformer.saveGoAsmNames].
type goAsmNames map[string]string

// cachedDebugArtifacts is defined here, rather than in debugdir.go, so msgp
// generates its marshalers alongside the other cache types; see debugdir.go.
type cachedDebugArtifacts struct {
	SourceFiles  map[string][]byte
	GarbledFiles map[string][]byte
}

// pkgCache contains information about a package that will be stored in fsCache.
// Note that pkgCache is "deep", containing information about all packages
// which are transitive dependencies as well.
type pkgCache struct {
	// ReflectAPIs is a static record of what std APIs use reflection on their
	// parameters, so we can avoid obfuscating types used with them.
	// The key is a [types.Func.FullName] plus [stripTypeArgs].
	//
	// TODO: we're not including fmt.Printf, as it would have many false positives,
	// unless we were smart enough to detect which arguments get used as %#v or %T.
	ReflectAPIs map[string]map[int]bool
	// ReflectCallEdges retain parameter forwarding through named calls even when
	// the callee's reflection use is only discovered in a downstream package.
	ReflectCallEdges []reflectCallEdge

	// ReflectObjectNames maps obfuscated names which are reflected to their original
	// non-obfuscated names. The key is a [reflectInspector.obfuscatedObjectName].
	ReflectObjectNames map[string]string
}

type reflectCallEdge struct {
	Caller, Callee           string
	CallerParam, CalleeParam int
}

func (c *pkgCache) CopyFrom(c2 pkgCache) {
	for name, params := range c2.ReflectAPIs {
		if c.ReflectAPIs[name] == nil {
			c.ReflectAPIs[name] = make(map[int]bool)
		}
		maps.Copy(c.ReflectAPIs[name], params)
	}
	maps.Copy(c.ReflectObjectNames, c2.ReflectObjectNames)
	if len(c2.ReflectCallEdges) > 0 {
		seen := make(map[reflectCallEdge]bool, len(c.ReflectCallEdges)+len(c2.ReflectCallEdges))
		for _, edge := range c.ReflectCallEdges {
			seen[edge] = true
		}
		for _, edge := range c2.ReflectCallEdges {
			if !seen[edge] {
				seen[edge] = true
				c.ReflectCallEdges = append(c.ReflectCallEdges, edge)
			}
		}
	}
}

func ssaBuildPkg(pkg *types.Package, files []*ast.File, info *types.Info) *ssa.Package {
	// Create SSA packages only for pkg's direct imports. pkg's syntax can only
	// name objects from those, so the builder's package-level lookups never need
	// the transitive closure; methods of types from deeper packages are created
	// on demand by go/ssa (see Program.objectMethod).
	ssaProg := ssa.NewProgram(fset, 0)
	for _, p := range pkg.Imports() {
		ssaProg.CreatePackage(p, nil, nil, true)
	}

	ssaPkg := ssaProg.CreatePackage(pkg, files, info, false)
	ssaPkg.Build()
	return ssaPkg
}

// openCache opens the hashed build cache, memoized per process since
// sharedCache.CacheDir is fixed and it is called several times per compile.
var openCache = sync.OnceValues(func() (*cache.Cache, error) {
	// Use a subdirectory for the hashed build cache, to clarify what it is,
	// and to allow us to have other directories or files later on without mixing.
	dir := filepath.Join(sharedCache.CacheDir, "build")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	return cache.Open(dir)
})

// parseFiles parses the given Go files of lpkg. It supports relative file paths,
// such as those found in listedPackage.CompiledGoFiles, as long as dir is set to
// listedPackage.Dir. When mainPatch is true and lpkg is a main package, garble's
// reflection support code is patched in; needed when compiling, not when only
// inspecting names as reverse and map do.
func parseFiles(lpkg *listedPackage, dir string, paths []string, mainPatch bool) (files []*ast.File, err error) {
	mainPackage := mainPatch && lpkg.Name == "main" && lpkg.ForTest == ""

	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}

		var src any

		base := filepath.Base(path)
		if lpkg.ImportPath == "internal/abi" && base == "type.go" {
			src, err = abiNamePatch(path)
			if err != nil {
				return nil, err
			}
		} else if mainPackage && reflectPatchFile == "" && !strings.HasPrefix(base, "_cgo_") {
			// Note that we cannot add our code to e.g. _cgo_gotypes.go.
			src, err = reflectMainPrePatch(path)
			if err != nil {
				return nil, err
			}

			reflectPatchFile = path
		}

		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return nil, err
		}

		if mainPackage && src != "" {
			astutil.AddNamedImport(fset, file, "_", "unsafe")
		}

		files = append(files, file)
	}
	if mainPackage && reflectPatchFile == "" {
		return nil, fmt.Errorf("main packages must get reflect code patched in")
	}
	return files, nil
}

func loadPkgCache(lpkg *listedPackage, pkg *types.Package, files []*ast.File, info *types.Info, ssaPkg *ssa.Package) (pkgCache, error) {
	fsCache, err := openCache()
	if err != nil {
		return pkgCache{}, err
	}
	filename, _, err := fsCache.GetFile(lpkg.GarbleActionID)
	// Already in the cache; load it directly.
	if err == nil {
		data, err := os.ReadFile(filename)
		if err != nil {
			return pkgCache{}, err
		}
		var loaded pkgCache
		if _, err := loaded.UnmarshalMsg(data); err != nil {
			return pkgCache{}, fmt.Errorf("msgp decode: %w", err)
		}
		return loaded, nil
	}
	return computePkgCache(fsCache, lpkg, pkg, files, info, ssaPkg)
}

func computePkgCache(fsCache *cache.Cache, lpkg *listedPackage, pkg *types.Package, files []*ast.File, info *types.Info, ssaPkg *ssa.Package) (pkgCache, error) {
	// Not yet in the cache. Load the cache entries for all direct dependencies,
	// build our cache entry, and write it to disk.
	// Note that practically all errors from Cache.GetFile are a cache miss;
	// for example, a file might exist but be empty if another process
	// is filling the same cache entry concurrently.
	computed := pkgCache{
		ReflectAPIs: map[string]map[int]bool{
			"reflect.TypeOf":  {0: true},
			"reflect.ValueOf": {0: true},
		},
		ReflectObjectNames: map[string]string{},
	}
	// Standard packages with no reflection dependency cannot contribute reflected
	// names. Non-standard packages may still forward models to an interface whose
	// implementation is found downstream, so retain their call edges.
	if !lpkg.hasDep("reflect") && lpkg.Standard {
		return computed, nil
	}
	for _, imp := range lpkg.Imports {
		if imp == "C" {
			// `go list -json` shows "C" in Imports but not Deps.
			// See https://go.dev/issue/60453.
			continue
		}
		// Shadowing lpkg ensures we don't use the wrong listedPackage below.
		lpkg, err := listPackage(lpkg, imp)
		if err != nil {
			return computed, err
		}
		if lpkg.BuildID == "" {
			continue // nothing to load
		}
		if err := func() error { // function literal for the deferred close
			if filename, _, err := fsCache.GetFile(lpkg.GarbleActionID); err == nil {
				// Cache hit; merge its entries into computed. We decode into a
				// fresh value rather than onto computed, as msgp replaces maps
				// rather than merging into them.
				data, err := os.ReadFile(filename)
				if err != nil {
					return err
				}
				var loaded pkgCache
				if _, err := loaded.UnmarshalMsg(data); err != nil {
					return err
				}
				computed.CopyFrom(loaded)
				return nil
			}
			// A non-standard dependency may forward a parameter through an
			// interface even if it never imports reflect itself.
			if !lpkg.hasDep("reflect") && lpkg.Standard {
				return nil
			}
			// Missing or corrupted entry in the cache for a dependency.
			// Could happen if GARBLE_CACHE was emptied but GOCACHE was not.
			// Compute it, which can recurse if many entries are missing.
			files, err := parseFiles(lpkg, lpkg.Dir, lpkg.CompiledGoFiles, true)
			if err != nil {
				return err
			}
			origImporter := importerForPkg(lpkg)
			pkg, info, err := typecheck(lpkg.ImportPath, files, origImporter, true)
			if err != nil {
				return err
			}
			computedImp, err := computePkgCache(fsCache, lpkg, pkg, files, info, nil)
			if err != nil {
				return err
			}
			computed.CopyFrom(computedImp)
			return nil
		}(); err != nil {
			return pkgCache{}, fmt.Errorf("pkgCache load for %s: %w", imp, err)
		}
	}

	// Reflection discovered by a downstream implementation must flow back across
	// interface calls in already compiled dependencies. Resolve those saved
	// forwarding edges after merging the dependency caches.
	for changed := true; changed; {
		changed = false
		for _, edge := range computed.ReflectCallEdges {
			if !computed.ReflectAPIs[edge.Callee][edge.CalleeParam] {
				continue
			}
			params := computed.ReflectAPIs[edge.Caller]
			if params == nil {
				params = make(map[int]bool)
				computed.ReflectAPIs[edge.Caller] = params
			}
			if !params[edge.CallerParam] {
				params[edge.CallerParam] = true
				changed = true
			}
		}
	}

	// Fill the reflect info from SSA, which builds on top of the syntax tree and type info.
	inspector := reflectInspector{
		lpkg:            lpkg,
		pkg:             pkg,
		propagatedInstr: map[ssa.Instruction]bool{},
		result:          computed, // append the results
	}
	if ssaPkg == nil {
		ssaPkg = ssaBuildPkg(pkg, files, info)
	}
	inspector.recordReflection(ssaPkg)
	computed = inspector.result // the edge slice header can grow during analysis

	data, err := computed.MarshalMsg(nil)
	if err != nil {
		return pkgCache{}, err
	}
	if err := fsCache.PutBytes(lpkg.GarbleActionID, data); err != nil {
		return pkgCache{}, err
	}
	return computed, nil
}

type importerWithMap struct {
	importMap  map[string]string
	importFrom func(path, dir string, mode types.ImportMode) (*types.Package, error)
}

func (im importerWithMap) Import(path string) (*types.Package, error) {
	panic("should never be called")
}

func (im importerWithMap) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path2 := im.importMap[path]; path2 != "" {
		path = path2
	}
	return im.importFrom(path, dir, mode)
}

func importerForPkg(lpkg *listedPackage) importerWithMap {
	return importerWithMap{
		importFrom: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			pkg, err := listPackage(lpkg, path)
			if err != nil {
				return nil, err
			}
			return os.Open(pkg.Export)
		}).(types.ImporterFrom).ImportFrom,
		importMap: lpkg.ImportMap,
	}
}
