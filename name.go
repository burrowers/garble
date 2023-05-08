package main

import (
	"go/types"
	"mvdan.cc/garble/internal/name"
)

var nameClient *name.Client

func setupNameClient() error {
	if nameServerAddr == "" {
		return nil
	}

	tmpNameClient, err := name.SetupClient(nameServerAddr)
	if err != nil {
		return err
	}
	nameClient = tmpNameClient
	return nil
}

func getObfuscatedImportPath(p *listedPackage) string {
	if nameClient == nil {
		return hashWithPackage(p, p.ImportPath)
	}
	return nameClient.GetPackageName(&name.PackageInfo{ImportPath: p.ImportPath})
}

func getObfuscatedFieldName(strct *types.Struct, fieldName string) string {
	if nameClient == nil {
		return hashWithStruct(strct, fieldName)
	}
	return nameClient.GetFieldName(&name.FieldInfo{Name: fieldName, StructIdentifier: strct.String()})
}
