package main

import (
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/tillberg/ansi-log"
	"github.com/tillberg/autorestart"
	"github.com/tillberg/stringset"
	"github.com/tillberg/watcher"
)

const enableSanityChecks = true

var Opts struct {
	Verbose    bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor    bool `long:"no-color" description:"Disable ANSI colors"`
	MaxWorkers int  `long:"max-workers" description:"Max number of build workers"`
}

var goPath = os.Getenv("GOPATH")
var srcRoot = filepath.Join(goPath, "src")
var tmpdir = filepath.Join(goPath, "autoinstall-tmp")

var packages = map[string]*Package{}
var updateQueue = []*Package{}
var buildQueue = []*Package{}
var buildSuccess = make(chan *Package)
var buildFailure = make(chan *Package)
var updateFinished = make(chan *Package)
var moduleUpdateChan = make(chan string)
var finishedInitialPass = false
var numWorkersActive = 0
var numBuilds = 0
var numUpdates = 0

var dependenciesNotReadyError = errors.New("At least one dependency is not ready")

func dispatcher() {
	neverChan := make(<-chan time.Time)
	for {
		timeout := neverChan
		hasQueuedWork := len(updateQueue) > 0 || len(buildQueue) > 0
		if hasQueuedWork {
			timeout = time.After(10 * time.Millisecond)
		} else if !finishedInitialPass && numWorkersActive == 0 {
			timeout = time.After(1 * time.Second)
		}

		select {
		case p := <-buildSuccess:
			numBuilds++
			numWorkersActive--
			switch p.State {
			case PackageBuilding:
				p.State = PackageReady
				p.BuiltModTime = p.UpdateStartTime
				triggerDependentPackages(p)
			case PackageBuildingButDirty:
				queueUpdate(p)
			default:
				alog.Panicf("buildSuccess with state %s", p.State)
			}
		case p := <-buildFailure:
			numWorkersActive--
			switch p.State {
			case PackageBuilding:
				p.State = PackageDirtyIdle
			case PackageBuildingButDirty:
				queueUpdate(p)
			default:
				alog.Panicf("buildFailure with state %s", p.State)
			}
		case pUpdate := <-updateFinished:
			numUpdates++
			numWorkersActive--
			p := packages[pUpdate.Name]
			if p == nil {
				alog.Panicf("Couldn't find package %s, yet dispatcher received an update for it.", pUpdate.Name)
			}
			switch p.State {
			case PackageUpdating:
				if pUpdate.UpdateError != nil {
					if pUpdate.RemovePackage {
						alog.Printf("@(dim:Removing package %s from index, as it has been removed from the filesystem.)\n", pUpdate.Name)
						delete(packages, pUpdate.Name)
					} else {
						p.State = PackageDirtyIdle
					}
				} else {
					p.mergeUpdate(pUpdate)
					queueBuild(p)
				}
			case PackageUpdatingButDirty:
				queueUpdate(p)
			default:
				alog.Panicf("updateFinished with state %s", p.State)
			}
		case pName := <-moduleUpdateChan:
			p := packages[pName]
			if p == nil {
				p = NewPackage(pName)
				p.init()
				packages[pName] = p
			}
			switch p.State {
			case PackageReady, PackageDirtyIdle:
				queueUpdate(p)
			case PackageUpdating:
				p.State = PackageUpdatingButDirty
			case PackageBuilding:
				p.State = PackageBuildingButDirty
			case PackageUpdateQueued, PackageUpdatingButDirty, PackageBuildingButDirty:
				// Already have an update queued (or will), no need to change
			case PackageBuildQueued:
				// Has a build queued, but we need to do update first. Splice it out of the buildQueue, then queue the update.
				unqueueBuild(p)
				queueUpdate(p)
			default:
				alog.Panicf("moduleUpdateChan encountered unexpected state %s", p.State)
			}
		case <-timeout:
			if !hasQueuedWork {
				// We've reached the conclusion of the initial pass
				finishedInitialPass = true
				printStartupSummary()
			}
			for numWorkersActive < Opts.MaxWorkers && len(updateQueue) > 0 {
				var pkg *Package
				pkg, updateQueue = updateQueue[0], updateQueue[1:]
				if pkg.State != PackageUpdateQueued {
					alog.Panicf("Package %s was in updateQueue but had state %s", pkg.Name, pkg.State)
				}
				pkg.State = PackageUpdating
				numWorkersActive++
				go update(pkg.Name)
			}
			for numWorkersActive < Opts.MaxWorkers && len(buildQueue) > 0 {
				var pkg *Package
				pkg, buildQueue = buildQueue[0], buildQueue[1:]
				if pkg.State != PackageBuildQueued {
					alog.Panicf("Package %s was in buildQueue but had state %s", pkg.Name, pkg.State)
				}
				depsModTime, err := calcDepsModTime(pkg)
				if pkg.SourceModTime.After(depsModTime) {
					depsModTime = pkg.SourceModTime
				}
				if err == dependenciesNotReadyError {
					// At least one dependency is not ready
					pkg.State = PackageDirtyIdle
				} else if err != nil {
					alog.Panicf("@(error:Encountered unexpected error received from calcDepsModTime: %v)", err)
				} else if !pkg.BuiltModTime.IsZero() && (pkg.BuiltModTime.Equal(depsModTime) || pkg.BuiltModTime.After(depsModTime)) {
					// No need to build, as this package is already up-to-date.
					if beVerbose() {
						alog.Printf("@(dim:No need to build) %s@(dim:. Target mod time is) %s@(dim:, source mod time is) %s@(dim:.)\n", pkg.Name, pkg.BuiltModTime, depsModTime)
					}
					pkg.State = PackageReady
					triggerDependentPackages(pkg)
				} else {
					pkg.State = PackageBuilding
					numWorkersActive++
					go pkg.build()
				}
			}
		}
	}
}

