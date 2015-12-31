package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
			// go func() {
			//  time.Sleep(1000 * time.Millisecond)
			//  dirtyModuleQueue <- moduleName
			// }()
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
	if beVerbose() {
		alog.Printf("@(dim:Building) %s@(dim:...)\n", moduleName)
	}
	absPath := filepath.Join(srcRoot, moduleName)
	packageName := parsePackageName(moduleName)
	var destPath string
	if packageName == "main" {
		exeName := filepath.Base(filepath.Dir(moduleName))
		destPath = filepath.Join("bin", exeName)
	} else {
		destPath = filepath.Join("pkg", fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH), moduleName) + ".a"
	}
	absDestPath := filepath.Join(goPath, destPath)
	var destExistedBefore bool
	if !beVerbose() {
		statBefore, _ := os.Stat(absDestPath)
		destExistedBefore = statBefore != nil
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
	var forceBuildMessageDisplay bool
	if !beVerbose() && !destExistedBefore {
		statAfter, _ := os.Stat(absDestPath)
		forceBuildMessageDisplay = statAfter != nil
	}
	if beVerbose() || forceBuildMessageDisplay {
		alog.Printf("@(green:Successfully built) %s\n", moduleName)
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

	// printStateSummary()
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
