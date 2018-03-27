package main

import (
	"go/build"

	"github.com/tillberg/stringset"
)

func getPackageImportsRecurse(pName string, all *stringset.StringSet) (isCommand bool) {
	bPkg, err := build.Default.Import(pName, goPath, 0)
	// Ignore import errors; they should show as higher-level build failures
	if err != nil {
		return false
	}
	for _, importName := range bPkg.Imports {
		if isStandardPkg(importName) {
			continue
		}
		if all.Add(importName) {
			getPackageImportsRecurse(importName, all)
		}
	}
	return bPkg.IsCommand()
}

func getPackageImports(pName string) (deps *stringset.StringSet, isCommand bool) {
	all := stringset.New()
	isCommand = getPackageImportsRecurse(pName, all)
	return all, isCommand
}
