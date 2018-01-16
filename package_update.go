package main

import (
	"crypto/sha1"
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"time"

	"github.com/tillberg/alog"
	"github.com/tillberg/autoinstall/buildid"
	"github.com/tillberg/stringset"
)

func update(pkgName string) {
	pUpdate := NewPackage(pkgName)
	defer func() {
		updateFinished <- pUpdate
	}()
	pUpdate.UpdateStartTime = time.Now()
	pUpdate.IsStandard = isGoStdLibPackage(pkgName)
	pUpdate.Imports = stringset.New()
	absPkgPath := pUpdate.getAbsSrcPath()
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
	for _, importName := range pkg.Imports {
		pUpdate.Imports.Add(importName)
	}
	if len(pkg.CgoFiles) > 0 {
		pUpdate.Imports.Add("runtime/cgo")
		pUpdate.Imports.Add("syscall")
	}
	sourceFileSlices := [][]string{
		pkg.GoFiles,
		pkg.CgoFiles,
		pkg.CFiles,
		pkg.CXXFiles,
		pkg.MFiles,
		pkg.HFiles,
		pkg.SFiles,
		pkg.SysoFiles,
		pkg.SwigFiles,
		pkg.SwigCXXFiles,
	}
	for _, files := range sourceFileSlices {
		for _, file := range files {
			pUpdate.AllSources = append(pUpdate.AllSources, file)
		}
	}

	if pUpdate.IsStandard {
		pUpdate.IsProgram = false
	} else {
		pUpdate.Imports.Add("runtime")
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
				if modTime.After(pUpdate.UpdateStartTime) && modTime.After(time.Now()) {
					alog.Printf("@(warn:File has future modification time: %q mod %s)\n", path, modTime.String())
					alog.Printf("@(warn:Correct triggering of builds depends on correctly-set system clocks.)\n")
					// Assume that it was not actually modified in the future, but that the system clock is just wrong
					// This will allow us to build the package, but we'll keep re-building every time autoinstall
					// restarts until the system clock gets past the file's time.
					modTime = pUpdate.UpdateStartTime.Add(-1 * time.Microsecond)
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
		if shouldRunTests() {
			statFiles(pkg.TestGoFiles)
		}

		if len(pkg.TestGoFiles) > 0 {
			pUpdate.HasTests = true
		}
		pUpdate.IsProgram = pkg.Name == "main"
	}

	targetPath := pUpdate.getAbsTargetPath()
	// alog.Println(targetPath)
	fileinfo, err := os.Stat(targetPath)
	if err != nil && !os.IsNotExist(err) {
		alog.Printf("@(error:Error stat-ing %s: %v)\n", targetPath, err)
		pUpdate.UpdateError = err
		return
	} else if err == nil {
		pUpdate.TargetModTime = fileinfo.ModTime()
		var err error
		pUpdate.CurrentBuildID, err = buildid.ReadBuildID(pUpdate.IsProgram, targetPath)
		if err != nil {
			alog.Printf("@(error:Error reading BuildID for %s: %v)\n", pkgName, err)
			err = os.Remove(targetPath)
			if err != nil {
				alog.Printf("@(error:Failed to remove %s: %v)\n", targetPath, err)
			} else {
				alog.Printf("@(warn:Removed %s after failing to read BuildID)\n", targetPath)
			}
		}
		// alog.Println(pUpdate.CurrentBuildID)
	}
}

func (p *Package) mergeUpdate(pUpdate *Package) {
	p.Imports = pUpdate.Imports
	p.TargetModTime = pUpdate.TargetModTime
	p.SourceModTime = pUpdate.SourceModTime
	p.RecentSrcName = pUpdate.RecentSrcName
	p.UpdateStartTime = pUpdate.UpdateStartTime
	p.IsProgram = pUpdate.IsProgram
	p.HasTests = pUpdate.HasTests
	p.AllSources = pUpdate.AllSources
	p.CurrentBuildID = pUpdate.CurrentBuildID
	p.IsStandard = pUpdate.IsStandard
	p.WasUpdated = true
	if p.IsStandard {
		p.State = PackageReady
		if p.CurrentBuildID == "" {
			// The "unsafe" package doesn't have a `.a` file, so the computedBuildID is used instead
			p.computeDesiredBuildID(nil)
			p.CurrentBuildID = p.DesiredBuildID
		}
		p.DesiredBuildID = p.CurrentBuildID
	}
	if len(p.AllSources) == 0 {
		p.State = PackageReady
		p.DesiredBuildID = p.CurrentBuildID
	}
}

// computeBuildID is borrowed from https://github.com/golang/go/blob/13c35a1b204f6e580b220e0df409a2c186e648a4/src/cmd/go/internal/load/pkg.go#L1622-L1670
func (p *Package) computeDesiredBuildID(deps DepSlice) {
	h := sha1.New()

	// Include the list of files compiled as part of the package.
	// This lets us detect removed files. See issue 3895.
	for _, file := range p.AllSources {
		fmt.Fprintf(h, "file %s\n", file)
	}

	// Include the content of runtime/internal/sys/zversion.go in the hash
	// for package runtime. This will give package runtime a
	// different build ID in each Go release.
	// if p.Standard && p.ImportPath == "runtime/internal/sys" && cfg.BuildContext.Compiler != "gccgo" {
	// 	data, err := ioutil.ReadFile(filepath.Join(p.Dir, "zversion.go"))
	// 	if err != nil {
	// 		base.Fatalf("go: %s", err)
	// 	}
	// 	fmt.Fprintf(h, "zversion %q\n", string(data))
	// }

	// Include the build IDs of any dependencies in the hash.
	// This, combined with the runtime/zversion content,
	// will cause packages to have different build IDs when
	// compiled with different Go releases.
	// This helps the go command know to recompile when
	// people use the same GOPATH but switch between
	// different Go releases. See issue 10702.
	// This is also a better fix for issue 8290.
	for _, dep := range deps {
		fmt.Fprintf(h, "dep %s %s\n", dep.Name, dep.BuildID)
	}

	p.DesiredBuildID = fmt.Sprintf("%x", h.Sum(nil))
}
