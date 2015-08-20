package main

import (
	set "github.com/deckarep/golang-set"
	"github.com/howeyc/fsnotify"
	"github.com/tillberg/ansi-log"
	"github.com/tillberg/autorestart"
	"github.com/tillberg/bismuth"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const moduleDirtyIdle = 0
const moduleDirtyQueued = 1
const moduleBuilding = 2
const moduleBuildingButDirty = 3
const moduleReady = 4

var srcRoot = filepath.Join(os.Getenv("GOPATH"), "src")
var goSrcRoot = filepath.Join(os.Getenv("GOROOT"), "src")
var dirtyModuleQueue = make(chan string, 3000)
var moduleState = make(map[string]int)
var moduleStateMutex sync.RWMutex

var pathDiscoveryChan = make(chan string)
var moduleTriggerChan = make(chan string)

var pathSkipSet = set.NewSetFromSlice([]interface{}{".git", ".hg", "node_modules"})
var rebuildExts = set.NewSetFromSlice([]interface{}{".go"})
var importsMap = make(map[string]set.Set)
var importsMapMutex sync.RWMutex

var watcher *fsnotify.Watcher

var builders chan *builder

type builder struct {
	ctx *bismuth.ExecContext
}

func newBuilder() *builder {
	me := &builder{}
	me.ctx = bismuth.NewExecContext()
	me.ctx.Connect()
	return me
}

func isStandardLibraryPackage(moduleName string) bool {
	stdLibCacheMutex.RLock()
	isInStdLib, ok := stdLibCache[moduleName]
	stdLibCacheMutex.RUnlock()
	if !ok {
		if moduleName == "C" {
			isInStdLib = true
		} else {
			// This is not a perfect method. Also, if GOROOT isn't defined, this will be checking some random paths
			stat, err := os.Stat(filepath.Join(goSrcRoot, moduleName))
			isInStdLib = err == nil && stat.IsDir()
		}
		stdLibCacheMutex.Lock()
		stdLibCache[moduleName] = isInStdLib
		stdLibCacheMutex.Unlock()
	}
	return isInStdLib
}

func parseModuleImports(moduleName string) set.Set {
	fileSet := token.NewFileSet()
	absPath := filepath.Join(srcRoot, moduleName)
	pkgMap, err := parser.ParseDir(fileSet, absPath, nil, parser.ImportsOnly)
	if err != nil {
		log.Printf("@(error:Error parsing import of module) %s@(error::) %s\n", moduleName, err)
		return nil
	}
	moduleDepNames := set.NewSet()
	for _, pkg := range pkgMap {
		for filename, file := range pkg.Files {
			if strings.HasSuffix(filename, "_test.go") {
				continue
			}
			for _, importObj := range file.Imports {
				moduleDepName := importObj.Path.Value
				// importObj.Path.Value is the parsed literal, including quote marks. Let's kludgily
				// remove the quotes:
				if len(moduleDepName) > 2 {
					moduleDepName = moduleDepName[1 : len(moduleDepName)-1]
					if moduleDepName != moduleName && !isStandardLibraryPackage(moduleDepName) {
						moduleDepNames.Add(moduleDepName)
					}
				}
			}
		}
	}
	return moduleDepNames
}

func getImportsForModule(moduleName string, forceRegen bool) set.Set {
	var moduleNames set.Set
	if !forceRegen {
		importsMapMutex.RLock()
		moduleNames = importsMap[moduleName]
		importsMapMutex.RUnlock()
	}
	if moduleNames == nil {
		moduleNames = parseModuleImports(moduleName)
		if moduleNames == nil {
			return nil
		}
		importsMapMutex.Lock()
		importsMap[moduleName] = moduleNames
		importsMapMutex.Unlock()
	}
	return moduleNames
}

var stdLibCache = make(map[string]bool)
var stdLibCacheMutex sync.RWMutex

func moduleHasUpToDateDependencies(moduleName string, forceRegen bool) bool {
	moduleDepNames := getImportsForModule(moduleName, forceRegen)
	if moduleDepNames == nil {
		return false
	}
	// log.Printf("module %s depends on %s\n", moduleName, moduleNames.ToSlice())
	for moduleDepName := range moduleDepNames.Iter() {
		moduleDepNameStr := moduleDepName.(string)
		moduleStateMutex.RLock()
		isReady := moduleState[moduleDepNameStr] == moduleReady
		moduleStateMutex.RUnlock()
		if !isReady {
			log.Printf("@(dim:%s is missing dep %s)\n", moduleName, moduleDepNameStr)
			return false
		}
		if !moduleHasUpToDateDependencies(moduleDepNameStr, false) {
			log.Printf("  (required by %s)\n", moduleName)
			return false
		}
	}
	return true
}

func triggerDependenciesOfModule(moduleName string) {
	triggers := []string{}
	importsMapMutex.RLock()
	for otherModuleName, moduleDepNames := range importsMap {
		// log.Println(moduleName, moduleDepNames)
		if moduleDepNames.Contains(moduleName) {
			triggers = append(triggers, otherModuleName)
		}
	}
	importsMapMutex.RUnlock()
	for _, trigger := range triggers {
		// log.Println("trigger", trigger)
		moduleTriggerChan <- trigger
	}
}

func printStateSummary() {
	moduleStateMutex.RLock()
	counts := make(map[int]int)
	for name, state := range moduleState {
		counts[state] = counts[state] + 1
		_ = name
		// if state == moduleDirtyIdle {
		// 	log.Printf("@(dim:  idle:) %s\n", name)
		// }
	}
	moduleStateMutex.RUnlock()
	log.Printf("@(dim:idle:) %d@(dim:, queued:) %d@(dim:, building:) %d@(dim:, building+dirty:) %d@(dim:, ready:) %d\n", counts[0], counts[1], counts[2], counts[3], counts[4])
}

func (b *builder) buildModule(moduleName string) {
	defer func() {
		builders <- b
	}()
	abort := func() {
		moduleStateMutex.Lock()
		currState := moduleState[moduleName]
		moduleState[moduleName] = moduleDirtyIdle
		moduleStateMutex.Unlock()
		if currState == moduleBuildingButDirty {
			moduleTriggerChan <- moduleName
		}
	}
	moduleStateMutex.Lock()
	moduleState[moduleName] = moduleBuilding
	moduleStateMutex.Unlock()
	if !moduleHasUpToDateDependencies(moduleName, true) {
		// If this module is missing any up-to-date dependecies, send
		// it to the end of the queue after a brief pause
		log.Printf("@(dim:Not building) %s @(dim:yet, dependencies not ready.)\n", moduleName)
		// go func() {
		// 	time.Sleep(1000 * time.Millisecond)
		// 	dirtyModuleQueue <- moduleName
		// }()
		abort()
		return
	}
	ctx := b.ctx
	log.Printf("@(dim:Building) %s@(dim:...)\n", moduleName)
	absPath := filepath.Join(srcRoot, moduleName)
	retCode, err := ctx.QuoteCwd("go-install", absPath, "go", "install")
	if retCode != 0 {
		log.Printf("@(error:Failed to build) %s@(error:.)\n", moduleName)
		abort()
		return
	}
	if err != nil {
		log.Printf("@(error:Failed to install %s@(error:: %s)\n", moduleName, err)
		abort()
		return
	}
	log.Printf("@(green:Successfully built) %s@(green:.)\n", moduleName)
	moduleStateMutex.Lock()
	currState := moduleState[moduleName]
	if currState == moduleBuildingButDirty {
		moduleState[moduleName] = moduleDirtyIdle
		moduleStateMutex.Unlock()
		moduleTriggerChan <- moduleName
	} else {
		moduleState[moduleName] = moduleReady
		moduleStateMutex.Unlock()
	}
	// printStateSummary()
	go triggerDependenciesOfModule(moduleName)
}

func processBuildQueue() {
	for {
		moduleName := <-dirtyModuleQueue
		b := <-builders
		go b.buildModule(moduleName)
	}
}

func processModuleTriggers() {
	neverChan := make(<-chan time.Time)
	for {
		timeoutChan := neverChan
		moduleNames := set.NewSet()
		for {
			select {
			case moduleName := <-moduleTriggerChan:
				moduleNames.Add(moduleName)
				// Debounce additional triggers for this module. This is particularly
				// important for filesystem events, as they often come in pairs.
				timeoutChan = time.After(10 * time.Millisecond)
				continue
			case <-timeoutChan:
				goto breakFor
			}
		}
	breakFor:
		for ifaceModuleName := range moduleNames.Iter() {
			moduleName := ifaceModuleName.(string)
			if strings.Contains(moduleName, "/test") {
				// Skip modules that look like test data
				continue
			}
			moduleStateMutex.Lock()
			currState := moduleState[moduleName]
			if currState == moduleBuilding {
				log.Printf("@(dim:Marking in-progress build of) %s @(dim:dirty.)\n", moduleName)
				moduleState[moduleName] = moduleBuildingButDirty
				moduleStateMutex.Unlock()
			} else if currState == moduleDirtyIdle || currState == moduleReady {
				log.Printf("@(dim:Queueing rebuild of) %s@(dim:.)\n", moduleName)
				moduleState[moduleName] = moduleDirtyQueued
				moduleStateMutex.Unlock()
				dirtyModuleQueue <- moduleName
			} else {
				moduleStateMutex.Unlock()
			}
		}
	}
}

func processPathTriggers() {
	for {
		// var verb string
		var path string
		var moduleName string
		select {
		case err := <-watcher.Error:
			log.Printf("Watcher error: %s\n", err)
			continue
		case ev := <-watcher.Event:
			path = ev.Name
			// verb = "change in"
			var err error
			moduleName, err = filepath.Rel(srcRoot, filepath.Dir(path))
			if err != nil {
				log.Bail(err)
			}
		case path = <-pathDiscoveryChan:
			// verb = "discovery of"
			moduleName = filepath.Dir(path)
		}
		if !rebuildExts.Contains(filepath.Ext(path)) {
			continue
		}
		// log.Printf("@(dim:Triggering module) %s @(dim:due to %s) %s\n", moduleName, verb, path)
		moduleTriggerChan <- moduleName
	}
}

func watchRecursive(relPath string) {
	absPath := filepath.Join(srcRoot, relPath)
	base := filepath.Base(absPath)
	if pathSkipSet.Contains(base) {
		return
	}
	stat, err := os.Stat(absPath)
	if err != nil {
		log.Printf("Error calling stat on %s: %s\n", absPath, err)
		return
	}
	if stat.IsDir() {
		watcher.Watch(absPath)
		entries, err := ioutil.ReadDir(absPath)
		if err != nil {
			log.Printf("Error reading directory %s: %s\n", absPath, err)
			return
		}
		for _, entry := range entries {
			watchRecursive(filepath.Join(relPath, entry.Name()))
		}
	} else {
		pathDiscoveryChan <- relPath
	}
}

func main() {
	log.EnableMultilineMode()
	log.EnableColorTemplate()
	log.SetFlags(0)
	log.SetPrefix("@(dim){isodate micros} ")
	log.AddAnsiColorCode("error", 31)
	log.AddAnsiColorCode("warn", 35)
	autorestart.CleanUpChildZombies()

	var err error
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Bail(err)
	}
	go watchRecursive("")
	go processPathTriggers()
	go processModuleTriggers()
	go autorestart.RestartOnChange()
	numBuilders := runtime.NumCPU()
	builders = make(chan *builder, numBuilders)
	for i := 0; i < numBuilders; i++ {
		builders <- newBuilder()
	}
	processBuildQueue()
}
