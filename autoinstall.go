package main

import (
	"go/build"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	set "github.com/deckarep/golang-set"
	"github.com/jessevdk/go-flags"
	"github.com/tillberg/ansi-log"
	"github.com/tillberg/autorestart"
	"github.com/tillberg/stringset"
	"github.com/tillberg/watcher"
)

var Opts struct {
	Verbose      bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor      bool `long:"no-color" description:"Disable ANSI colors"`
	MaxWorkers   int  `long:"max-workers" description:"Max number of build workers"`
	DisableTests bool `long:"disable-tests" description:"Don't try to run 'go test' after rebuilding packages"`
}

type ModuleState int

const (
	ModuleDirtyIdle ModuleState = iota
	ModuleDirtyQueued
	ModuleBuilding
	ModuleBuildingButDirty
	ModuleReady
)

var goPath = os.Getenv("GOPATH")
var srcRoot = filepath.Join(goPath, "src")
var dirtyModuleQueue = make(chan string, 10000)
var moduleState = make(map[string]ModuleState)
var moduleStateMutex sync.RWMutex
var pathDiscoveryChan = make(chan string)
var moduleTriggerChan = make(chan string)
var rebuildExts = set.NewSetFromSlice([]interface{}{".go"})
var importsMap = make(map[string]set.Set)
var importsMapMutex sync.RWMutex
var builders chan *builder
var goStdLibPackages set.Set
var finishedInitialPass = false
var alwaysBeVerbose bool

// func isPackageCommand(moduleName string) bool {
// 	absPath := filepath.Join(srcRoot, moduleName)
// 	pkg, err := build.ImportDir(absPath, build.ImportComment)
// 	if err != nil {
// 		alog.Printf("@(error:Error parsing package name of module) %s@(error::) %s\n", moduleName, err)
// 		return false
// 	}
// 	return pkg.IsCommand()
// }

func packageHasTests(moduleName string) bool {
	absPath := filepath.Join(srcRoot, moduleName)
	pkg, err := build.ImportDir(absPath, build.ImportComment)
	if err != nil {
		alog.Printf("Error reading directory %s: %s\n", absPath, err)
		return false
	}
	return len(pkg.TestGoFiles) > 0
}

func parseModuleImports(moduleName string, includeTests bool) set.Set {
	absPath := filepath.Join(srcRoot, moduleName)
	pkg, err := build.ImportDir(absPath, build.ImportComment)
	if err != nil {
		if beVerbose() {
			alog.Printf("@(error:Error parsing import of module) %s@(error::) %s\n", moduleName, err)
		}
		return nil
	}
	moduleDepNames := set.NewSet()
	for _, imp := range pkg.Imports {
		if !goStdLibPackages.Contains(imp) {
			moduleDepNames.Add(imp)
		}
	}
	return moduleDepNames
}

func getImportsForModule(moduleName string, forceRegen bool, includeTests bool) set.Set {
	var moduleNames set.Set
	if !forceRegen {
		importsMapMutex.RLock()
		moduleNames = importsMap[moduleName]
		importsMapMutex.RUnlock()
	}
	if moduleNames == nil {
		moduleNames = parseModuleImports(moduleName, includeTests)
		if moduleNames == nil {
			return nil
		}
		importsMapMutex.Lock()
		importsMap[moduleName] = moduleNames
		importsMapMutex.Unlock()
	}
	return moduleNames
}

