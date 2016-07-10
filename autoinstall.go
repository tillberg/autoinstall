package main

import (
	"errors"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/tillberg/ansi-log"
	"github.com/tillberg/autorestart"
	"github.com/tillberg/stringset"
	"github.com/tillberg/watcher"
)

const enableSanityChecks = false
const DATE_FORMAT = "2006-01-02T15:04:05"

var Opts struct {
	Verbose    bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor    bool `long:"no-color" description:"Disable ANSI colors"`
	MaxWorkers int  `long:"max-workers" description:"Max number of build workers"`
}

var goPath = os.Getenv("GOPATH")
var srcRoot = filepath.Join(goPath, "src")
var tmpdir = filepath.Join(goPath, ".autoinstall-tmp")

var packages = map[string]*Package{}
var updateQueue = []*Package{}
var buildQueue = []*Package{}
var buildSuccess = make(chan *Package)
var buildFailure = make(chan *Package)
var updateFinished = make(chan *Package)
var moduleUpdateChan = make(chan string)
var finishedInitialPass = false
var numBuildSucesses = 0
var numBuildFailures = 0
var numReady = 0
var numUnready = 0
var numBuildsActive = 0
var numUpdatesActive = 0
var startupLogger *alog.Logger
var neverChan = make(<-chan time.Time)

func numWorkersActive() int {
	return numUpdatesActive + numBuildsActive
}

var dependenciesNotReadyError = errors.New("At least one dependency is not ready")
var startupFormatStrBase = alog.Colorify("@(green:%d) @(dim:package%s built,) @(green:%d) @(dim:package%s up to date.)")
var startupFormatStrChecking = alog.Colorify(" @(dim:Checking) @(green:%d)@(dim:...)")
var startupFormatStrNotReady = alog.Colorify(" @(warn:%d) @(dim:package%s could not be built.)\n")

func pluralize(num int, s string) string {
	if num == 1 {
		return ""
	}
	return s
}

func updateStartupText(final bool) {
	if !final {
		format := startupFormatStrBase + startupFormatStrChecking
		checkingTotal := len(updateQueue) + numUpdatesActive + len(buildQueue) + numBuildsActive
		startupLogger.Replacef(format, numBuildSucesses, pluralize(numBuildSucesses, "s"), numReady, pluralize(numReady, "s"), checkingTotal)
	} else {
		format := startupFormatStrBase + startupFormatStrNotReady
		startupLogger.Replacef(format, numBuildSucesses, pluralize(numBuildSucesses, "s"), numReady, pluralize(numReady, "s"), numUnready, pluralize(numUnready, "s"))
		startupLogger.Close()
	}
}

type DispatchState int

const (
	DispatchIdle DispatchState = iota
	DispatchCanPushWork
	DispatchWaitingForWork
	DispatchMaybeFinishedInitialPass
)

func getDispatchState() DispatchState {
	if len(updateQueue) > 0 || len(buildQueue) > 0 {
		if numWorkersActive() < Opts.MaxWorkers {
			return DispatchCanPushWork
		}
	} else if !finishedInitialPass && numWorkersActive() == 0 {
		return DispatchMaybeFinishedInitialPass
	}
	if numWorkersActive() > 0 {
		return DispatchWaitingForWork
	}
	return DispatchIdle
}

func getTimeout(state DispatchState) <-chan time.Time {
	switch state {
	case DispatchIdle:
		return neverChan
	case DispatchCanPushWork:
		// Debounce filesystem events a little (though `watcher` already should be doing that more aggressively).
		// In particular, this delay ensures that we process all messages waiting in the various queues before
		// pushing work to workers.
		return time.After(2 * time.Millisecond)
	case DispatchWaitingForWork:
		// Periodically output messages about outstanding builds, both to inform the user about progress on very
		// slow builds as well as to help debug "stuck" builds.
		return time.After(30 * time.Second)
	case DispatchMaybeFinishedInitialPass:
		// When we can wait a whole second with empty queues and no active work, then we call the initial pass
		// complete. This is kind of kludgy but is functional; a more "correct" solution would require `watcher`
		// informing us when *it* had completed a first full walk of the directory tree.
		return time.After(1 * time.Second)
	}
	alog.Panicf("getTimeout received unexpected DispatchState %s", state)
	return nil
}

