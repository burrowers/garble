package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path"
	"sort"
	"strings"

	"github.com/Binject/debug/goobj2"
)

const (
	goFilePrefix = "gofile.."
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

func hashImport(pkg string) string {
	return hashWith(buildInfo.imports[pkg].buildID, pkg)
}

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
		fmt.Printf("++ Obfuscating object file for %s ++\n", p.pkg.ImportPath)
		for _, am := range p.pkg.ArchiveMembers {
			if am.IsCompilerObj() {
				continue
			}

			var privateImports []string
			privateImports = append(privateImports, p.pkg.ImportPath)
			if strings.ContainsRune(p.pkg.ImportPath, '/') {
				privateImports = append(privateImports, path.Base(p.pkg.ImportPath))
			}
			for i := range am.Imports {
				if isPrivate(am.Imports[i].Pkg) {
					am.Imports[i].Pkg = hashImport(am.Imports[i].Pkg)
				}
			}
			for i := range am.Packages {
				if isPrivate(am.Packages[i]) {
					privateImports = append(privateImports, am.Packages[i])
					if strings.ContainsRune(am.Packages[i], '/') {
						privateImports = append(privateImports, path.Base(am.Packages[i]))
					}
					am.Packages[i] = hashImport(am.Packages[i])
				}
			}
			// move imports that contain another import as a substring to the front,
			// so that the shorter import will not match first and leak part of an
			// import path
			sort.Slice(privateImports, func(i, j int) bool {
				return privateImports[i] > privateImports[j]
			})

			fmt.Printf("== Private imports: %v ==\n", privateImports)
			if len(privateImports) == 0 {
				continue
			}

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

	//fmt.Print("\n\n")

	if err = ioutil.WriteFile(importCfgPath, buf.Bytes(), 0644); err != nil {
		return err
	}

	return nil
}

func garbleSymbolName(symName string, privateImports []string, sb *strings.Builder) (s string) {
	var off int
	for {
		o, l := privateImportIndex(symName[off:], privateImports)
		if o == -1 {
			if sb.Len() != 0 {
				sb.WriteString(symName[off:])
			}
			break
		}

		sb.WriteString(symName[off : off+o])
		sb.WriteString(hashImport(symName[off+o : off+o+l]))
		off += o + l
	}

	if sb.Len() == 0 {
		return symName
	}

	s = sb.String()
	sb.Reset()

	//fmt.Printf("Garbled symbol: %s as %s\n", symName, s)

	return s
}

func privateImportIndex(symName string, privateImports []string) (int, int) {
	firstOff, l := -1, 0
	for _, privateImport := range privateImports {
		// search for the package name plus a period if the
		// package name doesn't have slashes, to minimize the
		// likelihood that the package isn't matched as a
		// substring of another ident name.
		// ex: privateImport = main, symName = "domainname"
		if !strings.ContainsRune(privateImport, '/') {
			privateImport += "."
		}

		off := strings.Index(symName, privateImport)
		if off == -1 {
			continue
		}

		if off < firstOff || firstOff == -1 {
			firstOff = off
			l = len(privateImport)
		}
	}

	if firstOff == -1 {
		return -1, 0
	}

	return firstOff, l
}

func garbleSymData(data []byte, privateImports []string, dataTyp dataType, buf *bytes.Buffer) (b []byte) {
	var off int
	for {
		o, l := privateImportIndex(string(data[off:]), privateImports)
		if o == -1 {
			if buf.Len() != 0 {
				buf.Write(data[off:])
			}
			break
		}

		switch dataTyp {
		case importPath:
			return createImportPathData(hashImport(string(data[o : o+l])))
		case namedata:
			return patchReflectData(hashImport(string(data[o:o+l])), o, data)
		default:
			buf.Write(data[off : off+o])
			buf.WriteString(hashImport(string(data[off+o : off+o+l])))
			off += o + l
		}

	}

	if buf.Len() == 0 {
		return data
	}

	b = buf.Bytes()
	buf.Reset()

	return b
}

func createImportPathData(importPath string) []byte {
	var bits byte
	l := 1 + 2 + len(importPath)
	b := make([]byte, l)
	b[0] = bits
	b[1] = uint8(len(importPath) >> 8)
	b[2] = uint8(len(importPath))
	copy(b[3:], importPath)

	return b
}

func patchReflectData(garbledImp string, off int, data []byte) []byte {
	oldNameLen := int(uint16(data[1])<<8 | uint16(data[2]))
	newName := string(data[3:off]) + garbledImp + string(data[off+len(garbledImp)-1:3+oldNameLen])

	data[1] = uint8(len(newName) >> 8)
	data[2] = uint8(len(newName))

	return append(data[:3], append([]byte(newName), data[3+oldNameLen:]...)...)
}
