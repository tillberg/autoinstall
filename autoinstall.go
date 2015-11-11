package main

import (
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	set "github.com/deckarep/golang-set"
	"github.com/jessevdk/go-flags"
	log "github.com/tillberg/ansi-log"
	"github.com/tillberg/autorestart"
	"gopkg.in/fsnotify.v1"
)

var Opts struct {
	Verbose     bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	AutoRestart bool `long:"auto-restart" description:"Restart self when updated (for dev use)"`
	NoColor     bool `long:"no-color" description:"Disable ANSI colors"`
}

const moduleDirtyIdle = 0
const moduleDirtyQueued = 1
const moduleBuilding = 2
const moduleBuildingButDirty = 3
const moduleReady = 4

var goPath = os.Getenv("GOPATH")
var srcRoot = filepath.Join(goPath, "src")
var dirtyModuleQueue = make(chan string, 10000)
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
var goStdLibPackages set.Set
var finishedInitialPass = false
var alwaysBeVerbose bool

func parsePackageName(moduleName string) string {
	fileSet := token.NewFileSet()
	absPath := filepath.Join(srcRoot, moduleName)
	pkgMap, err := parser.ParseDir(fileSet, absPath, nil, parser.PackageClauseOnly)
	if err != nil {
		log.Printf("@(error:Error parsing package name of module) %s@(error::) %s\n", moduleName, err)
		return ""
	}
	for pkgName := range pkgMap {
		return pkgName
	}
	return ""
}

