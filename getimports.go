package main

import (
	"go/build"

	"github.com/tillberg/alog"
	"github.com/tillberg/stringset"
)

func getPackageImportsRecurse(pName string, osArch OSArch, all *stringset.StringSet, isRoot bool) (isCommand bool) {
	// var buildContext build.Context = build.Default
	buildContext := build.Default
	if !osArch.IsLocal() {
		buildContext.GOOS = osArch.OS
		buildContext.GOARCH = osArch.Arch
	}
	bPkg, err := buildContext.Import(pName, goPath, 0)
	if err != nil {
		// Ignore import errors on non-root packages; they should show as higher-level build failures
		if isRoot {
			alog.Printf("@(warn:Error parsing package) @(dim:%s:) %s\n", pName, err.Error())
		}
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
			getPackageImportsRecurse(importName, osArch, all, false)
		}
	}
	return true
}

func getPackageImports(pName string, osArch OSArch) (deps *stringset.StringSet, isCommand bool) {
	all := stringset.New()
	isCommand = getPackageImportsRecurse(pName, osArch, all, true)
	return all, isCommand
}
