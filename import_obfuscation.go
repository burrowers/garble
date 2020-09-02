package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Binject/debug/goobj2"
)

// pkgInfo stores a parsed go archive/object file,
// and the original path to which it was read from.
type pkgInfo struct {
	pkg  *goobj2.Package
	path string
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

func obfuscateImports(objPath, importCfgPath string) (map[string]string, error) {
	importCfg, err := goobj2.ParseImportCfg(importCfgPath)
	if err != nil {
		return nil, err
	}
	mainPkg, err := goobj2.Parse(objPath, "main", importCfg)
	if err != nil {
		return nil, fmt.Errorf("error parsing main objfile: %v", err)
	}
	privatePkgs := []pkgInfo{{mainPkg, objPath}}

	// build list of imported packages that are private
	for pkgPath, info := range importCfg {
		if isPrivate(pkgPath) {
			pkg, err := goobj2.Parse(info.Path, pkgPath, importCfg)
			if err != nil {
				return nil, fmt.Errorf("error parsing objfile %s at %s: %v", pkgPath, info.Path, err)
			}

			privatePkgs = append(privatePkgs, pkgInfo{pkg, info.Path})
		}
	}

	var sb strings.Builder
	var buf bytes.Buffer
	garbledImports := make(map[string]string)
	for _, p := range privatePkgs {
		// log.Printf("++ Obfuscating object file for %s ++", p.pkg.ImportPath)
		for _, am := range p.pkg.ArchiveMembers {
			// log.Printf("\t## Obfuscating archive member %s ##", am.ArchiveHeader.Name)
			// skip objects that are not used by the linker, or that do not contain
			// any Go symbol info
			if am.IsCompilerObj() || am.IsDataObj() {
				continue
			}

			// remove dwarf file list, it isn't needed as we pass "-w, -s" to the linker
			am.DWARFFileList = nil

			// add all private import paths to a list to garble
			var privateImports []string
			privateImports = append(privateImports, p.pkg.ImportPath)
			if strings.ContainsRune(p.pkg.ImportPath, '/') {
				privateImports = append(privateImports, importPathCombos(p.pkg.ImportPath)...)
			}

			initImport := func(imp string) string {
				if !isPrivate(imp) {
					return imp
				}

				privateImports = append(privateImports, imp)
				if strings.ContainsRune(imp, '/') {
					privateImports = append(privateImports, importPathCombos(imp)...)
				}
				return hashImport(imp, garbledImports)
			}

			for i := range am.Imports {
				am.Imports[i].Pkg = initImport(am.Imports[i].Pkg)
			}
			for i := range am.Packages {
				am.Packages[i] = initImport(am.Packages[i])
			}

			// move imports that contain another import as a substring to the front,
			// so that the shorter import will not match first and leak part of an
			// import path
			sort.Slice(privateImports, func(i, j int) bool {
				iSlashes := strings.Count(privateImports[i], "/")
				jSlashes := strings.Count(privateImports[j], "/")
				// sort by number of slashes first, then alphabetically
				if iSlashes == jSlashes {
					return privateImports[i] > privateImports[j]
				}
				return iSlashes > jSlashes
			})
			privateImports = dedupImportPaths(privateImports)

			// no private import paths, nothing to garble
			// log.Printf("\t== Private imports: %v ==\n", privateImports)
			if len(privateImports) == 0 {
				continue
			}

			// garble all private import paths in all symbol names
			lists := [][]*goobj2.Sym{am.SymDefs, am.NonPkgSymDefs, am.NonPkgSymRefs}
			for _, list := range lists {
				for _, s := range list {
					// skip debug symbols, and remove the debug symbol's data to save space
					if s.Kind >= goobj2.SDWARFINFO && s.Kind <= goobj2.SDWARFLINES {
						s.Data = nil
						continue
					}

					// skip local asm symbols. For some reason garbling these breaks things
					// add the symbol name to a blacklist, so we don't garble related symbols
					if s.Kind == goobj2.SABIALIAS {
						if parts := strings.SplitN(s.Name, ".", 2); parts[0] == "main" {
							skipPrefixes = append(skipPrefixes, s.Name)
							skipPrefixes = append(skipPrefixes, `"".`+parts[1])
							continue
						}
					}

					// garble read only static data, but not strings. If import paths are in strings,
					// that means garbling strings might effect the behavior of the compiled binary
					if s.Kind == goobj2.SRODATA && s.Data != nil && !strings.HasPrefix(s.Name, "go.string.") {
						var dataTyp dataType
						if strings.HasPrefix(s.Name, "type..importpath.") {
							dataTyp = importPath
						} else if strings.HasPrefix(s.Name, "type..namedata.") {
							dataTyp = namedata
						}
						s.Data = garbleSymData(s.Data, privateImports, garbledImports, dataTyp, &buf)

						if s.Size != 0 {
							s.Size = uint32(len(s.Data))
						}
					}
					s.Name = garbleSymbolName(s.Name, privateImports, garbledImports, &sb)

					for i := range s.Reloc {
						s.Reloc[i].Name = garbleSymbolName(s.Reloc[i].Name, privateImports, garbledImports, &sb)
					}
					if s.Type != nil {
						s.Type.Name = garbleSymbolName(s.Type.Name, privateImports, garbledImports, &sb)
					}
					if s.Func != nil {
						for i := range s.Func.FuncData {
							s.Func.FuncData[i].Sym.Name = garbleSymbolName(s.Func.FuncData[i].Sym.Name, privateImports, garbledImports, &sb)
						}
						for _, inl := range s.Func.InlTree {
							inl.Func.Name = garbleSymbolName(inl.Func.Name, privateImports, garbledImports, &sb)
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
				am.SymRefs[i].Name = garbleSymbolName(am.SymRefs[i].Name, privateImports, garbledImports, &sb)
			}
		}

		if err := p.pkg.Write(p.path); err != nil {
			return nil, fmt.Errorf("error writing objfile %s at %s: %v", p.pkg.ImportPath, p.path, err)
		}
	}

	// garble importcfg so the linker knows where to find garbled imports
	newCfg, err := os.Create(importCfgPath)
	if err != nil {
		return nil, fmt.Errorf("error creating importcfg: %v", err)
	}
	defer newCfg.Close()
	newCfgWr := bufio.NewWriter(newCfg)

	for pkgPath, info := range importCfg {
		if isPrivate(pkgPath) {
			pkgPath = hashImport(pkgPath, garbledImports)
		}
		if info.IsSharedLib {
			newCfgWr.WriteString("packageshlib")
		} else {
			newCfgWr.WriteString("packagefile")
		}

		newCfgWr.WriteRune(' ')
		newCfgWr.WriteString(pkgPath)
		newCfgWr.WriteRune('=')
		newCfgWr.WriteString(info.Path)
		newCfgWr.WriteRune('\n')
	}

	if err := newCfgWr.Flush(); err != nil {
		return nil, fmt.Errorf("error writing importcfg: %v", err)
	}

	return garbledImports, nil
}

// importPathCombos returns a list of import paths that
// could all potentially be in symbol names of the
// package that imported 'path'.
// TODO: last element returned should get same buildID
// as full path?
// ie github.com/foo/bar.buildID == bar.buildID
func importPathCombos(path string) []string {
	paths := strings.Split(path, "/")
	combos := make([]string, 0, len(paths))

	var restPrivate bool
	if isPrivate(paths[0]) {
		combos = append(combos, paths[0])
		restPrivate = true
	}

	// find first private match
	privateIdx := 1
	if !restPrivate {
		newPath := paths[0]
		for i := 1; i < len(paths); i++ {
			newPath += "/" + paths[i]
			if isPrivate(newPath) {
				combos = append(combos, paths[i])
				combos = append(combos, newPath)

				privateIdx = i + 1
				restPrivate = true
				break
			}
		}

		if !restPrivate {
			return nil
		}
	}

	lastComboIdx := 2
	for i := privateIdx; i < len(paths)-1; i++ {
		combos = append(combos, paths[i])
		combos = append(combos, combos[lastComboIdx-1]+"/"+paths[i])
		lastComboIdx += 2
	}
	combos = append(combos, paths[len(paths)-1])

	return combos
}

func dedupImportPaths(paths []string) []string {
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

// TODO: possible that package collisions can occur; for instance, if
// 'github.com/thingy/foo' and 'bar/baz/foo/zip' were both private imports
// of the same object, 'foo' would be added to as a private import
// twice, due to the logic of importPathCombos(). There needs to be some
// way to differentiate between 'foo' of 'github.com/thingy/foo' and
// 'bar/baz/foo/zip' so the same buildID is not used, which would create
// an identical hash.
func hashImport(pkg string, garbledImports map[string]string) string {
	if garbledPkg, ok := garbledImports[pkg]; ok {
		return garbledPkg
	}

	garbledPkg := hashWith(buildInfo.imports[pkg].buildID, pkg)
	garbledImports[pkg] = garbledPkg

	return garbledPkg
}

// garbleSymbolName finds all private imports in a symbol name, garbles them,
// and returns the modified symbol name.
func garbleSymbolName(symName string, privateImports []string, garbledImports map[string]string, sb *strings.Builder) string {
	prefix, name, skipSym := splitSymbolPrefix(symName)
	if skipSym {
		// log.Printf("\t\t? Skipped symbol: %s", symName)
		return symName
	}

	var off int
	for {
		o, l := privateImportIndex(name[off:], privateImports)
		if o == -1 {
			if sb.Len() != 0 {
				sb.WriteString(name[off:])
			}
			break
		}

		sb.WriteString(name[off : off+o])
		sb.WriteString(hashImport(name[off+o:off+o+l], garbledImports))
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
// in symName. If no private imports from privateImports are present in
// symName, -1, 0 is returned.
func privateImportIndex(symName string, privateImports []string) (int, int) {
	firstOff, l := -1, 0
	for _, privateImport := range privateImports {
		// search for the package name plus a period if the
		// package name doesn't have slashes, to minimize the
		// likelihood that the package isn't matched as a
		// substring of another ident name.
		// ex: privateImport = main, symName = "domainname"
		var noSlashes bool
		if !strings.ContainsRune(privateImport, '/') {
			privateImport += "."
			noSlashes = true
		}

		off := strings.Index(symName, privateImport)
		if off == -1 {
			continue
			// check that we didn't match inside an import path. If the
			// byte before the start of the match is not a small set of
			// symbols that can make up a symbol name, we must have matched
			// inside of an ident name as a substring. Or, if the byte
			// before the start of the match is a forward slash, we are
			// definitely inside of an input path.
		} else if off != 0 && (!isSymbol(symName[off-1]) || symName[off-1] == '/') {
			continue
		}

		if off < firstOff || firstOff == -1 {
			firstOff = off
			l = len(privateImport)
			if noSlashes {
				l--
			}
		}
	}

	if firstOff == -1 {
		return -1, 0
	}

	return firstOff, l
}

func isSymbol(c byte) bool {
	switch c {
	case ' ', '(', ')', '*', ',', '[', ']', '_', '{', '}':
		return true
	default:
		return false
	}
}

// garbleSymData finds all private imports a symbol's data, garbles them, and
// returns the modified symbol data.
func garbleSymData(data []byte, privateImports []string, garbledImports map[string]string, dataTyp dataType, buf *bytes.Buffer) []byte {
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
		o, l := privateImportIndex(string(symData[off:]), privateImports)
		if o == -1 {
			if buf.Len() != 0 {
				buf.Write(symData[off:])
			}
			break
		}

		if dataTyp == importPath {
			return createImportPathData(hashImport(string(symData[o:o+l]), garbledImports))
		}

		buf.Write(symData[off : off+o])
		buf.WriteString(hashImport(string(symData[off+o:off+o+l]), garbledImports))
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

func createImportPathData(importPath string) []byte {
	l := 3 + len(importPath)
	b := make([]byte, l)
	b[0] = 0
	b[1] = uint8(len(importPath) >> 8)
	b[2] = uint8(len(importPath))
	copy(b[3:], importPath)

	return b
}

func patchReflectData(newName []byte, data []byte) []byte {
	oldNameLen := int(uint16(data[1])<<8 | uint16(data[2]))

	data[1] = uint8(len(newName) >> 8)
	data[2] = uint8(len(newName))

	return append(data[:3], append(newName, data[3+oldNameLen:]...)...)
}