func parseModuleImports(moduleName string) set.Set {
	fileSet := token.NewFileSet()
	absPath := filepath.Join(srcRoot, moduleName)
	pkgMap, err := parser.ParseDir(fileSet, absPath, nil, parser.ImportsOnly)
	if err != nil {
		if beVerbose() {
			log.Printf("@(error:Error parsing import of module) %s@(error::) %s\n", moduleName, err)
		}
		return nil
	}
	moduleDepNames := set.NewSet()
	for _, pkg := range pkgMap {
		for filename, file := range pkg.Files {
			if strings.HasSuffix(filename, "_test.go") {
				continue
			}
			if strings.HasSuffix(filename, "_cgo.go") {
				continue
			}
			for _, importObj := range file.Imports {
				moduleDepName := importObj.Path.Value
				// importObj.Path.Value is the parsed literal, including quote marks. Let's kludgily
				// remove the quotes:
				if len(moduleDepName) > 2 {
					moduleDepName = moduleDepName[1 : len(moduleDepName)-1]
					if moduleDepName != moduleName && !goStdLibPackages.Contains(moduleDepName) {
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

func listMissingDependencies(moduleName string) []string {
	allDepNames := set.NewSet()
	moduleDepNames := getImportsForModule(moduleName, true)
	if moduleDepNames == nil {
		return nil
	}
	stack := []string{}
	for moduleDepName := range moduleDepNames.Iter() {
		stack = append(stack, moduleDepName.(string))
	}
	for len(stack) > 0 {
		curr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if allDepNames.Add(curr) {
			nexts := getImportsForModule(moduleName, false)
			if nexts != nil {
				for next := range nexts.Iter() {
					stack = append(stack, next.(string))
				}
			}
		}
	}
	notReady := []string{}
	moduleStateMutex.RLock()
	defer moduleStateMutex.RUnlock()
	for moduleDepName := range allDepNames.Iter() {
		moduleDepNameStr := moduleDepName.(string)
		// log.Printf("module %s depends on %s\n", moduleName, moduleNames.ToSlice())
		isReady := moduleState[moduleDepNameStr] == moduleReady
		if !isReady {
			// Check to see if this dependency even exists
			if beVerbose() {
				stat, err := os.Stat(filepath.Join(srcRoot, moduleDepNameStr))
				if err != nil || !stat.IsDir() {
					log.Printf("@(warn:Dependency missing:) %s@(dim:, needed by) %s\n", moduleDepNameStr, moduleName)
				}
			}
			notReady = append(notReady, moduleDepNameStr)
		}
	}
	return notReady
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

func getStateSummary() map[int]int {
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
	return counts
}

func printStateSummary() {
	counts := getStateSummary()
	log.Printf("@(dim:idle:) %d@(dim:, queued:) %d@(dim:, building:) %d@(dim:, building+dirty:) %d@(dim:, ready:) %d\n", counts[0], counts[1], counts[2], counts[3], counts[4])
}

func beVerbose() bool {
	return finishedInitialPass || Opts.Verbose
}

func processBuildQueue() {
	neverChan := make(<-chan time.Time)
	for {
		timeoutChan := neverChan
		if !finishedInitialPass {
			timeoutChan = time.After(500 * time.Millisecond)
		}
		select {
		case moduleName := <-dirtyModuleQueue:
			b := <-builders
			go b.buildModule(moduleName)
		case <-timeoutChan:
			counts := getStateSummary()
			if counts[moduleDirtyQueued] == 0 && counts[moduleBuilding] == 0 && counts[moduleBuildingButDirty] == 0 && (counts[moduleReady] > 0 || counts[moduleDirtyIdle] > 0) {
				finishedInitialPass = true
				log.Printf("@(dim:Finished initial pass of all packages.)\n")
				log.Printf("@(green:%d) @(dim:packages up-to-date;) @(warn:%d) @(dim:packages could not be built.)\n", counts[moduleReady], counts[moduleDirtyIdle])
			}
		}
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
			// if strings.Contains(moduleName, "/test") {
			// 	// Skip modules that look like test data
			// 	continue
			// }
			moduleStateMutex.Lock()
			currState := moduleState[moduleName]
			if currState == moduleBuilding {
				if beVerbose() {
					log.Printf("@(dim:Marking in-progress build of) %s @(dim:dirty.)\n", moduleName)
				}
				moduleState[moduleName] = moduleBuildingButDirty
				moduleStateMutex.Unlock()
			} else if currState == moduleDirtyIdle || currState == moduleReady {
				// log.Printf("@(dim:Queueing build of) %s\n", moduleName)
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
		case err := <-watcher.Errors:
			log.Printf("Watcher error: %s\n", err)
			continue
		case ev := <-watcher.Events:
			path = ev.Name
			// verb = "change in"
			var err error
			moduleName, err = filepath.Rel(srcRoot, filepath.Dir(path))
			if err != nil {
				log.Bail(err)
			}
			if ev.Op&fsnotify.Create != 0 {
				stat, err := os.Stat(path)
				if err == nil && stat.IsDir() {
					relPath, err := filepath.Rel(srcRoot, path)
					if err != nil {
						log.Printf("@(error:Error resolving relative path from %s to %s: %s)\n", srcRoot, path, err)
					} else {
						log.Printf("@(dim:Watching new directory) %s\n", relPath)
						go watchRecursive(relPath)
					}
				}
			}
		case path = <-pathDiscoveryChan:
			// verb = "discovery of"
			moduleName = filepath.Dir(path)
		}
		if !rebuildExts.Contains(filepath.Ext(path)) || strings.HasSuffix(path, "_test.go") {
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
		// log.Printf("WATCH %s\n", relPath)
		watcher.Add(absPath)
		entries, err := ioutil.ReadDir(absPath)
		if err != nil {
			log.Printf("Error reading directory %s: %s\n", absPath, err)
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "_workspace" {
				continue // kludge to avoid recursing into Godeps workspaces
			}
			watchRecursive(filepath.Join(relPath, name))
		}
	} else {
		pathDiscoveryChan <- relPath
	}
}

func main() {
	_, err := flags.ParseArgs(&Opts, os.Args)
	if err != nil {
		err2, ok := err.(*flags.Error)
		if ok && err2.Type == flags.ErrHelp {
			return
		}
		log.Printf("Error parsing command-line options: %s\n", err)
		return
	}
	if goPath == "" {
		log.Printf("GOPATH is not set in the environment. Please set GOPATH first, then retry.\n")
		log.Printf("For help setting GOPATH, see https://golang.org/doc/code.html\n")
		return
	}
	if Opts.NoColor {
		log.DisableColor()
	}
	if Opts.AutoRestart {
		autorestart.CleanUpChildZombiesQuietly()
	}
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("@(error:Error initializing fsnotify Watcher: %s)\n", err)
		return
	}
	log.Printf("@(dim:autoinstall started; beginning first pass of all packages...)\n")
	if !Opts.Verbose {
		log.Printf("@(dim:Use) --verbose @(dim:to show all messages during startup.)\n")
	}
	goStdLibPackages = set.NewThreadUnsafeSet()
	for _, s := range goStdLibPackagesSlice {
		goStdLibPackages.Add(s)
	}
	go processPathTriggers()
	go processModuleTriggers()
	watchRecursive("")
	time.Sleep(200 * time.Millisecond)
	if Opts.AutoRestart {
		go autorestart.RestartOnChange()
	}
	numBuilders := runtime.NumCPU()
	builders = make(chan *builder, numBuilders)
	for i := 0; i < numBuilders; i++ {
		builders <- newBuilder()
	}
	processBuildQueue()
}
