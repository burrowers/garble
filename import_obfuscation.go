// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Binject/debug/goobj2"
)

// pkgInfo stores a parsed go archive/object file,
// and the original path to which it was read from.
type pkgInfo struct {
	pkg     *goobj2.Package
	path    string
	private bool
}

// dataType signifies whether the Data portion of a
// goobj2.Sym is reflection data for an import path,
// reflection data for a method of struct field, or
// something else.
type dataType uint8

const (
	other dataType = iota
	importPath
	namedata
)

// privateImports stores package paths and names that
// match GOPRIVATE. privateNames are elements of the
// paths in privatePaths, separated so that the shorter
// names don't accidentally match another import, such
// as a stdlib package
type privateImports struct {
	privatePaths []string
	privateNames []privateName
}

// privateName is a package name with a unique seed
// that insures that two package names with the same
// name but from different package paths will be hashed
// differently.
type privateName struct {
	name string
	seed []byte
}

func appendPrivateNameMap(pkg *goobj2.Package, nameMap map[string]string) error {
	for _, member := range pkg.ArchiveMembers {
		if member.ArchiveHeader.Name != garbleMapHeaderName {
			continue
		}

		serializedMap := member.ArchiveHeader.Data
		serializedMap = serializedMap[:bytes.IndexByte(serializedMap, 0x00)]
		if err := json.Unmarshal(serializedMap, &nameMap); err != nil {
			return err
		}
		return nil
	}
	return nil
}

// extractDebugObfSrc extracts obfuscated sources from object files if -debugdir flag is enabled.
func extractDebugObfSrc(pkgPath string, pkg *goobj2.Package) error {
	if envGarbleDebugDir == "" {
		return nil
	}

	var archiveMember *goobj2.ArchiveMember
	for _, member := range pkg.ArchiveMembers {
		if member.ArchiveHeader.Name == garbleSrcHeaderName {
			archiveMember = &member
			break
		}
	}

	if archiveMember == nil {
		return nil
	}

	osPkgPath := filepath.FromSlash(pkgPath)
	pkgDebugDir := filepath.Join(envGarbleDebugDir, osPkgPath)
	if err := os.MkdirAll(pkgDebugDir, 0o755); err != nil {
		return err
	}

	archive := bytes.NewBuffer(archiveMember.ArchiveHeader.Data)
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		debugFilePath := filepath.Join(pkgDebugDir, header.Name)
		debugFile, err := os.Create(debugFilePath)
		if err != nil {
			return err
		}
		if _, err := io.Copy(debugFile, tarReader); err != nil {
			return err
		}
		if err := debugFile.Close(); err != nil {
			return err
		}

		obfuscationTime := header.ModTime.Local()

		// Restore the actual source obfuscation time so as not to mislead the user.
		if err := os.Chtimes(debugFilePath, obfuscationTime, obfuscationTime); err != nil {
			return err
		}
	}
}