func dispatcher() {
	startupLogger = alog.New(os.Stderr, "@(dim:{isodate}) ", 0)

	for {
		if !finishedInitialPass {
			updateStartupText(false)
		}
		dispatchState := getDispatchState()
		timeout := getTimeout(dispatchState)

		select {
		case p := <-buildSuccess:
			numBuildSucesses++
			numBuildsActive--
			switch p.State {
			case PackageBuilding:
				chState(p, PackageReady)
				p.BuiltModTime = p.UpdateStartTime
				p.LastBuildInputsModTime = time.Time{}
				triggerDependentPackages(p.ImportName)
			case PackageBuildingButDirty:
				queueUpdate(p)
			default:
				alog.Panicf("buildSuccess with state %s", p.State)
			}
		case p := <-buildFailure:
			numBuildFailures++
			numBuildsActive--
			switch p.State {
			case PackageBuilding:
				chState(p, PackageDirtyIdle)
			case PackageBuildingButDirty:
				queueUpdate(p)
			default:
				alog.Panicf("buildFailure with state %s", p.State)
			}
		case pUpdate := <-updateFinished:
			numUpdatesActive--
			p := packages[pUpdate.Name]
			if p == nil {
				alog.Panicf("Couldn't find package %s, yet dispatcher received an update for it.", pUpdate.Name)
			}
			switch p.State {
			case PackageUpdating:
				if pUpdate.UpdateError != nil {
					if pUpdate.RemovePackage {
						alog.Printf("@(dim:Removing package %s from index, as it has been removed from the filesystem.)\n", pUpdate.Name)
						delete(packages, p.Name)
						// Trigger updates of any packages that depend on this import name
						// XXX this should be modified if triggerDependentPackages is made more specific in the future
						triggerDependentPackages(p.ImportName)
					} else {
						chState(p, PackageDirtyIdle)
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
				numUnready++
			}
			switch p.State {
			case PackageReady, PackageDirtyIdle:
				queueUpdate(p)
			case PackageUpdating:
				chState(p, PackageUpdatingButDirty)
			case PackageBuilding:
				chState(p, PackageBuildingButDirty)
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
			switch dispatchState {
			case DispatchMaybeFinishedInitialPass:
				// We've reached the conclusion of the initial pass
				finishedInitialPass = true
				printStartupSummary()
			case DispatchCanPushWork:
				pushWork()
			case DispatchWaitingForWork:
				for _, p := range packages {
					switch p.State {
					case PackageBuilding, PackageBuildingButDirty:
						alog.Printf("@(dim:Still building %s...)\n", p.Name)
					case PackageUpdating, PackageUpdatingButDirty:
						alog.Printf("@(dim:Still checking %s...)\n", p.Name)
					}
				}
			default:
				alog.Panicf("dispatch hit timeout with unexpected dispatchState %s", dispatchState)
			}
		}
	}
}

func pushWork() {
	for numWorkersActive() < Opts.MaxWorkers && len(updateQueue) > 0 {
		var pkg *Package
		pkg, updateQueue = updateQueue[0], updateQueue[1:]
		if pkg.State != PackageUpdateQueued {
			alog.Panicf("Package %s was in updateQueue but had state %s", pkg.Name, pkg.State)
		}
		chState(pkg, PackageUpdating)
		numUpdatesActive++
		go update(pkg.Name)
	}
	for numWorkersActive() < Opts.MaxWorkers && len(buildQueue) > 0 {
		var pkg *Package
		pkg, buildQueue = buildQueue[0], buildQueue[1:]
		if pkg.State != PackageBuildQueued {
			alog.Panicf("Package %s was in buildQueue but had state %s", pkg.Name, pkg.State)
		}
		if !pkg.shouldBuild() {
			chState(pkg, PackageDirtyIdle)
		} else {
			recentDep, err := getMostRecentDep(pkg)
			if err == dependenciesNotReadyError {
				// At least one dependency is not ready
				chState(pkg, PackageDirtyIdle)
			} else if err != nil {
				alog.Panicf("@(error:Encountered unexpected error received from calcDepsModTime: %v)", err)
			} else {
				var inputsModTime time.Time
				if recentDep != nil {
					inputsModTime = recentDep.BuiltModTime
				}
				if pkg.SourceModTime.After(inputsModTime) {
					inputsModTime = pkg.SourceModTime
				}
				printTimes := func() {
					alog.Printf("    @(dim:Target ModTime) @(time:%s)\n", pkg.BuiltModTime.Format(DATE_FORMAT))
					if recentDep != nil {
						alog.Printf("    @(dim:  deps ModTime) @(time:%s) %s\n", recentDep.BuiltModTime.Format(DATE_FORMAT), recentDep.Name)
					} else {
						alog.Printf("    @(dim:  deps ModTime) n/a @(dim:no dependencies)\n")
					}
					alog.Printf("    @(dim:   src ModTime) @(time:%s) %s\n", pkg.SourceModTime.Format(DATE_FORMAT), pkg.RecentSrcName)
				}
				if !pkg.UpdateStartTime.After(inputsModTime) {
					// This package last updated after some of its inputs. Send it back to update again.
					queueUpdate(pkg)
				} else if !pkg.BuiltModTime.IsZero() && !inputsModTime.After(pkg.BuiltModTime) {
					// No need to build, as this package is already up to date.
					if Opts.Verbose {
						alog.Printf("@(dim:No need to build) %s\n", pkg.Name)
						printTimes()
					}
					chState(pkg, PackageReady)
					triggerDependentPackages(pkg.ImportName)
				} else if !inputsModTime.After(pkg.LastBuildInputsModTime) {
					if Opts.Verbose {
						// Sometimes, package updates/builds can be unnecessary triggered repeatedly. For example, sometimes builds themselves
						// can cause files to be touched in depended-upon packages, resulting in a cycle of endless failed builds (successful
						// builds would not be retried already because we would compare the timestamps and determine that the target was up to
						// date).
						alog.Printf("@(dim:Not building) %s@(dim:, as the package and its dependencies have not changed since its last build, which failed.)\n", pkg.Name)
					}
					chState(pkg, PackageDirtyIdle)
				} else {
					if Opts.Verbose && !pkg.BuiltModTime.IsZero() {
						alog.Printf("@(dim:Building) %s@(dim::)\n", pkg.Name)
						printTimes()
					}
					chState(pkg, PackageBuilding)
					numBuildsActive++
					pkg.LastBuildInputsModTime = inputsModTime
					go pkg.build()
				}
			}
		}
	}
}

func chState(p *Package, state PackageState) {
	if p.State == PackageReady {
		numReady--
	}
	if state == PackageReady {
		numReady++
	}
	if p.State == PackageDirtyIdle {
		numUnready--
	}
	if state == PackageDirtyIdle {
		numUnready++
	}
	p.State = state
}

func queueUpdate(p *Package) {
	if enableSanityChecks {
		for _, pkg := range updateQueue {
			if pkg == p {
				alog.Panicf("Package %s already in updateQueue, cannot queue twice", p.Name)
			}
		}
	}
	chState(p, PackageUpdateQueued)
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
	chState(p, PackageBuildQueued)
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
	chState(p, PackageDirtyIdle)
}

func triggerDependentPackages(importName string) {
	// Note: This is currently over-cautious, matching even packages that currently import a different, vendored/unvendored version of this package
	// alog.Printf("@(dim:Triggering check of packages that import %s)\n", p.ImportName)
	for _, pkg := range packages {
		if pkg.Imports != nil && pkg.Imports.Has(importName) {
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
				chState(pkg, PackageUpdatingButDirty)
			case PackageBuilding:
				chState(pkg, PackageBuildingButDirty)
			case PackageBuildQueued:
				// Package already queued for build, so no need to change
			default:
				alog.Panicf("triggerDependentPackages saw unexpected state %s", pkg.State)
			}
		}
	}
}

func diagnoseNotReady(parent *Package, p *Package) {
	switch p.State {
	case PackageBuilding, PackageBuildingButDirty, PackageUpdating, PackageUpdatingButDirty, PackageUpdateQueued, PackageBuildQueued:
		// Just wait. Though it may be helpful to inform the user of this?
	case PackageDirtyIdle:
		alog.Printf("@(dim:Can't build) %s @(dim:because) %s @(dim:isn't ready.)\n", parent.Name, p.Name)
		p.LastBuildInputsModTime = time.Time{} // Force a build even if a previous one failed, so that we can get the output again
		queueUpdate(p)
	default:
		alog.Panicf("diagnoseNotReady encountered unexpected state %s", p.State)
	}
}

func diagnoseCircularDependency(p *Package) {
	importNameQuoted := strconv.Quote(p.ImportName)
	absPkgPath := filepath.Join(srcRoot, p.Name)
	files, _ := ioutil.ReadDir(absPkgPath)
	for _, filename := range files {
		if filepath.Ext(filename.Name()) != ".go" {
			continue
		}
		path := filepath.Join(absPkgPath, filename.Name())
		fileSet := token.NewFileSet()
		ast, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			alog.Printf("Error parsing %s: %v\n", path, err)
		}
		for _, imp := range ast.Imports {
			if imp.Path.Value == importNameQuoted {
				position := fileSet.Position(imp.Pos())
				alog.Printf("@(error:Circular import found on line %d of %s)\n", position.Line, path)
			}
		}
	}
}

func getMostRecentDep(p *Package) (*Package, error) {
	var recentDep *Package
	for importName, _ := range p.Imports.Raw() {
		depPackage := resolveImport(p, importName)
		if depPackage == nil || depPackage.State != PackageReady {
			if depPackage == p {
				diagnoseCircularDependency(p)
			} else if beVerbose() {
				if depPackage == nil {
					alog.Printf("%s @(dim:requires) %s@(dim:, which could not be found.)\n", p.Name, importName)
				} else {
					diagnoseNotReady(p, depPackage)
				}
			}
			return nil, dependenciesNotReadyError
		}
		if recentDep == nil || depPackage.BuiltModTime.After(recentDep.BuiltModTime) {
			recentDep = depPackage
		}
	}
	return recentDep, nil
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
	alog.Printf("@(dim:Finished initial pass of all packages.)\n")
	updateStartupText(true)
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
			if Opts.Verbose {
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
	} else {
		alog.AddAnsiColorCode("time", alog.ColorBlue)
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
	listener.IgnorePart = stringset.New(".git", ".hg", "node_modules", "_workspace")
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
	startupLogger.Close()
}
