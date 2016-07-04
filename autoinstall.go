package main

import (
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/tillberg/ansi-log"
	"github.com/tillberg/autorestart"
	"github.com/tillberg/stringset"
	"github.com/tillberg/watcher"
)

var Opts struct {
	Verbose    bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor    bool `long:"no-color" description:"Disable ANSI colors"`
	MaxWorkers int  `long:"max-workers" description:"Max number of build workers"`
	RunTests   bool `long:"run-tests" description:"Run 'go test' after successfully rebuilding packages"`
}

type moduleImports struct {
	// List of dependencies that must be ready in order to build this module.
	resolvedDependencies *stringset.StringSet

	// List of possible (vendored) dependencies to watch for in the future.
	extraDepListens *stringset.StringSet
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
var rebuildExts = stringset.New(".go")
var importsMap = map[string]*moduleImports{}
var importsMapMutex sync.RWMutex
var builders chan *builder
var goStdLibPackages *stringset.StringSet
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

const fieldSeparator = '\x00'

var moduleImportsParseError = errors.New("Error parsing module imports")

func parseModuleImports(moduleName string, includeTests bool) ([]string, error) {
	absPath := filepath.Join(srcRoot, moduleName)
	pkg, err := build.ImportDir(absPath, build.ImportComment)
	if err != nil {
		if beVerbose() {
			alog.Printf("@(error:Error parsing import of module) %s@(error::) %s\n", moduleName, err)
		}
		return nil, moduleImportsParseError
	}
	moduleDepNames := []string{}
	for _, imp := range pkg.Imports {
		if !goStdLibPackages.Has(imp) {
			moduleDepNames = append(moduleDepNames, imp)
		}
	}
	return moduleDepNames, nil
}

func getImportsForModule(moduleName string, forceRegen bool, includeTests bool) *moduleImports {
	var imports *moduleImports
	if !forceRegen {
		importsMapMutex.RLock()
		imports = importsMap[moduleName]
		importsMapMutex.RUnlock()
	}
	if imports == nil {
		rawModuleNames, err := parseModuleImports(moduleName, includeTests)
		if err != nil {
			importsMapMutex.Lock()
			delete(importsMap, moduleName)
			importsMapMutex.Unlock()
			return nil
		}
		imports = &moduleImports{
			resolvedDependencies: stringset.New(),
			extraDepListens:      stringset.New(),
		}
		for _, depName := range rawModuleNames {
			dep := dependency{
				moduleName: moduleName,
				depName:    depName,
			}
			resolvedDepName, extraDepListens := dep.resolve()
			imports.resolvedDependencies.Add(resolvedDepName)
			for _, extraDepListen := range extraDepListens {
				imports.extraDepListens.Add(extraDepListen)
			}
		}

		importsMapMutex.Lock()
		importsMap[moduleName] = imports
		importsMapMutex.Unlock()
	}
	return imports
}

type dependency struct {
	moduleName string
	depName    string
}

func (dep dependency) allVendorOptions() []string {
	all := []string{}
	rootPath := dep.moduleName
	for {
		vendorOption := rootPath + "/vendor/" + dep.depName
		all = append(all, vendorOption)
		lastIndex := strings.LastIndex(rootPath, "/")
		if lastIndex == -1 {
			break
		}
		rootPath = rootPath[:lastIndex]
	}
	return all
}

// Resolve this dependency, looking for any possible vendored providers.
func (dep dependency) resolve() (string, []string) {
	allVendorOptions := dep.allVendorOptions()
	importsMapMutex.Lock()
	defer importsMapMutex.Unlock()
	for i, vendorOption := range allVendorOptions {
		// alog.Printf("@(dim:Trying %s)\n", vendorOption)
		if _, exists := importsMap[vendorOption]; exists {
			// alog.Printf("Found @(cyan:%s)\n", vendorOption)
			return vendorOption, allVendorOptions[:i]
		}
	}
	return dep.depName, allVendorOptions
}

func listMissingDependencies(moduleName string, includeTests bool) []string {
	allDepNames := stringset.New()
	imports := getImportsForModule(moduleName, true, includeTests)
	// alog.Printf("module %s depends on %s\n", moduleName, imports)
	if imports == nil {
		// In the event that this module was deleted, we should fire off a rebuild of anything that depends on this module:
		go triggerDependenciesOfModule(moduleName)
		return nil
	}
	var currDep string
	stack := []string{}
	for moduleDepName := range imports.resolvedDependencies.Raw() {
		stack = append(stack, moduleDepName)
	}
	for len(stack) > 0 {
		currDep, stack = stack[len(stack)-1], stack[:len(stack)-1]
		if allDepNames.Add(currDep) {
			depImports := getImportsForModule(currDep, false, false)
			if depImports != nil {
				// XXX should we abort if depImports == nil?
				for next := range depImports.resolvedDependencies.Raw() {
					stack = append(stack, next)
				}
			}
		}
	}
	notReady := []string{}
	moduleStateMutex.RLock()
	defer moduleStateMutex.RUnlock()
	for moduleDepName := range allDepNames.Raw() {
		isReady := moduleState[moduleDepName] == ModuleReady
		if !isReady {
			// Check to see if this dependency even exists
			if beVerbose() {
				stat, err := os.Stat(filepath.Join(srcRoot, moduleDepName))
				if err != nil || !stat.IsDir() {
					alog.Printf("@(warn:Dependency missing:) %s@(dim:, needed by) %s\n", moduleDepName, moduleName)
				}
			}
			notReady = append(notReady, moduleDepName)
		}
	}
	return notReady
}

func triggerDependenciesOfModule(moduleName string) {
	triggers := []string{}
	importsMapMutex.RLock()
	for otherModuleName, imports := range importsMap {
		// alog.Println(moduleName, moduleDepNames)
		if imports.resolvedDependencies.Has(moduleName) || imports.extraDepListens.Has(moduleName) {
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
	return Opts.RunTests && finishedInitialPass
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
		moduleNames := stringset.New()
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
		for moduleName := range moduleNames.Raw() {
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

func processPathTriggers(notifyChan chan watcher.PathEvent) {
	for pathEvent := range notifyChan {
		path := pathEvent.Path
		moduleName, err := filepath.Rel(srcRoot, filepath.Dir(path))
		if err != nil {
			alog.Bail(err)
		}
		if !rebuildExts.Has(filepath.Ext(path)) {
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
	goStdLibPackages = stringset.New()
	for _, s := range goStdLibPackagesSlice {
		goStdLibPackages.Add(s)
	}

	listener := watcher.NewListener()
	listener.Path = srcRoot
	// "_workspace" is a kludge to avoid recursing into Godeps workspaces
	// "node_modules" is a kludge to avoid walking into typically-huge node_modules trees
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
