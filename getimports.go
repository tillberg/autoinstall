package main

import (
	"go/build"

	"github.com/tillberg/stringset"
)

func getPackageImportsRecurse(pName string, all *stringset.StringSet, isRoot bool) (isCommand bool) {
	// var buildContext build.Context = build.Default
	bPkg, err := build.Default.Import(pName, goPath, 0)
	// Ignore import errors; they should show as higher-level build failures
	if err != nil {
		return false
	}
	if isRoot && !bPkg.IsCommand() {
		return false
	}
	for _, importName := range bPkg.Imports {
		if isStandardPkg(importName) {
			continue
		}
		if all.Add(importName) {
			getPackageImportsRecurse(importName, all, false)
		}
	}
	return true
}

func getPackageImports(pName string) (deps *stringset.StringSet, isCommand bool) {
	all := stringset.New()
	isCommand = getPackageImportsRecurse(pName, all, true)
	return all, isCommand
}
