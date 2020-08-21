package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
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
	for _, p := range pkgs {
		fmt.Printf("++ Obfuscating object file for %s ++\n", p.pkg.ImportPath)

		var privateImports []string
		if p.pkg.ImportPath != "main" && isPrivate(p.pkg.ImportPath) {
			privateImports = append(privateImports, p.pkg.ImportPath)
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
				s.Name = garbleSymbolName(s.Name, privateImports, &sb)
				/*s.Data = garbleSymData(s.Data, privateImports)
				if s.Size != 0 {
					s.Size = uint32(len(s.Data))
				}*/

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

	if err = goobj2.WriteObjFile2(pkgs[0].pkg, "/home/capnspacehook/Documents/obf_binclude.o"); err != nil {
		return err
	}

	var cfgBuf bytes.Buffer
	for pkgPath, info := range importCfg {
		if isPrivate(pkgPath) {
			pkgPath = hashWith("fakebuildID", pkgPath)
		}
		if info.IsSharedLib {
			cfgBuf.WriteString("packageshlib")
		} else {
			cfgBuf.WriteString("packagefile")
		}

		cfgBuf.WriteRune(' ')
		cfgBuf.WriteString(pkgPath)
		cfgBuf.WriteRune('=')
		cfgBuf.WriteString(info.Path)
		cfgBuf.WriteRune('\n')
	}

	fmt.Print("\n\n")

	if err = ioutil.WriteFile(importCfgPath, cfgBuf.Bytes(), 0644); err != nil {
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

	fmt.Printf("Garbled symbol: %s as %s\n", symName, s)

	return s
}

func privateImportIndex(symName string, privateImports []string) (int, int) {
	for _, privateImport := range privateImports {
		off := strings.Index(symName, privateImport)
		if off == -1 {
			continue
		}
		return off, len(privateImport)
	}

	return -1, 0
}

func garbleSymData(data []byte, privateImports []string) []byte {
	off := -1
	for _, privateImport := range privateImports {
		off = bytes.Index(data, []byte(privateImport))
		if off == -1 {
			continue
		}

		l := len(privateImport)
		garbled := hashWith("fakebuildID", string(data[off:off+l]))
		data = append(data[:off], append([]byte(garbled), data[off+l:]...)...)
	}

	return data
}