// obfuscateImports does all the necessary work to replace the import paths of
// obfuscated packages with hashes. It takes the single object file and import
// config passed to the linker, as well as a temporary directory to store
// modified object files.
//
// For each garbled package, we write a modified version of its object file,
// replacing import paths as necessary. We can't modify the object files
// in-place, as those are the cached compiler output. Modifying the output of
// the compiler cache would trigger recompilations.
//
// Note that we can modify the importcfg file in-place, because it's not part of
// the build cache.
//
// It returns the path to the modified main object file, to be used for linking.
// We also return a map of how the imports were garbled, as well as the private
// name map recovered from the archive files, so that we can amend -X flags.
func obfuscateImports(objPath, tempDir, importCfgPath string, importMap goobj2.ImportMap) (garbledObj string, garbledImports, privateNameMap map[string]string, _ error) {
	importCfg, err := goobj2.ParseImportCfg(importCfgPath)
	if err != nil {
		return "", nil, nil, err
	}
	mainPkg, err := goobj2.Parse(objPath, "main", importMap)
	if err != nil {
		return "", nil, nil, fmt.Errorf("error parsing main objfile: %v", err)
	}
	if err := extractDebugObfSrc("main", mainPkg); err != nil {
		return "", nil, nil, err
	}
	pkgs := []pkgInfo{{mainPkg, objPath, true}}

	privateNameMap = make(map[string]string)
	// build list of imported packages that are private
	for pkgPath, info := range importCfg.Packages {
		// if the '-tiny' flag is passed, we will strip filename
		// and position info of every package, but not garble anything
		if private := isPrivate(pkgPath); envGarbleTiny || private {
			pkg, err := goobj2.Parse(info.Path, pkgPath, importMap)
			if err != nil {
				return "", nil, nil, fmt.Errorf("error parsing objfile %s at %s: %v", pkgPath, info.Path, err)
			}

			pkgs = append(pkgs, pkgInfo{pkg, info.Path, private})

			if err := appendPrivateNameMap(pkg, privateNameMap); err != nil {
				return "", nil, nil, fmt.Errorf("error parsing name map %s at %s: %v", pkgPath, info.Path, err)
			}
			// Avoiding double extraction from main object file
			if objPath != info.Path {
				if err := extractDebugObfSrc(pkgPath, pkg); err != nil {
					return "", nil, nil, err
				}
			}
		}
	}

	var sb strings.Builder
	var buf bytes.Buffer

	garbledImports = make(map[string]string)
	replacedFiles := make(map[string]string)
	for _, p := range pkgs {
		// log.Printf("++ Obfuscating object file for %s ++", p.pkg.ImportPath)
		for _, am := range p.pkg.ArchiveMembers {
			// log.Printf("\t## Obfuscating archive member %s ##", am.ArchiveHeader.Name)

			// skip objects that are not used by the linker, or that do not contain
			// any Go symbol info
			if am.IsCompilerObj() || am.IsDataObj {
				continue
			}

			// not part of a private package, so just strip filename
			// and position info and move on
			if !p.private {
				stripPCLinesAndNames(&am)
				continue
			}

			// add all private import paths to a list to garble
			var privImports privateImports
			privImports.privatePaths, privImports.privateNames = explodeImportPath(p.pkg.ImportPath)
			// the main package might not have the import path "main" due to modules,
			// so add "main" to private import paths
			if p.pkg.ImportPath == buildInfo.firstImport {
				privImports.privatePaths = append(privImports.privatePaths, "main")
			}

			initImport := func(imp string) string {
				if !isPrivate(imp) {
					return imp
				}

				privPaths, privNames := explodeImportPath(imp)
				privImports.privatePaths = append(privImports.privatePaths, privPaths...)
				privImports.privateNames = append(privImports.privateNames, privNames...)

				return hashImport(imp, nil, garbledImports)
			}

			for i := range am.Imports {
				am.Imports[i].Pkg = initImport(am.Imports[i].Pkg)
			}
			for i := range am.Packages {
				am.Packages[i] = initImport(am.Packages[i])
			}

			privImports.privatePaths = dedupStrings(privImports.privatePaths)
			privImports.privateNames = dedupPrivateNames(privImports.privateNames)
			// move imports that contain another import as a substring to the front,
			// so that the shorter import will not match first and leak part of an
			// import path
			sort.Slice(privImports.privatePaths, func(i, j int) bool {
				iSlashes := strings.Count(privImports.privatePaths[i], "/")
				jSlashes := strings.Count(privImports.privatePaths[j], "/")
				// sort by number of slashes unless equal, then sort reverse alphabetically
				if iSlashes == jSlashes {
					return privImports.privatePaths[i] > privImports.privatePaths[j]
				}
				return iSlashes > jSlashes
			})
			sort.Slice(privImports.privateNames, func(i, j int) bool {
				return privImports.privateNames[i].name > privImports.privateNames[j].name
			})

			// no private import paths, nothing to garble
			if len(privImports.privatePaths) == 0 {
				continue
			}
			// log.Printf("\t== Private imports: %v ==\n", privImports)

			// garble all private import paths in all symbol names
			garbleSymbols(&am, privImports, garbledImports, &buf, &sb)
		}

		// An archive under the temporary file. Note that
		// ioutil.TempFile creates a file to ensure no collisions, so we
		// simply use its name after closing the file.
		tempObjFile, err := ioutil.TempFile(tempDir, "pkg.*.a")
		if err != nil {
			return "", nil, nil, fmt.Errorf("creating temp file: %v", err)
		}
		tempObj := tempObjFile.Name()
		tempObjFile.Close()
		if err := p.pkg.Write(tempObj); err != nil {
			return "", nil, nil, fmt.Errorf("error writing objfile %s at %s: %v", p.pkg.ImportPath, p.path, err)
		}
		replacedFiles[p.path] = tempObj
	}

	// garble importcfg so the linker knows where to find garbled imports
	if err := garbleImportCfg(importCfgPath, importCfg, garbledImports, replacedFiles); err != nil {
		return "", nil, nil, err
	}

	return replacedFiles[objPath], garbledImports, privateNameMap, nil
}

