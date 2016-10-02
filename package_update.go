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
		pUpdate.UpdateError = err
		// If the directory no longer exists, then tell the dispatcher to remove this package from the index
		_, statErr := os.Stat(absPkgPath)
		if statErr != nil && os.IsNotExist(statErr) {
			pUpdate.RemovePackage = true
		} else if beVerbose() {
			alog.Printf("@(warn:Error parsing import of module %s: %s)\n", pkgName, err)
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
			modTime := fileinfo.ModTime()
			if modTime.After(pUpdate.UpdateStartTime) {
				if modTime.After(time.Now()) {
					alog.Printf("@(warn:File has future modification time: %q mod %s)\n", path, modTime.String())
					alog.Printf("@(warn:Correct triggering of builds depends on correctly-set system clocks.)\n")
					// Assume that it was not actually modified in the future, but that the system clock is just wrong
					// This will allow us to build the package, but we'll keep re-building every time autoinstall
					// restarts until the system clock gets past the file's time.
					modTime = pUpdate.UpdateStartTime.Add(-1 * time.Microsecond)
				}
			}
			if modTime.After(pUpdate.SourceModTime) {
				pUpdate.SourceModTime = modTime
				pUpdate.RecentSrcName = fileinfo.Name()
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
	p.RecentSrcName = pUpdate.RecentSrcName
	p.UpdateStartTime = pUpdate.UpdateStartTime
	p.IsProgram = pUpdate.IsProgram
	p.WasUpdated = true
}