func queueUpdate(p *Package) {
	if enableSanityChecks {
		for _, pkg := range updateQueue {
			if pkg == p {
				alog.Panicf("Package %s already in updateQueue, cannot queue twice", p.Name)
			}
		}
	}
	p.State = PackageUpdateQueued
	updateQueue = append(updateQueue, p)
}

func queueBuild(p *Package) {
	if enableSanityChecks {
		for _, pkg := range buildQueue {
			if pkg == p {
				alog.Panicf("Package %s already in buildQueue, cannot queue twice", p.Name)
			}
		}
	}
	p.State = PackageBuildQueued
	buildQueue = append(buildQueue, p)
}

func unqueueBuild(p *Package) {
	found := false
	// alog.Printf("Unqueue %s\n", p.Name)
	// for _, pkg := range buildQueue {
	// 	alog.Printf("    @(dim:-- %s)\n", pkg.Name)
	// }
	for i, pkg := range buildQueue {
		if p == pkg {
			buildQueue = append(buildQueue[:i], buildQueue[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		alog.Panicf("Package %s was in state PackageBuildQueued but could not be found in buildQueue.", p.Name)
	}
	p.State = PackageDirtyIdle
}

func triggerDependentPackages(p *Package) {
	// Note: This is currently over-cautious, matching even packages that currently import a different, vendored version of this package
	for _, pkg := range packages {
		if pkg.Imports != nil && pkg.Imports.Has(p.ImportName) {
			switch pkg.State {
			case PackageReady, PackageDirtyIdle:
				if pkg.WasUpdated {
					queueBuild(pkg)
				} else {
					queueUpdate(pkg)
				}
			case PackageUpdateQueued, PackageUpdatingButDirty, PackageBuildingButDirty:
				// Package is already queued for update (or will be), so no need for change
			case PackageUpdating:
				pkg.State = PackageUpdatingButDirty
			case PackageBuilding:
				pkg.State = PackageBuildingButDirty
			case PackageBuildQueued:
				// Package already queued for build, so no need to change
			default:
				alog.Panicf("triggerDependentPackages saw unexpected state %s", pkg.State)
			}
		}
	}
}

func calcDepsModTime(p *Package) (time.Time, error) {
	var depsModTime time.Time
	for importName, _ := range p.Imports.Raw() {
		depPackage := resolveImport(p, importName)
		if depPackage == nil || depPackage.State != PackageReady {
			return time.Time{}, dependenciesNotReadyError
		}
		if depPackage.BuiltModTime.After(depsModTime) {
			depsModTime = depPackage.BuiltModTime
		}
	}
	return depsModTime, nil
}

// Resolve this dependency, looking for any possible vendored providers.
func resolveImport(p *Package, importName string) *Package {
	rootPath := p.Name
	for {
		vendorOption := rootPath + "/vendor/" + importName
		depPackage, exists := packages[vendorOption]
		if exists {
			return depPackage
		}
		lastIndex := strings.LastIndex(rootPath, "/")
		if lastIndex == -1 {
			break
		}
		rootPath = rootPath[:lastIndex]
	}
	return packages[importName]
}

func printStartupSummary() {
	numReady := 0
	numUnready := 0
	for _, pkg := range packages {
		switch pkg.State {
		case PackageReady:
			numReady++
		case PackageDirtyIdle:
			numUnready++
		}
	}
	alog.Printf("@(dim:Finished initial pass of all packages.)\n")
	alog.Printf("@(green:%d) @(dim:packages built,) @(green:%d) @(dim:packages up-to-date;) @(warn:%d) @(dim:packages could not be built.)\n", numBuilds, numReady, numUnready)
	alog.Println(numUpdates)
}

func beVerbose() bool {
	return finishedInitialPass || Opts.Verbose
}

var buildExtensions = stringset.New(".go", ".c", ".cc", ".cxx", ".cpp", ".h", ".hh", ".hpp", ".hxx", ".s", ".swig", ".swigcxx", ".syso")

func processPathTriggers(notifyChan chan watcher.PathEvent) {
	for pathEvent := range notifyChan {
		path := pathEvent.Path
		moduleName, err := filepath.Rel(srcRoot, filepath.Dir(path))
		if err != nil {
			alog.Bail(err)
		}
		if buildExtensions.Has(filepath.Ext(path)) {
			if beVerbose() {
				alog.Printf("@(dim:Triggering module) @(cyan:%s) @(dim:due to update of) @(cyan:%s)\n", moduleName, path)
			}
			moduleUpdateChan <- moduleName
		}
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
	alog.Printf("@(dim:autoinstall started.)\n")
	if Opts.MaxWorkers == 0 {
		Opts.MaxWorkers = runtime.GOMAXPROCS(0)
	}
	pluralProcess := ""
	if Opts.MaxWorkers != 1 {
		pluralProcess = "es"
	}
	alog.Printf("@(dim:Building all packages in) @(dim,cyan:%s)@(dim: using up to )@(dim,cyan:%d)@(dim: process%s.)\n", goPath, Opts.MaxWorkers, pluralProcess)
	if !Opts.Verbose {
		alog.Printf("@(dim:Use) --verbose @(dim:to show all messages during startup.)\n")
	}

	listener := watcher.NewListener()
	listener.Path = srcRoot
	// "_workspace" is a kludge to avoid recursing into Godeps workspaces
	// "node_modules" is a kludge to avoid walking into typically-huge node_modules trees
	listener.IgnorePart = stringset.New(".git", ".hg", "node_modules", "data", "_workspace")
	listener.NotifyOnStartup = true
	listener.DebounceDuration = 200 * time.Millisecond
	listener.Start()

	// Delete any straggler tmp files, carefully
	files, err := ioutil.ReadDir(tmpdir)
	if err == nil {
		for _, file := range files {
			if filepath.Ext(file.Name()) == ".tmp" {
				os.Remove(filepath.Join(tmpdir, file.Name()))
			}
		}
	} else if os.IsNotExist(err) {
		err = os.MkdirAll(tmpdir, 0700)
		if err != nil {
			alog.Printf("@(error:Error creating temp directory at %s: %v)\n", tmpdir, err)
			return
		}
	} else {
		alog.Printf("@(error:Error checking contents of temp directory at %s: %v)\n", tmpdir, err)
		return
	}

	go processPathTriggers(listener.NotifyChan)
	go dispatcher()

	<-sighup
}
