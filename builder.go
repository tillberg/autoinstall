package main

import (
	"fmt"
	"go/build"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tillberg/ansi-log"
	"github.com/tillberg/bismuth"
)

type builder struct {
	ctx *bismuth.ExecContext
}

func newBuilder() *builder {
	me := &builder{}
	me.ctx = bismuth.NewExecContext()
	me.ctx.Connect()
	return me
}

func (b *builder) buildModule(moduleName string) {
	defer func() {
		builders <- b
	}()
	abort := func() {
		moduleStateMutex.Lock()
		currState := moduleState[moduleName]
		moduleState[moduleName] = ModuleDirtyIdle
		moduleStateMutex.Unlock()
		if currState == ModuleBuildingButDirty {
			moduleTriggerChan <- moduleName
		}
	}
	depsAreReady := func(includeTests bool) bool {
		missingDeps := listMissingDependencies(moduleName, includeTests)
		if missingDeps == nil || len(missingDeps) > 0 {
			// If this module is missing any up-to-date dependecies, send
			// it to the end of the queue after a brief pause
			if missingDeps != nil && beVerbose() {
				etAlStr := ""
				if len(missingDeps) > 1 {
					pluralStr := "s"
					if len(missingDeps) == 2 {
						pluralStr = ""
					}
					etAlStr = alog.Colorify(fmt.Sprintf("@(dim:, and) %d @(dim:other%s)", len(missingDeps)-1, pluralStr))
				}
				verb := "building"
				if includeTests {
					verb = "testing"
				}
				alog.Printf("@(dim:Not %s) %s@(dim:;) %s @(dim:not ready)%s@(dim:.)\n", verb, moduleName, missingDeps[0], etAlStr)
			}
			return false
		}
		return true
	}

	moduleStateMutex.Lock()
	moduleState[moduleName] = ModuleBuilding
	moduleStateMutex.Unlock()
	buildStartTime := time.Now()
	timer := alog.NewTimer()
	// Check that all of this module's dependencies are built. If not, abort this build and send it to the back of the queue.
	if !depsAreReady(false) {
		abort()
		return
	}
	ctx := b.ctx
	// Just in case it gets deleted for some reason:
	tmpdir := os.Getenv("TMPDIR")
	if tmpdir != "" {
		os.MkdirAll(tmpdir, 0700)
	}
	absPath := filepath.Join(srcRoot, moduleName)

	pkg, err := build.ImportDir(absPath, build.ImportComment)
	if err != nil {
		alog.Printf("@(error:Error reading package name of) %s@(error::) %v", moduleName, err)
		abort()
		return
	}
	// alog.Printf("Package name of %s: %s\n", moduleName, pkg.Name)
	buildTargetPath := ""
	if pkg.Name == "main" {
		// XXX This is too simple, as there is some special casing for go tools at
		// https://github.com/golang/go/blob/003a68bc7fcb917b5a4d92a5c2244bb1adf8f690/src/cmd/go/pkg.go#L693-L715
		buildTargetPath = filepath.Join(pkg.BinDir, filepath.Base(moduleName))
	} else {
		pkgArch := fmt.Sprintf("%s_%s", build.Default.GOOS, build.Default.GOARCH)
		buildTargetPath = filepath.Join(pkg.PkgRoot, pkgArch, moduleName+".a")
	}
	// alog.Printf("Package target: %q\n", buildTargetPath)

	getBuildTargetModTime := func() (time.Time, error) {
		stat, err := os.Stat(buildTargetPath)
		if err != nil {
			return time.Time{}, err
		}
		return stat.ModTime(), nil
	}

	skipBuild := false
	if !finishedInitialPass {
		files, err := ioutil.ReadDir(absPath)
		if err != nil {
			alog.Printf("@(error:Error reading directory) %s@(error::) %v\n", absPath, err)
			abort()
			return
		}
		var newestGoSrc time.Time
		for _, file := range files {
			if filepath.Ext(file.Name()) == ".go" && !strings.HasSuffix(file.Name(), "_test.go") {
				if file.ModTime().After(newestGoSrc) {
					newestGoSrc = file.ModTime()
				}
			}
		}
		// alog.Printf("mod time of %s: %s\n", moduleName, newestGoSrc)
		buildTargetModTime, err := getBuildTargetModTime()
		if err != nil {
			if !os.IsNotExist(err) {
				alog.Printf("@(error:Failed to stat build target) %s @(error:for) %s@(error::) %v\n", buildTargetPath, moduleName, err)
				abort()
				return
			}
		} else {
			if buildTargetModTime.After(newestGoSrc) {
				// alog.Printf("@(dim:Skipping build for) %s@(dim:. No recent changes.)\n", moduleName)
				skipBuild = true
			}
		}
	}

	if !skipBuild {
		if beVerbose() {
			alog.Printf("@(dim:Building) %s@(dim:...)\n", moduleName)
		}
		var err error
		var retCode int
		if beVerbose() {
			retCode, err = ctx.QuoteCwd("go-install", absPath, "go", "install")
		} else {
			_, _, retCode, err = ctx.RunCwd(absPath, "go", "install")
		}
		if retCode != 0 {
			if beVerbose() {
				alog.Printf("@(error:Failed to build) %s @(dim)(status=%d)@(r)\n", moduleName, retCode)
			}
			abort()
			return
		}
		if err != nil {
			alog.Printf("@(error:Failed to install) %s@(error:: %s)\n", moduleName, err)
			abort()
			return
		}
		err = os.Chtimes(buildTargetPath, time.Now(), buildStartTime)
		if err != nil {
			// Squelch errors at least for godoc, which was special-cased in 1.6:
			if moduleName != "golang.org/x/tools/cmd/godoc" {
				alog.Printf("@(error:Error setting atime/mtime of) %s@(error::) %v\n", buildTargetPath, err)
			}
		}
		durationStr := timer.FormatElapsedColor(2*time.Second, 10*time.Second)
		alog.Printf("@(dim:[)%s@(dim:]) @(green:Successfully built) %s\n", durationStr, moduleName)
	}

	moduleStateMutex.Lock()
	currState := moduleState[moduleName]
	if currState == ModuleBuildingButDirty {
		moduleState[moduleName] = ModuleDirtyIdle
		moduleStateMutex.Unlock()
		moduleTriggerChan <- moduleName
	} else {
		moduleState[moduleName] = ModuleReady
		moduleStateMutex.Unlock()
	}

	go triggerDependenciesOfModule(moduleName)

	if runTests() && packageHasTests(moduleName) && depsAreReady(true) {
		alog.Printf("@(dim:Testing) %s@(dim:...)\n", moduleName)
		if _, err := ctx.QuoteCwd("test-"+moduleName, absPath, "go", "test"); err != nil {
			alog.Printf("@(error:Failed to run tests for) %s@(error:: %s)\n", moduleName, err)
			abort()
			return
		}
	}
}
