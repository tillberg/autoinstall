package main

import (
	"fmt"
	"os"
	"path/filepath"
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
	if beVerbose() {
		alog.Printf("@(dim:Building) %s@(dim:...)\n", moduleName)
	}
	absPath := filepath.Join(srcRoot, moduleName)
	var err error
	var retCode int
	start := time.Now()
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
	alog.Printf("@(green:Successfully built) %s @(green:in) %.0f ms\n", moduleName, time.Since(start).Seconds()*1000.0)

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