// stripPCLinesAndNames removes all filename and position info
// from an archive member.
func stripPCLinesAndNames(am *goobj2.ArchiveMember) {
	lists := [][]*goobj2.Sym{am.SymDefs, am.NonPkgSymDefs, am.NonPkgSymRefs}
	for _, list := range lists {
		for _, s := range list {
			// remove filename symbols when -tiny is passed as they
			// are only used for printing panics, and -tiny removes
			// panic printing; we need to set the symbol names to
			// 'gofile..', otherwise the linker will expect to see
			// filename symbols and panic
			if strings.HasPrefix(s.Name, "gofile..") {
				s.Name = "gofile.."
			}

			if s.Func == nil {
				continue
			}

			for _, inl := range s.Func.InlTree {
				inl.Line = 1
			}

			s.Func.PCFile = nil
			s.Func.PCLine = nil
			s.Func.PCInline = nil

			// remove unneeded debug aux symbols
			s.Func.DwarfInfo = nil
			s.Func.DwarfLoc = nil
			s.Func.DwarfRanges = nil
			s.Func.DwarfDebugLines = nil

		}
	}

	// remove dwarf file list, it isn't needed as we pass "-w, -s" to the linker
	am.DWARFFileList = nil
}

// explodeImportPath returns lists of import paths
// and package names that could all potentially be
// in symbol names of the package that imported 'path'.
// ex. path=github.com/foo/bar/baz, GOPRIVATE=github.com/*
// pkgPaths=[github.com/foo/bar, github.com/foo]
// pkgNames=[foo, bar, baz]
// Because package names could refer to multiple import
// paths, a seed is bundled with each package name to
// ensure that 2 identical package names from different
// import paths will get hashed differently.
func explodeImportPath(path string) ([]string, []privateName) {
	paths := strings.Split(path, "/")
	if len(paths) == 1 {
		return []string{path}, nil
	}

	pkgPaths := make([]string, 0, len(paths)-1)
	pkgNames := make([]privateName, 0, len(paths)-1)

	var restPrivate bool
	if isPrivate(paths[0]) {
		pkgPaths = append(pkgPaths, paths[0])
		restPrivate = true
	}

	// find first private match
	privateIdx := 1
	if !restPrivate {
		newPath := paths[0]
		for i := 1; i < len(paths); i++ {
			newPath += "/" + paths[i]
			if isPrivate(newPath) {
				pkgPaths = append(pkgPaths, newPath)
				pkgNames = append(pkgNames, newPrivateName(paths[i], newPath))

				privateIdx = i + 1
				restPrivate = true
				break
			}
		}

		if !restPrivate {
			return nil, nil
		}
	}

	lastComboIdx := 1
	for i := privateIdx; i < len(paths); i++ {
		newPath := pkgPaths[lastComboIdx-1] + "/" + paths[i]
		pkgPaths = append(pkgPaths, newPath)
		pkgNames = append(pkgNames, newPrivateName(paths[i], newPath))

		lastComboIdx++
	}
	lastPath := paths[len(paths)-1]
	pkgNames = append(pkgNames, newPrivateName(lastPath, path))

	return pkgPaths, pkgNames
}

// newPrivateName creates a privateName, from a package name
// and package path, setting the seed to path's actionID if
// we know it; if not, the seed is set to the path itself.
func newPrivateName(name, path string) privateName {
	pName := privateName{name: name}

	if actionID := buildInfo.imports[path].actionID; actionID == nil {
		pName.seed = []byte(path)
	} else {
		pName.seed = actionID
	}

	return pName
}

func dedupStrings(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	j := 0
	for _, v := range paths {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		paths[j] = v
		j++
	}
	return paths[:j]
}