func listMissingDependencies(moduleName string, includeTests bool) []string {
	allDepNames := set.NewSet()
	moduleDepNames := getImportsForModule(moduleName, true, includeTests)
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
			nexts := getImportsForModule(moduleName, false, false)
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
		// alog.Printf("module %s depends on %s\n", moduleName, moduleNames.ToSlice())
		isReady := moduleState[moduleDepNameStr] == ModuleReady
		if !isReady {
			// Check to see if this dependency even exists
			if beVerbose() {
				stat, err := os.Stat(filepath.Join(srcRoot, moduleDepNameStr))
				if err != nil || !stat.IsDir() {
					alog.Printf("@(warn:Dependency missing:) %s@(dim:, needed by) %s\n", moduleDepNameStr, moduleName)
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
		// alog.Println(moduleName, moduleDepNames)
		if moduleDepNames.Contains(moduleName) {
			triggers = append(triggers, otherModuleName)
		}
	}
	importsMapMutex.RUnlock()
	for _, trigger := range triggers {
		// alog.Println("trigger", trigger)
		moduleTriggerChan <- trigger
	}
}

func getStateSummary() map[ModuleState]int {
	moduleStateMutex.RLock()
	counts := make(map[ModuleState]int)
	for name, state := range moduleState {
		counts[state] = counts[state] + 1
		_ = name
		// if state == ModuleDirtyIdle {
		// 	alog.Printf("@(dim:  idle:) %s\n", name)
		// }
	}
	moduleStateMutex.RUnlock()
	return counts
}

func printStateSummary() {
	counts := getStateSummary()
	alog.Printf("@(dim:idle:) %d@(dim:, queued:) %d@(dim:, building:) %d@(dim:, building+dirty:) %d@(dim:, ready:) %d\n",
		counts[ModuleDirtyIdle], counts[ModuleDirtyQueued], counts[ModuleBuilding], counts[ModuleBuildingButDirty], counts[ModuleReady])
}

func beVerbose() bool {
	return finishedInitialPass || Opts.Verbose
}

func runTests() bool {
	return !Opts.DisableTests && finishedInitialPass
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
			if counts[ModuleDirtyQueued] == 0 && counts[ModuleBuilding] == 0 && counts[ModuleBuildingButDirty] == 0 && (counts[ModuleReady] > 0 || counts[ModuleDirtyIdle] > 0) {
				finishedInitialPass = true
				alog.Printf("@(dim:Finished initial pass of all packages.)\n")
				alog.Printf("@(green:%d) @(dim:packages up-to-date;) @(warn:%d) @(dim:packages could not be built.)\n", counts[ModuleReady], counts[ModuleDirtyIdle])
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
				timeoutChan = time.After(100 * time.Millisecond)
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
			if currState == ModuleBuilding {
				if beVerbose() {
					alog.Printf("@(dim:Marking in-progress build of) %s @(dim:dirty.)\n", moduleName)
				}
				moduleState[moduleName] = ModuleBuildingButDirty
				moduleStateMutex.Unlock()
			} else if currState == ModuleDirtyIdle || currState == ModuleReady {
				// alog.Printf("@(dim:Queueing build of) %s\n", moduleName)
				moduleState[moduleName] = ModuleDirtyQueued
				moduleStateMutex.Unlock()
				dirtyModuleQueue <- moduleName
			} else {
				moduleStateMutex.Unlock()
			}
		}
	}
}

func processPathTriggers(notifyChan chan string) {
	for path := range notifyChan {
		moduleName, err := filepath.Rel(srcRoot, filepath.Dir(path))
		if err != nil {
			alog.Bail(err)
		}
		if !rebuildExts.Contains(filepath.Ext(path)) {
			continue
		}
		// alog.Printf("@(dim:Triggering module) @(cyan:%s) @(dim:due to update of) @(cyan:%s)\n", moduleName, path)
		moduleTriggerChan <- moduleName
	}
}

func main() {
	sighup := autorestart.NotifyOnSighup()
	_, err := flags.ParseArgs(&Opts, os.Args)
	if err != nil {
		err2, ok := err.(*flags.Error)
		if ok && err2.Type == flags.ErrHelp {
			return
		}
		alog.Printf("Error parsing command-line options: %s\n", err)
		return
	}
	if goPath == "" {
		alog.Printf("GOPATH is not set in the environment. Please set GOPATH first, then retry.\n")
		alog.Printf("For help setting GOPATH, see https://golang.org/doc/code.html\n")
		return
	}
	if Opts.NoColor {
		alog.DisableColor()
	}
	if Opts.MaxWorkers == 0 {
		Opts.MaxWorkers = runtime.NumCPU()
	}
	alog.Printf("@(dim:autoinstall started.)\n")
	pluralProcess := ""
	if Opts.MaxWorkers != 1 {
		pluralProcess = "es"
	}
	alog.Printf("@(dim:Building all packages in) @(dim,cyan:%s)@(dim: using up to )@(dim,cyan:%d)@(dim: process%s.)\n", goPath, Opts.MaxWorkers, pluralProcess)
	if !Opts.Verbose {
		alog.Printf("@(dim:Use) --verbose @(dim:to show all messages during startup.)\n")
	}
	goStdLibPackages = set.NewThreadUnsafeSet()
	for _, s := range goStdLibPackagesSlice {
		goStdLibPackages.Add(s)
	}

	listener := watcher.NewListener()
	listener.Path = srcRoot
	// "_workspace" is a kludge to avoid recursing into Godeps workspaces
	listener.IgnorePart = stringset.New(".git", ".hg", "node_modules", "data", "_workspace")
	listener.NotifyOnStartup = true
	listener.DebounceDuration = 200 * time.Millisecond
	listener.Start()

	go processPathTriggers(listener.NotifyChan)
	go processModuleTriggers()

	time.Sleep(200 * time.Millisecond)
	builders = make(chan *builder, Opts.MaxWorkers)
	for i := 0; i < Opts.MaxWorkers; i++ {
		builders <- newBuilder()
	}
	go processBuildQueue()
	<-sighup
}
