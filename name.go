package main

import (
	"go/types"
	"mvdan.cc/garble/internal/name"
)

// TODO: Merge to interface?
var (
	getObfuscatedImportPath func(*listedPackage) string
	getObfuscatedFieldName  func(*types.Struct, string) string
	getObfuscatedName       func(*listedPackage, string) string
	getObfuscatedFile       func(*listedPackage, string) string
	blacklistName           func(string)
)

func setupNameGenerator() error {
	if nameServerAddr == "" {
		getObfuscatedImportPath = func(p *listedPackage) string {
			return hashWithPackage(p, p.ImportPath)
		}
		getObfuscatedFieldName = func(strct *types.Struct, fieldName string) string {
			return hashWithStruct(strct, fieldName)
		}
		getObfuscatedName = func(p *listedPackage, name string) string {
			return hashWithPackage(p, name)
		}
		getObfuscatedFile = getObfuscatedName
		return nil
	}

	nameGenerator, err := name.ConnectClient(nameServerAddr)
	if err != nil {
		return err
	}

	getObfuscatedImportPath = func(p *listedPackage) string {
		return nameGenerator.GetName(&name.Info{
			Type: name.Package,
			Name: p.ImportPath,
		})
	}
	getObfuscatedFieldName = func(strct *types.Struct, fieldName string) string {
		return nameGenerator.GetName(&name.Info{
			Type:            name.Field,
			ScopeIdentifier: strct.String(),
			Name:            fieldName,
		})
	}
	getObfuscatedName = func(p *listedPackage, n string) string {
		return nameGenerator.GetName(&name.Info{
			Type:            name.Name,
			ScopeIdentifier: p.ImportPath,
			Name:            n,
		})
	}
	getObfuscatedFile = func(p *listedPackage, n string) string {
		return nameGenerator.GetName(&name.Info{
			Type:            name.File,
			ScopeIdentifier: p.ImportPath,
			Name:            n,
		})
	}
	blacklistName = func(s string) {
		nameGenerator.BlacklistName(s)
	}
	return nil
}