func dedupPrivateNames(names []privateName) []privateName {
	seen := make(map[string]struct{}, len(names))
	j := 0
	for _, v := range names {
		combined := v.name + string(v.seed)
		if _, ok := seen[combined]; ok {
			continue
		}
		seen[combined] = struct{}{}
		names[j] = v
		j++
	}
	return names[:j]
}

func hashImport(pkg string, seed []byte, garbledImports map[string]string) string {
	if seed == nil {
		if garbledPkg, ok := garbledImports[pkg]; ok {
			return garbledPkg
		}
		seed = buildInfo.imports[pkg].actionID
	}

	garbledPkg := hashWith(seed, pkg)
	garbledImports[pkg] = garbledPkg

	return garbledPkg
}

// garbleSymbols replaces all private import paths/package names in symbol names
// and data of an archive member.
func garbleSymbols(am *goobj2.ArchiveMember, privImports privateImports, garbledImports map[string]string, buf *bytes.Buffer, sb *strings.Builder) {
	lists := [][]*goobj2.Sym{am.SymDefs, am.NonPkgSymDefs, am.NonPkgSymRefs}
	for _, list := range lists {
		for _, s := range list {
			// skip debug symbols, and remove the debug symbol's data to save space
			if s.Kind >= goobj2.SDWARFINFO && s.Kind <= goobj2.SDWARFLINES {
				s.Size = 0
				s.Data = nil
				continue
			}

			// skip local asm symbols. For some reason garbling these breaks things
			// add the symbol name to a blacklist, so we don't garble related symbols
			// TODO: don't add duplicates
			if s.Kind == goobj2.SABIALIAS {
				if parts := strings.SplitN(s.Name, ".", 2); parts[0] == "main" {
					skipPrefixes = append(skipPrefixes, s.Name)
					skipPrefixes = append(skipPrefixes, `"".`+parts[1])
					continue
				}
			}

			// garble read-only static data, but not strings. If import paths are in string
			// symbols, that means garbling string symbols might effect the behavior of the
			// compiled binary
			if s.Kind == goobj2.SRODATA && s.Data != nil && !strings.HasPrefix(s.Name, "go.string.") {
				var dataTyp dataType
				if strings.HasPrefix(s.Name, "type..importpath.") {
					dataTyp = importPath
				} else if strings.HasPrefix(s.Name, "type..namedata.") {
					dataTyp = namedata
				}
				s.Data = garbleSymData(s.Data, privImports, garbledImports, dataTyp, buf)

				if s.Size != 0 {
					s.Size = uint32(len(s.Data))
				}
			}
			s.Name = garbleSymbolName(s.Name, privImports, garbledImports, sb)

			for i := range s.Reloc {
				s.Reloc[i].Name = garbleSymbolName(s.Reloc[i].Name, privImports, garbledImports, sb)
			}
			if s.Type != nil {
				s.Type.Name = garbleSymbolName(s.Type.Name, privImports, garbledImports, sb)
			}
			if s.Func != nil {
				for i := range s.Func.FuncData {
					s.Func.FuncData[i].Sym.Name = garbleSymbolName(s.Func.FuncData[i].Sym.Name, privImports, garbledImports, sb)
				}
				for _, inl := range s.Func.InlTree {
					inl.Func.Name = garbleSymbolName(inl.Func.Name, privImports, garbledImports, sb)
					if envGarbleTiny {
						inl.Line = 1
					}
				}

				if envGarbleTiny {
					s.Func.PCFile = nil
					s.Func.PCLine = nil
					s.Func.PCInline = nil
				}

				// remove unneeded debug aux symbols
				s.Func.DwarfInfo = nil
				s.Func.DwarfLoc = nil
				s.Func.DwarfRanges = nil
				s.Func.DwarfDebugLines = nil
			}
		}
	}
	for i := range am.SymRefs {
		am.SymRefs[i].Name = garbleSymbolName(am.SymRefs[i].Name, privImports, garbledImports, sb)
	}

	// remove dwarf file list, it isn't needed as we pass "-w, -s" to the linker
	am.DWARFFileList = nil
}

