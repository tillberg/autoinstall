package main

import (
	"go/build"
	"os"
	"path/filepath"
	"time"

	"github.com/tillberg/ansi-log"
	"github.com/tillberg/stringset"
)

func update(pkgName string) {
	pUpdate := NewPackage(pkgName)
	defer func() {
		updateFinished <- pUpdate
	}()
	pUpdate.UpdateStartTime = time.Now()

	absPkgPath := filepath.Join(srcRoot, pkgName)
	pkg, err := build.ImportDir(absPkgPath, build.ImportComment)
	if err != nil {
		if beVerbose() {
			alog.Printf("@(error:Error parsing import of module %s: %s)\n", pkgName, err)
		}
		pUpdate.UpdateError = err
		// If the directory no longer exists, then tell the dispatcher to remove this package from the index
		_, err := os.Stat(absPkgPath)
		if err != nil && os.IsNotExist(err) {
			pUpdate.RemovePackage = true
		}
		return
	}
	pUpdate.Imports = stringset.New()
	for _, importName := range pkg.Imports {
		if !goStdLibPackages.Has(importName) {
			pUpdate.Imports.Add(importName)
		}
	}
	statFiles := func(files []string) {
		for _, filename := range files {
			path := filepath.Join(absPkgPath, filename)
			fileinfo, err := os.Stat(path)
			if err != nil {
				alog.Printf("@(error:Error stat-ing %s: %v)\n", path, err)
				pUpdate.UpdateError = err
				return
			}
			if fileinfo.ModTime().After(pUpdate.SourceModTime) {
				pUpdate.SourceModTime = fileinfo.ModTime()
			}
		}
	}
	statFiles(pkg.GoFiles)
	statFiles(pkg.CgoFiles)
	statFiles(pkg.CFiles)
	statFiles(pkg.CXXFiles)
	statFiles(pkg.MFiles)
	statFiles(pkg.HFiles)
	statFiles(pkg.SFiles)
	statFiles(pkg.SwigFiles)
	statFiles(pkg.SwigCXXFiles)
	statFiles(pkg.SysoFiles)

	pUpdate.IsProgram = pkg.Name == "main"

	targetPath := pUpdate.getAbsTargetPath()
	fileinfo, err := os.Stat(targetPath)
	if err != nil && !os.IsNotExist(err) {
		alog.Printf("@(error:Error stat-ing %s: %v)\n", targetPath, err)
		pUpdate.UpdateError = err
		return
	} else if err == nil {
		pUpdate.BuiltModTime = fileinfo.ModTime()
	}
}

func (p *Package) mergeUpdate(pUpdate *Package) {
	p.Imports = pUpdate.Imports
	p.BuiltModTime = pUpdate.BuiltModTime
	p.SourceModTime = pUpdate.SourceModTime
	p.UpdateStartTime = pUpdate.UpdateStartTime
	p.IsProgram = pUpdate.IsProgram
	p.WasUpdated = true
}
