package main

import (
	"go/build"
	"strings"
	"time"

	"github.com/tillberg/alog"
	"github.com/tillberg/stringset"
)

func getPackageImportsRecurse(pName string, target Target, all *stringset.StringSet, level int) (isCommand bool) {
	if beVeryVerbose() {
		t := alog.NewTimer()
		defer func() {
			durStr := t.FormatElapsedColor(100*time.Millisecond, 200*time.Millisecond)
			alog.Printf("@(dim:[)%s@(dim:]) getPackageImportsRecurse%s %s\n", durStr, strings.Repeat("+", level), pName)
		}()
	}
	// var buildContext build.Context = build.Default
	buildContext := build.Default
	if !target.OSArch.IsLocal() {
		buildContext.GOOS = target.OSArch.OS
		buildContext.GOARCH = target.OSArch.Arch
	}
	bPkg, err := buildContext.Import(pName, goPath, 0)
	if err != nil {
		// Ignore import errors on non-root packages; they should show as higher-level build failures
		if level == 0 {
			alog.Printf("@(warn:Error parsing package) @(dim:%s:) %s\n", pName, err.Error())
		}
		return false
	}
	if level == 0 && !bPkg.IsCommand() {
		return false
	}
	for _, importName := range bPkg.Imports {
		if isStandardPkg(importName) {
			continue
		}
		if all.Add(importName) {
			getPackageImportsRecurse(importName, target, all, level+1)
		}
	}
	return true
}

func getPackageImports(pName string, target Target) (deps *stringset.StringSet, isCommand bool) {
	all := stringset.New()
	isCommand = getPackageImportsRecurse(pName, target, all, 0)
	return all, isCommand
}