// garbleSymbolName finds all private imports in a symbol name, garbles them,
// and returns the modified symbol name.
func garbleSymbolName(symName string, privImports privateImports, garbledImports map[string]string, sb *strings.Builder) string {
	prefix, name, skipSym := splitSymbolPrefix(symName)
	if skipSym {
		// log.Printf("\t\t? Skipped symbol: %s", symName)
		return symName
	}

	// remove filename symbols when -tiny is passed
	// as they are only used for printing panics,
	// and -tiny removes panic printing
	if envGarbleTiny && prefix == "gofile.." {
		return prefix
	}

	var namedataSym bool
	if prefix == "type..namedata." {
		namedataSym = true
	}

	var off int
	for {
		o, l, privName := privateImportIndex(name[off:], privImports, namedataSym)
		if o == -1 {
			if sb.Len() != 0 {
				sb.WriteString(name[off:])
			}
			break
		}

		sb.WriteString(name[off : off+o])
		sb.WriteString(hashImport(name[off+o:off+o+l], privName.seed, garbledImports))
		off += o + l
	}

	if sb.Len() == 0 {
		// log.Printf("\t\t? Skipped symbol: %s", symName)
		return symName
	}
	defer sb.Reset()

	return prefix + sb.String()
}

// if symbols have one of these prefixes, skip
// garbling
// TODO: skip compiler generated/builtin symbols
var skipPrefixes = []string{
	// these symbols never contain import paths
	"gclocals.",
	"gclocals·",

	// string names be what they be
	"go.string.",

	// skip debug symbols
	"go.info.",

	// skip entrypoint symbols
	"main.init.",
	"main..stmp",
}

// symbols that are related to the entrypoint
// that cannot be garbled
var entrypointSyms = [...]string{
	"main.main",
	"main..inittask",
}

// if any of these strings are found in a
// symbol name, it should not be garbled
var skipSubstrs = [...]string{
	// skip test symbols
	"_test.",
}

// prefixes of symbols that we will garble,
// but we split the symbol name by one of
// these prefixes so that we do not
// accidentally garble an essential prefix
var symPrefixes = [...]string{
	"go.builtin.",
	"go.itab.",
	"go.itablink.",
	"go.interface.",
	"go.map.",
	"gofile..",
	"type..eq.",
	"type..eqfunc.",
	"type..hash.",
	"type..importpath.",
	"type..namedata.",
	"type.",
}

// splitSymbolPrefix returns the symbol name prefix Go uses
// to help designate the type of the symbol, and the rest of
// the symbol name. Additionally, a bool is returned that
// signifies whether garbling the symbol name should be skipped.
func splitSymbolPrefix(symName string) (string, string, bool) {
	if symName == "" {
		return "", "", true
	}

	for _, prefix := range skipPrefixes {
		if strings.HasPrefix(symName, prefix) {
			return "", "", true
		}
	}

	for _, entrySym := range entrypointSyms {
		if symName == entrySym {
			return "", "", true
		}
	}

	for _, substr := range skipSubstrs {
		if strings.Contains(symName, substr) {
			return "", "", true
		}
	}

	for _, prefix := range symPrefixes {
		if strings.HasPrefix(symName, prefix) {
			return symName[:len(prefix)], symName[len(prefix):], false
		}
	}

	return "", symName, false
}

// privateImportIndex returns the offset and length of a private import
// in symName. If no private imports from privImports are present in
// symName, -1, 0 is returned.
// TODO: it is possible that there could be multiple private names with
// the same name but different seeds, and currently the first name will
// always be returned. Not sure how to know which private name is correct
// in that case
func privateImportIndex(symName string, privImports privateImports, nameDataSym bool) (int, int, privateName) {
	matchPkg := func(pkg string) int {
		off := strings.Index(symName, pkg)
		if off == -1 {
			return -1
			// check that we didn't match inside an import path. If the
			// byte before the start of the match is not a small set of
			// symbols that can make up a symbol name, we must have matched
			// inside of an ident name as a substring. Or, if the byte
			// before the start of the match is a forward slash, we are
			// definitely inside of an input path.
		} else if off != 0 && (!isSymbol(symName[off-1]) || symName[off-1] == '/') {
			return -1
		}

		return off
	}

	var privName privateName
	firstOff, l := -1, 0
	for _, privatePkg := range privImports.privatePaths {
		off := matchPkg(privatePkg)
		if off == -1 {
			continue
		} else if off < firstOff || firstOff == -1 {
			firstOff = off
			l = len(privatePkg)
		}
	}

	if nameDataSym {
		for _, pName := range privImports.privateNames {
			// search for the package name plus a period, to
			// minimize the likelihood that the package isn't
			// matched as a substring of another ident name.
			// ex: pkgName = main, symName = "domainname"
			off := matchPkg(pName.name + ".")
			if off == -1 {
				continue
			} else if off < firstOff || firstOff == -1 {
				firstOff = off
				l = len(pName.name)
				privName = pName
			}
		}
	}

	return firstOff, l, privName
}

