package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/tillberg/alog"
	"github.com/tillberg/autorestart"
	"github.com/tillberg/bismuth2"
	"github.com/tillberg/stringset"
	"github.com/tillberg/watcher"
)

const enableSanityChecks = false
const DATE_FORMAT = "2006-01-02T15:04:05.000000"

var Opts struct {
	Verbose      bool   `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor      bool   `long:"no-color" description:"Disable ANSI colors"`
	MaxWorkers   int    `long:"max-workers" description:"Max number of build workers"`
	RunTests     bool   `long:"run-tests" description:"Run tests after building packages (after initial pass)"`
	Tags         string `long:"tags" description:"-tags parameter to pass to go-build"`
	TestArgShort bool   `long:"test-arg-short" description:"Pass the -short flag to go test"`
	TestArgRun   string `long:"test-arg-run" description:"Pass the -run flag to go test with this value"`
	LDFlags      string `long:"ldflags" description:"Pass the -ldflags to go install with this value"`
}

var goPath = (func() string {
	p := os.Getenv("GOPATH")
	if p == "" {
		panic("GOPATH must be set")
	}
	return p
})()

var goPathSrcRoot = filepath.Join(goPath, "src")

var packages = map[string]*Package{}
var updateQueue = []*Package{}
var buildQueue = []*Package{}
var buildSuccess = make(chan *Package)
var buildFailure = make(chan *Package)
var updateFinished = make(chan *Package)
var moduleUpdateChan = make(chan string) // Buffer size must be greater than # of packages in stdlib
var finishedInitialPass = false
var numBuildSucesses = 0
var numBuildFailures = 0
var numReady = 0
var numUnready = 0
var numBuildsActive = 0
var numUpdatesActive = 0
var startupLogger *alog.Logger
var neverChan = make(<-chan time.Time)
var lastModuleUpdateTime time.Time

var goStdLibPackages struct {
	*stringset.StringSet
	sync.RWMutex
}

func init() {
	goStdLibPackages.StringSet = stringset.New()
}

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
	DispatchCanPushUpdateWork
	DispatchCanPushAnyWork
	DispatchWaitingForWork
	DispatchMaybeFinishedInitialPass
)

func getDispatchState() DispatchState {
	if len(updateQueue) > 0 || len(buildQueue) > 0 {
		if numWorkersActive() < Opts.MaxWorkers {
			if time.Since(lastModuleUpdateTime) > 500*time.Millisecond {
				return DispatchCanPushAnyWork
			} else {
				return DispatchCanPushUpdateWork
			}
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
	case DispatchCanPushUpdateWork:
		fallthrough
	case DispatchCanPushAnyWork:
		// Debounce filesystem events a little (though `watcher` already should be doing that more aggressively).
		// In particular, this delay ensures that we process all messages waiting in the various queues before
		// pushing work to workers.
		return time.After(1 * time.Millisecond)
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
				p.TargetModTime = p.UpdateStartTime
				p.LastBuildInputsModTime = time.Time{}
				triggerDependentPackages(p.ImportName)
			case PackageBuildingButDirty:
				queueUpdate(p, fmt.Sprintf("sources changed during successful build"))
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
				queueUpdate(p, fmt.Sprintf("sources changed during failed build"))
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
					queueBuild(p, fmt.Sprintf("update finished successfully"))
				}
			case PackageUpdatingButDirty:
				queueUpdate(p, fmt.Sprintf("sources changed while updating"))
			default:
				alog.Panicf("updateFinished with state %s", p.State)
			}
		case pName := <-moduleUpdateChan:
			lastModuleUpdateTime = time.Now()
			p := packages[pName]
			if p == nil {
				p = NewPackage(pName)
				p.init()
				packages[pName] = p
				numUnready++
			}
			switch p.State {
			case PackageReady, PackageDirtyIdle:
				queueUpdate(p, fmt.Sprintf("sources changed"))
			case PackageUpdating:
				chState(p, PackageUpdatingButDirty)
			case PackageBuilding:
				chState(p, PackageBuildingButDirty)
			case PackageUpdateQueued, PackageUpdatingButDirty, PackageBuildingButDirty:
				// Already have an update queued (or will), no need to change
			case PackageBuildQueued:
				// Has a build queued, but we need to do update first. Splice it out of the buildQueue, then queue the update.
				unqueueBuild(p)
				queueUpdate(p, fmt.Sprintf("sources changed before build"))
			default:
				alog.Panicf("moduleUpdateChan encountered unexpected state %s", p.State)
			}
		case <-timeout:
			switch dispatchState {
			case DispatchMaybeFinishedInitialPass:
				// We've reached the conclusion of the initial pass
				finishedInitialPass = true
				printStartupSummary()
			case DispatchCanPushUpdateWork:
				pushUpdateWork()
			case DispatchCanPushAnyWork:
				pushUpdateWork()
				pushBuildWork()
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

// var squelchWarningTargetNames = stringset.New("example", "_example", "examples", "_examples", "simple", "_simple", "test", "_test", "testdata", "_testdata", "basic", "hello")
var knownNameCollisions = stringset.New()
var dimComma = alog.Colorify("@(dim:,) ")

func warnNameCollision(pkgNames []string, target string) {
	// if !beVerbose() && squelchWarningTargetNames.Has(target) {
	// 	return
	// }
	if finishedInitialPass || knownNameCollisions.Add(target) {
		sort.Strings(pkgNames)
		buf := bytes.Buffer{}
		for i, name := range pkgNames {
			if i != 0 {
				buf.WriteString(dimComma)
			}
			buf.WriteString(name)
		}
		alog.Printf("%s @(warn:has multiple potential source packages:) %s\n", target, buf.String())
	}
}

func pushUpdateWork() {
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
}

func pushBuildWork() {
workerLoop:
	for len(updateQueue) == 0 && numUpdatesActive == 0 && numWorkersActive() < Opts.MaxWorkers && len(buildQueue) > 0 {
		var pkg *Package
		pkg, buildQueue = buildQueue[0], buildQueue[1:]
		if pkg.State != PackageBuildQueued {
			alog.Panicf("Package %s was in buildQueue but had state %s", pkg.Name, pkg.State)
		}
		if !pkg.shouldBuild() {
			chState(pkg, PackageDirtyIdle)
			continue workerLoop
		}

		recentDep, err := getMostRecentDep(pkg)
		if err != nil {
			if err == dependenciesNotReadyError {
				// At least one dependency is not ready
				chState(pkg, PackageDirtyIdle)
			} else {
				alog.Panicf("@(error:Encountered unexpected error received from calcDepsModTime: %v)", err)
			}
			continue workerLoop
		}

		deps, err := calculateDeps(pkg)
		if err != nil {
			// calculateDeps logs an error already
			chState(pkg, PackageDirtyIdle)
			continue workerLoop
		}

		pkg.computeDesiredBuildID(deps)
		if pkg.IsProgram && pkg.CurrentBuildID != "" && pkg.CurrentBuildID != pkg.DesiredBuildID {
			// alog.Printf("@(dim:%s) Build ID Desired %s\n", pkg.Name, pkg.DesiredBuildID)
			// alog.Printf("@(dim:%s) Build ID Current %s\n", pkg.Name, pkg.CurrentBuildID)
			targetPath := pkg.getAbsTargetPath()
			collisions := stringset.New()
			for _, otherPkg := range packages {
				if pkg != otherPkg && targetPath == otherPkg.getAbsTargetPath() {
					collisions.Add(otherPkg.Name)
				}
			}
			if collisions.Len() > 0 {
				warnNameCollision(append(collisions.All(), pkg.Name), filepath.Base(targetPath))
				if !finishedInitialPass {
					if beVerbose() {
						alog.Printf("@(warn:Skipping) %s @(warn:because another source package could be up to date.)\n", pkg.Name)
					}
					chState(pkg, PackageDirtyIdle)
					continue workerLoop
				}
			}
		}

		var inputsModTime time.Time
		if recentDep != nil {
			inputsModTime = recentDep.TargetModTime
		}
		if pkg.SourceModTime.After(inputsModTime) {
			inputsModTime = pkg.SourceModTime
		}

		printTimes := func() {
			alog.Printf("    @(dim:Target ModTime) @(time:%s)\n", pkg.TargetModTime.Format(DATE_FORMAT))
			if recentDep != nil {
				alog.Printf("    @(dim:  deps ModTime) @(time:%s) %s\n", recentDep.TargetModTime.Format(DATE_FORMAT), recentDep.Name)
			} else {
				alog.Printf("    @(dim:  deps ModTime) n/a @(dim:no dependencies)\n")
			}
			alog.Printf("    @(dim:   src ModTime) @(time:%s) %s\n", pkg.SourceModTime.Format(DATE_FORMAT), pkg.RecentSrcName)
		}

		if !pkg.UpdateStartTime.After(inputsModTime) {
			// This package last updated after some of its inputs. Send it back to update again.
			queueUpdate(pkg, fmt.Sprintf("sources modified after update start time"))
			continue workerLoop
		}

		if !pkg.TargetModTime.IsZero() && !inputsModTime.After(pkg.TargetModTime) && pkg.CurrentBuildID == pkg.DesiredBuildID {
			// No need to build, as this package is already up to date.
			if Opts.Verbose {
				alog.Printf("@(dim:No need to build) %s\n", pkg.Name)
				printTimes()
			}
			chState(pkg, PackageReady)
			triggerDependentPackages(pkg.ImportName)
			continue workerLoop
		}

		if !inputsModTime.After(pkg.LastBuildInputsModTime) {
			if Opts.Verbose {
				// Sometimes, package updates/builds can be unnecessary triggered repeatedly. For example, sometimes builds themselves
				// can cause files to be touched in depended-upon packages, resulting in a cycle of endless failed builds (successful
				// builds would not be retried already because we would compare the timestamps and determine that the target was up to
				// date).
				alog.Printf("@(dim:Not building) %s@(dim:, as the package and its dependencies have not changed since its last build, which failed.)\n", pkg.Name)
			}
			chState(pkg, PackageDirtyIdle)
			continue workerLoop
		}

		if Opts.Verbose && !pkg.TargetModTime.IsZero() {
			alog.Printf("@(dim:Building) %s@(dim::)\n", pkg.Name)
			printTimes()
		}
		if Opts.Verbose && pkg.CurrentBuildID != "" && pkg.CurrentBuildID != pkg.DesiredBuildID {
			alog.Printf("@(dim:%s) Build ID Desired %s\n", pkg.Name, pkg.DesiredBuildID)
			alog.Printf("@(dim:%s) Build ID Current %s\n", pkg.Name, pkg.CurrentBuildID)
		}
		chState(pkg, PackageBuilding)
		numBuildsActive++
		pkg.LastBuildInputsModTime = inputsModTime
		go pkg.build()
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

func queueUpdate(p *Package, reason string) {
	if enableSanityChecks {
		for _, pkg := range updateQueue {
			if pkg == p {
				alog.Panicf("Package %s already in updateQueue, cannot queue twice", p.Name)
			}
		}
	}
	if beVerbose() {
		alog.Printf("@(dim:Queued update for %s: %s)\n", p.Name, reason)
	}
	chState(p, PackageUpdateQueued)
	updateQueue = append(updateQueue, p)
}

func queueBuild(p *Package, reason string) {
	if p.IsStandard {
		return
	}
	if enableSanityChecks {
		for _, pkg := range buildQueue {
			if pkg == p {
				alog.Panicf("Package %s already in buildQueue, cannot queue twice", p.Name)
			}
		}
	}
	if beVerbose() {
		alog.Printf("@(dim:Queued build for %s: %s)\n", p.Name, reason)
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
					queueBuild(pkg, fmt.Sprintf("triggered deps of %s", importName))
				} else {
					queueUpdate(pkg, fmt.Sprintf("triggered deps of %s", importName))
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
		queueUpdate(p, fmt.Sprintf("forced via diagnoseNotReady from parent %s", parent.Name))
	default:
		alog.Panicf("diagnoseNotReady encountered unexpected state %s", p.State)
	}
}

func diagnoseCircularDependency(p *Package) {
	importNameQuoted := strconv.Quote(p.ImportName)
	absPkgPath := p.getAbsSrcPath()
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
		if importName == "C" {
			continue
		}
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
		if recentDep == nil || depPackage.TargetModTime.After(recentDep.TargetModTime) {
			recentDep = depPackage
		}
	}
	return recentDep, nil
}

// Resolve this dependency, looking for any possible vendored providers.
func resolveImport(p *Package, importName string) *Package {
	rootPath := p.Name + "/"
	for {
		vendorOption := rootPath + "vendor/" + importName
		depPackage, exists := packages[vendorOption]
		if exists {
			return depPackage
		}
		if len(rootPath) == 0 {
			break
		}
		lastIndex := strings.LastIndex(rootPath[:len(rootPath)-1], "/")
		if lastIndex == -1 {
			rootPath = ""
		} else {
			rootPath = rootPath[:lastIndex+1]
		}
	}
	return packages[importName]
}

// This mirrors https://github.com/golang/go/blob/13c35a1b204f6e580b220e0df409a2c186e648a4/src/cmd/go/internal/load/pkg.go#L1026-L1075
func calculateDeps(p *Package) (DepSlice, error) {
	deps := map[string]*Package{}
	var recurse func(pkg *Package) error
	recurse = func(pkg *Package) error {
		if pkg.Imports == nil {
			return nil
		}
		for importName := range pkg.Imports.Raw() {
			if importName == "C" {
				continue
			}
			depPkg := resolveImport(pkg, importName)
			if depPkg == nil {
				if beVerbose() {
					alog.Printf("@(dim:Could not find dependency) %s @(dim:for) %s\n", importName, pkg.Name)
				}
				return dependenciesNotReadyError
			}
			if _, ok := deps[depPkg.Name]; !ok {
				deps[depPkg.Name] = depPkg
				err := recurse(depPkg)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := recurse(p)
	if err != nil {
		return nil, err
	}

	depSlice := make(DepSlice, 0, len(deps))
	for _, pkg := range deps {
		depSlice = append(depSlice, Dep{
			Name:    pkg.Name,
			BuildID: pkg.DesiredBuildID,
		})
	}
	sort.Sort(depSlice)
	return depSlice, nil
}

func printStartupSummary() {
	alog.Printf("@(dim:Finished initial pass of all packages.)\n")
	updateStartupText(true)
}

func beVerbose() bool {
	return finishedInitialPass || Opts.Verbose
}

func shouldRunTests() bool {
	return finishedInitialPass && Opts.RunTests
}

var buildExtensions = stringset.New(".go", ".c", ".cc", ".cxx", ".cpp", ".h", ".hh", ".hpp", ".hxx", ".s", ".swig", ".swigcxx", ".syso")

func processPathTriggers(notifyChan chan watcher.PathEvent) {
	for pathEvent := range notifyChan {
		path := pathEvent.Path
		moduleName, err := filepath.Rel(goPathSrcRoot, filepath.Dir(path))
		if err != nil {
			alog.Bail(err)
		}
		if buildExtensions.Has(filepath.Ext(path)) {
			if Opts.Verbose && finishedInitialPass {
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
	if len(Opts.LDFlags) >= 2 && strings.HasPrefix(Opts.LDFlags, "'") && strings.HasSuffix(Opts.LDFlags, "'") {
		Opts.LDFlags = Opts.LDFlags[1 : len(Opts.LDFlags)-1]
	}

	initStdLibDone := make(chan struct{}, 1)
	go func() {
		filepath.Walk(filepath.Join(build.Default.GOROOT, "src"), initStandardPackages)
		initStdLibDone <- struct{}{}
	}()

	ctx := bismuth2.New()
	ctx.Quote("go-version", "go", "version")

	listener := watcher.NewListener()
	listener.Path = goPathSrcRoot
	// "_workspace" is a kludge to avoid recursing into Godeps workspaces
	// "node_modules" is a kludge to avoid walking into typically-huge node_modules trees
	listener.IgnorePart = stringset.New(".git", ".hg", "node_modules", "_workspace", "etld")
	listener.NotifyOnStartup = true
	listener.DebounceDuration = 200 * time.Millisecond
	listener.Start()

	// Delete any straggler .autoinstall-tmp files
	filepath.Walk(filepath.Join(goPath, "bin"), cleanAutoinstallTmpFiles)
	filepath.Walk(filepath.Join(goPath, "bin"), cleanAutoinstallTmpFiles)

	go dispatcher()

	<-initStdLibDone
	goStdLibPackages.RLock()
	allStdPackages := goStdLibPackages.All()
	goStdLibPackages.RUnlock()
	for _, pkgName := range allStdPackages {
		moduleUpdateChan <- pkgName
	}

	go processPathTriggers(listener.NotifyChan)

	<-sighup
	startupLogger.Close()
}

func cleanAutoinstallTmpFiles(path string, info os.FileInfo, err error) error {
	if err != nil {
		return nil
	}
	if filepath.Base(path) == ".autoinstall-tmp" {
		err := os.Remove(path)
		if err != nil {
			alog.Printf("@(error:Error deleting temp file %s: %v)", path, err)
		}
	}
	return nil
}

func initStandardPackages(path string, info os.FileInfo, err error) error {
	if err != nil {
		return nil
	}
	if info.IsDir() {
		pkgName, err := filepath.Rel(filepath.Join(build.Default.GOROOT, "src"), path)
		if err != nil {
			alog.Printf("Error calculating relative path from %s to %s: %v\n", build.Default.GOROOT, path)
			return err
		}
		if len(pkgName) > 0 {
			goStdLibPackages.Lock()
			goStdLibPackages.Add(pkgName)
			goStdLibPackages.Unlock()
		}
	}
	return nil
}

func isGoStdLibPackage(pkg string) bool {
	goStdLibPackages.RLock()
	defer goStdLibPackages.RUnlock()
	return goStdLibPackages.Has(pkg)
}
