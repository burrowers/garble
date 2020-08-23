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

func obfuscateImports(objPath, importCfgPath string) error {
	importCfg, err := goobj2.ParseImportCfg(importCfgPath)
	if err != nil {
		return err
	}
	mainPkg, err := goobj2.Parse(objPath, "main", importCfg)
	if err != nil {
		return err
	}
	pkgs := []pkgInfo{{mainPkg, objPath}}

	for pkgPath, info := range importCfg {
		if isPrivate(pkgPath) {
			pkg, err := goobj2.Parse(info.Path, pkgPath, importCfg)
			if err != nil {
				return err
			}
			pkgs = append(pkgs, pkgInfo{pkg, info.Path})
		}
	}

	var sb strings.Builder
	var buf bytes.Buffer
	for _, p := range pkgs {
		fmt.Printf("++ Obfuscating object file for %s ++\n", p.pkg.ImportPath)

		var privateImports []string
		if p.pkg.ImportPath != "main" && isPrivate(p.pkg.ImportPath) {
			privateImports = append(privateImports, p.pkg.ImportPath)
			if strings.ContainsRune(p.pkg.ImportPath, '/') {
				privateImports = append(privateImports, path.Base(p.pkg.ImportPath))
			}
		}
		for i := range p.pkg.Imports {
			if isPrivate(p.pkg.Imports[i].Pkg) {
				p.pkg.Imports[i].Pkg = hashWith("fakebuildID", p.pkg.Imports[i].Pkg)
			}
		}
		for i := range p.pkg.Packages {
			if isPrivate(p.pkg.Packages[i]) {
				privateImports = append(privateImports, p.pkg.Packages[i])
				p.pkg.Packages[i] = hashWith("fakebuildID", p.pkg.Packages[i])
			}
		}
		// move imports that contain another import as a substring to the front,
		// so that the shorter import will not match first and leak part of an
		// import path
		sort.Slice(privateImports, func(i, j int) bool {
			if strings.Contains(privateImports[i], privateImports[j]) {
				return true
			}
			return false
		})

		fmt.Printf("== Private imports: %v ==\n", privateImports)
		if len(privateImports) == 0 {
			continue
		}

		lists := [][]*goobj2.Sym{p.pkg.SymDefs, p.pkg.NonPkgSymDefs, p.pkg.NonPkgSymRefs}
		for _, list := range lists {
			for _, s := range list {
				if int(s.Kind) == 2 && s.Data != nil { // read only static data
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
					for _, inl := range s.Func.InlTree {
						inl.Func.Name = garbleSymbolName(inl.Func.Name, privateImports, &sb)
					}
				}
			}
		}
		for i := range p.pkg.SymRefs {
			p.pkg.SymRefs[i].Name = garbleSymbolName(p.pkg.SymRefs[i].Name, privateImports, &sb)
		}

		if err = goobj2.WriteObjFile2(p.pkg, p.path); err != nil {
			return err
		}
	}

	/*if err = goobj2.WriteObjFile2(pkgs[0].pkg, "/home/capnspacehook/Documents/obf_binclude.o"); err != nil {
		return err
	}*/

	for pkgPath, info := range importCfg {
		if isPrivate(pkgPath) {
			pkgPath = hashWith("fakebuildID", pkgPath)
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
		sb.WriteString(hashWith("fakebuildID", symName[off+o:off+o+l]))
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
			return createImportPathData(hashWith("fakebuildID", string(data[o:o+l])))
		case namedata:
			return patchReflectData(hashWith("fakebuildID", string(data[o:o+l])), l, data)
		default:
			buf.Write(data[off : off+o])
			buf.WriteString(hashWith("fakebuildID", string(data[off+o:off+o+l])))
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

func patchReflectData(name string, oldNameLen int, data []byte) []byte {
	data[1] = uint8(len(name) >> 8)
	data[2] = uint8(len(name))

	return append(data[:3], append([]byte(name), data[3+oldNameLen:]...)...)
}