func isSymbol(c byte) bool {
	switch c {
	case ' ', '(', ')', '*', ',', '[', ']', '_', '{', '}':
		return true
	default:
		return false
	}
}

// garbleSymData finds all private imports in a symbol's data blob,
// garbles them, and returns the modified symbol data.
func garbleSymData(data []byte, privImports privateImports, garbledImports map[string]string, dataTyp dataType, buf *bytes.Buffer) []byte {
	var symData []byte
	switch dataTyp {
	case importPath:
		symData = data[3:]
	case namedata:
		oldNameLen := int(uint16(data[1])<<8 | uint16(data[2]))
		symData = data[3 : 3+oldNameLen]
	default:
		symData = data
	}

	var off int
	for {
		o, l, privName := privateImportIndex(string(symData[off:]), privImports, dataTyp == namedata)
		if o == -1 {
			if buf.Len() != 0 {
				buf.Write(symData[off:])
			}
			break
		}

		// there is only one import path in the symbol's data, garble it and return
		if dataTyp == importPath {
			return createImportPathData(hashImport(string(symData[o:o+l]), privName.seed, garbledImports))
		}

		buf.Write(symData[off : off+o])
		buf.WriteString(hashImport(string(symData[off+o:off+o+l]), privName.seed, garbledImports))
		off += o + l
	}

	if buf.Len() == 0 {
		return data
	}
	defer buf.Reset()

	if dataTyp == namedata {
		return patchReflectData(buf.Bytes(), data)
	}

	return buf.Bytes()
}

// createImportPathData creates reflection data for an
// import path
func createImportPathData(importPath string) []byte {
	l := 3 + len(importPath)
	b := make([]byte, l)
	b[0] = 0
	b[1] = uint8(len(importPath) >> 8)
	b[2] = uint8(len(importPath))
	copy(b[3:], importPath)

	return b
}

// patchReflectData replaces the name of a struct field or
// method in reflection namedata
func patchReflectData(newName []byte, data []byte) []byte {
	oldNameLen := int(uint16(data[1])<<8 | uint16(data[2]))

	data[1] = uint8(len(newName) >> 8)
	data[2] = uint8(len(newName))

	return append(data[:3], append(newName, data[3+oldNameLen:]...)...)
}

// garbleImportCfg writes a new importcfg with private import paths garbled.
func garbleImportCfg(path string, importCfg goobj2.ImportCfg, garbledImports, replacedFiles map[string]string) error {
	newCfg, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("error creating importcfg: %v", err)
	}
	defer newCfg.Close()
	newCfgWr := bufio.NewWriter(newCfg)

	for pkgPath, otherPath := range importCfg.ImportMap {
		if isPrivate(pkgPath) {
			pkgPath = hashImport(pkgPath, nil, garbledImports)
		}
		if isPrivate(otherPath) {
			otherPath = hashImport(otherPath, nil, garbledImports)
		}
		newCfgWr.WriteString("importmap ")
		newCfgWr.WriteString(pkgPath)
		newCfgWr.WriteByte('=')
		newCfgWr.WriteString(otherPath)
		newCfgWr.WriteByte('\n')
	}

	for pkgPath, info := range importCfg.Packages {
		if isPrivate(pkgPath) {
			pkgPath = hashImport(pkgPath, nil, garbledImports)
		}
		if info.IsSharedLib {
			newCfgWr.WriteString("packageshlib ")
		} else {
			newCfgWr.WriteString("packagefile ")
		}

		newCfgWr.WriteString(pkgPath)
		newCfgWr.WriteByte('=')
		if replaced := replacedFiles[info.Path]; replaced != "" {
			newCfgWr.WriteString(replaced)
		} else {
			newCfgWr.WriteString(info.Path)
		}
		newCfgWr.WriteByte('\n')
	}

	if err := newCfgWr.Flush(); err != nil {
		return fmt.Errorf("error writing importcfg: %v", err)
	}

	return nil
}
