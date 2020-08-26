package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"sort"
	"strings"

	"github.com/Binject/debug/goobj2"
)

type pkgInfo struct {
	pkg  *goobj2.Package
	path string
}

type dataType uint8

const (
	other dataType = iota
	importPath
	namedata
)

func obfuscateImports(objPath, importCfgPath string) error {
	importCfg, err := goobj2.ParseImportCfg(importCfgPath)
	if err != nil {
		return err
	}
	mainPkg, err := goobj2.Parse(objPath, "main", importCfg)
	if err != nil {
		return err
	}
	privatePkgs := []pkgInfo{{mainPkg, objPath}}

	// build list of imported packages that are private
	for pkgPath, info := range importCfg {
		if isPrivate(pkgPath) {
			pkg, err := goobj2.Parse(info.Path, pkgPath, importCfg)
			if err != nil {
				return fmt.Errorf("error parsing objfile %s at %s: %v", pkgPath, info.Path, err)
			}

			privatePkgs = append(privatePkgs, pkgInfo{pkg, info.Path})
		}
	}

	var sb strings.Builder
	var buf bytes.Buffer
	for _, p := range privatePkgs {
		fmt.Printf("\n\n++ Obfuscating object file for %s ++\n", p.pkg.ImportPath)
		for _, am := range p.pkg.ArchiveMembers {
			if am.IsCompilerObj() {
				continue
			}

			// add all private import paths to a list to garble
			var privateImports []string
			privateImports = append(privateImports, p.pkg.ImportPath)
			if strings.ContainsRune(p.pkg.ImportPath, '/') {
				privateImports = append(privateImports, importPathCombos(p.pkg.ImportPath)...)
			}
			for i := range am.Imports {
				if isPrivate(am.Imports[i].Pkg) {
					privateImports = append(privateImports, am.Imports[i].Pkg)
					if strings.ContainsRune(am.Imports[i].Pkg, '/') {
						privateImports = append(privateImports, importPathCombos(am.Imports[i].Pkg)...)
					}
					am.Imports[i].Pkg = hashImport(am.Imports[i].Pkg)
				}
			}
			for i := range am.Packages {
				if isPrivate(am.Packages[i]) {
					privateImports = append(privateImports, am.Packages[i])
					if strings.ContainsRune(am.Packages[i], '/') {
						privateImports = append(privateImports, importPathCombos(am.Packages[i])...)
					}
					am.Packages[i] = hashImport(am.Packages[i])
				}
			}

			// move imports that contain another import as a substring to the front,
			// so that the shorter import will not match first and leak part of an
			// import path
			sort.Slice(privateImports, func(i, j int) bool {
				iSlashes := strings.Count(privateImports[i], "/")
				jSlashes := strings.Count(privateImports[j], "/")
				if iSlashes == 0 && jSlashes == 0 {
					return privateImports[i] > privateImports[j]
				}
				return iSlashes > jSlashes
			})
			privateImports = dedupImportPaths(privateImports)

			// no private import paths, nothing to garble
			fmt.Printf("== Private imports: %v ==\n", privateImports)
			if len(privateImports) == 0 {
				continue
			}

			// garble all private import paths in all symbol names
			lists := [][]*goobj2.Sym{am.SymDefs, am.NonPkgSymDefs, am.NonPkgSymRefs}
			for _, list := range lists {
				for _, s := range list {
					// garble read only static data, but not strings. If import paths are in strings,
					// that means garbling strings might effect the behavior of the compiled binary
					if int(s.Kind) == 2 && s.Data != nil && !strings.HasPrefix(s.Name, "go.string.") {
						var dataTyp dataType
						if strings.HasPrefix(s.Name, "type..importpath.") {
							dataTyp = importPath
						} else if strings.HasPrefix(s.Name, "type..namedata.") {
							dataTyp = namedata
						}
						s.Data = garbleSymData(s.Data, privateImports, dataTyp, &buf)

						if s.Size != 0 {
							s.Size = uint32(len(s.Data))
						}
					}
					s.Name = garbleSymbolName(s.Name, privateImports, &sb)

					for i := range s.Reloc {
						s.Reloc[i].Name = garbleSymbolName(s.Reloc[i].Name, privateImports, &sb)
					}
					if s.Type != nil {
						s.Type.Name = garbleSymbolName(s.Type.Name, privateImports, &sb)
					}
					if s.Func != nil {
						for i := range s.Func.FuncData {
							s.Func.FuncData[i].Sym.Name = garbleSymbolName(s.Func.FuncData[i].Sym.Name, privateImports, &sb)
						}
						for _, inl := range s.Func.InlTree {
							inl.Func.Name = garbleSymbolName(inl.Func.Name, privateImports, &sb)
						}
					}
				}
			}
			for i := range am.SymRefs {
				am.SymRefs[i].Name = garbleSymbolName(am.SymRefs[i].Name, privateImports, &sb)
			}

			if err = goobj2.WriteObjFile2(p.pkg, p.path); err != nil {
				return err
			}
		}
	}

	// garble importcfg so the linker knows where to find garbled imports
	for pkgPath, info := range importCfg {
		if isPrivate(pkgPath) {
			pkgPath = hashImport(pkgPath)
		}
		if info.IsSharedLib {
			buf.WriteString("packageshlib")
		} else {
			buf.WriteString("packagefile")
		}

		buf.WriteRune(' ')
		buf.WriteString(pkgPath)
		buf.WriteRune('=')
		buf.WriteString(info.Path)
		buf.WriteRune('\n')
	}

	fmt.Print("\n\n")

	if err = ioutil.WriteFile(importCfgPath, buf.Bytes(), 0644); err != nil {
		return err
	}

	return nil
}

// importPathCombos returns a list of import paths that
// could all potentially be in symbol names of the
// package that imported 'path'.
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

func hashImport(pkg string) string {
	return hashWith(buildInfo.imports[pkg].buildID, pkg)
}

func garbleSymbolName(symName string, privateImports []string, sb *strings.Builder) (s string) {
	prefix, name, skipSym := splitSymbolPrefix(symName)
	if skipSym {
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
		sb.WriteString(hashImport(name[off+o : off+o+l]))
		off += o + l
	}

	if sb.Len() == 0 {
		return symName
	}
	defer sb.Reset()

	s = prefix + sb.String()

	return s
}

var skipPrefixes = [...]string{
	"gclocals·",
	"go.constinfo.",
	"go.cuinfo.",
	"go.info.",
	"go.string",
}

var symPrefixes = [...]string{
	"go.builtin.",
	"go.itab.",
	"go.itablink.",
	"go.interface.",
	"go.map.",
	"gofile..",
	"type.",
}

func splitSymbolPrefix(symName string) (string, string, bool) {
	if symName == "" {
		return "", "", true
	}

	for _, prefix := range skipPrefixes {
		if strings.HasPrefix(symName, prefix) {
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
	return c == 32 || // ' '
		(c >= 40 && c <= 42) || c == 44 || // '(', ')', '*', ','
		c == 91 || c == 93 || c == 95 || // '[', ']', '_'
		c == 123 || c == 125 // '{', '}'

}

func garbleSymData(data []byte, privateImports []string, dataTyp dataType, buf *bytes.Buffer) (b []byte) {
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
			return createImportPathData(hashImport(string(symData[o : o+l])))
		}

		buf.Write(symData[off : off+o])
		buf.WriteString(hashImport(string(symData[off+o : off+o+l])))
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
